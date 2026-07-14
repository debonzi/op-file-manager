// Package app renders the two-pane opfm terminal interface.
package app

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/debonzi/op-file-manager/internal/config"
	"github.com/debonzi/op-file-manager/internal/domain"
	"github.com/debonzi/op-file-manager/internal/localfs"
	"github.com/debonzi/op-file-manager/internal/opclient"
)

type Focus int

const (
	FocusLocal Focus = iota
	FocusRemote
)

// ContextInfo contains friendly metadata shown in the header. It intentionally
// excludes any Document content and is safe to retain for a TUI session.
type ContextInfo struct {
	AccountName string
	VaultName   string
}

type interactionMode int

const (
	modeNormal interactionMode = iota
	modeFilter
	modeNewFolder
	modeHelp
	modeDetails
	modeConfirmTransfer
	modeConfirmRemoval
)

type Model struct {
	ctx     context.Context
	client  *opclient.Client
	config  config.Config
	root    localfs.Root
	context ContextInfo

	width  int
	height int
	focus  Focus

	localDir          string
	remoteDir         []string
	localCursor       int
	remoteCursor      int
	localFilter       string
	remoteFilter      string
	localExpanded     map[string]bool
	remoteExpanded    map[string]bool
	pendingCreatedDir []string

	tree           domain.Tree
	remoteLoading  bool
	authenticating bool
	status         string

	mode    interactionMode
	input   string
	details *detail
	pending *transfer
	removal *removal
}

type transferDirection int

const (
	upload transferDirection = iota
	download
)

type transfer struct {
	direction  transferDirection
	localFile  string
	remotePath string
	document   *domain.Document
	overwrite  bool
}

type removalKind int

const (
	archiveDocument removalKind = iota
	deleteDirectoryMarker
)

type removal struct {
	kind     removalKind
	document *domain.Document
	path     string
}

type detail struct {
	title string
	lines []string
}

// treeRow is a visible, metadata-only entry in one browser tree. It retains
// the canonical path independently from its rendered name, so selection and
// destructive actions cannot be confused by indentation or icon glyphs.
type treeRow struct {
	key           string
	parentKey     string
	name          string
	localPath     string
	remotePath    []string
	local         *localfs.Entry
	remote        *domain.Entry
	isDir         bool
	isSymlink     bool
	invalid       bool
	last          bool
	ancestorLasts []bool
}

type documentsLoadedMsg struct {
	tree domain.Tree
	err  error
}

type transferFinishedMsg struct{ err error }
type signInFinishedMsg struct{ err error }
type directoryFinishedMsg struct {
	dir     []string
	created bool
	err     error
}
type removalFinishedMsg struct{ err error }

func New(ctx context.Context, client *opclient.Client, cfg config.Config, root localfs.Root, info ContextInfo) *Model {
	return &Model{
		ctx:            ctx,
		client:         client,
		config:         cfg,
		root:           root,
		context:        info,
		localDir:       ".",
		localExpanded:  map[string]bool{},
		remoteExpanded: map[string]bool{},
		tree:           domain.BuildTree(nil),
		remoteLoading:  true,
		status:         "Loading document metadata…",
	}
}

func (m *Model) Init() tea.Cmd { return m.loadDocuments() }

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case documentsLoadedMsg:
		m.remoteLoading = false
		if msg.err != nil {
			m.pendingCreatedDir = nil
			m.status = safeError(msg.err)
			return m, nil
		}
		selectedKey := ""
		if row, ok := m.selectedRemoteRow(); ok {
			selectedKey = row.key
		}
		m.tree = msg.tree
		m.ensureRemoteDestination()
		m.selectRemoteRow(selectedKey)
		m.clampCursors()
		if len(m.pendingCreatedDir) > 0 {
			created := append([]string(nil), m.pendingCreatedDir...)
			m.pendingCreatedDir = nil
			if m.selectCreatedRemoteFolder(created) {
				m.status = "Remote folder created; selected /" + strings.Join(created, "/")
				return m, nil
			}
			m.status = "Remote folder created"
			return m, nil
		}
		m.status = "Document metadata refreshed"
		return m, nil
	case transferFinishedMsg:
		if msg.err != nil {
			m.status = safeError(msg.err)
			return m, nil
		}
		m.remoteLoading = true
		m.status = "Transfer complete; refreshing document metadata…"
		return m, m.loadDocuments()
	case directoryFinishedMsg:
		if msg.err != nil {
			if msg.created {
				m.pendingCreatedDir = nil
			}
			m.status = safeError(msg.err)
			return m, nil
		}
		if msg.created {
			m.pendingCreatedDir = append([]string(nil), msg.dir...)
			m.status = "Remote folder created; refreshing document metadata…"
		} else {
			m.handleRemovedRemoteDirectory(msg.dir)
			m.status = "Remote folder removed; refreshing document metadata…"
		}
		m.remoteLoading = true
		return m, m.loadDocuments()
	case removalFinishedMsg:
		if msg.err != nil {
			m.status = safeError(msg.err)
			return m, nil
		}
		m.remoteLoading = true
		m.status = "Remote item archived; refreshing document metadata…"
		return m, m.loadDocuments()
	case signInFinishedMsg:
		m.authenticating = false
		if msg.err != nil {
			m.status = safeError(msg.err)
			return m, nil
		}
		m.remoteLoading = true
		m.status = "Signed in; refreshing document metadata…"
		return m, m.loadDocuments()
	case tea.KeyPressMsg:
		return m.handleKey(msg.String(), msg.Key().Text)
	}
	return m, nil
}

func (m *Model) handleKey(key, text string) (tea.Model, tea.Cmd) {
	if key == "ctrl+c" {
		return m, tea.Quit
	}

	switch m.mode {
	case modeFilter:
		return m.handleFilterKey(key, text)
	case modeNewFolder:
		return m.handleFolderKey(key, text)
	case modeHelp:
		if key == "esc" || key == "?" || key == "enter" {
			m.mode = modeNormal
			m.status = "Help closed"
		}
		return m, nil
	case modeDetails:
		if key == "esc" || key == "d" || key == "enter" {
			m.mode = modeNormal
			m.details = nil
			m.status = "Details closed"
		}
		return m, nil
	case modeConfirmTransfer:
		return m.handleTransferConfirmation(key)
	case modeConfirmRemoval:
		return m.handleRemovalConfirmation(key)
	}

	switch key {
	case "q":
		return m, tea.Quit
	case "tab":
		if m.focus == FocusLocal {
			m.focus = FocusRemote
		} else {
			m.focus = FocusLocal
		}
		m.status = "Focused " + m.focusName() + " pane"
		return m, nil
	case "r":
		m.remoteLoading = true
		m.status = "Refreshing document metadata…"
		return m, m.loadDocuments()
	case "s":
		if m.authenticating {
			return m, nil
		}
		m.authenticating = true
		m.status = "Sign in through the 1Password CLI…"
		return m, tea.Exec(m.client.SignInCommand(m.ctx, m.config.AccountID), func(err error) tea.Msg {
			return signInFinishedMsg{err: err}
		})
	case "?":
		m.mode = modeHelp
		return m, nil
	case "/":
		m.mode = modeFilter
		m.input = m.currentFilter()
		m.status = "Filtering " + m.focusName() + " entries"
		return m, nil
	case "up", "k":
		m.moveCursor(-1)
		return m, nil
	case "down", "j":
		m.moveCursor(1)
		return m, nil
	case "backspace":
		m.activateParentDirectory()
		return m, nil
	case "left", "h":
		m.closeOrSelectParent()
		return m, nil
	case "enter", "right", "l":
		m.openSelected()
		return m, nil
	case "n":
		m.beginCreateDirectory()
		return m, nil
	case "d":
		m.showDetails()
		return m, nil
	case "ctrl+d":
		return m.prepareRemoval()
	case "f5":
		return m.prepareTransfer()
	}
	return m, nil
}

func (m *Model) handleFilterKey(key, text string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.setCurrentFilter("")
		m.input = ""
		m.mode = modeNormal
		m.status = "Filter cleared"
		return m, nil
	case "enter":
		m.mode = modeNormal
		if m.input == "" {
			m.status = "Filter cleared"
		} else {
			m.status = "Filter active: " + displayName(m.input)
		}
		return m, nil
	case "backspace":
		m.input = removeLastRune(m.input)
		m.setCurrentFilter(m.input)
		return m, nil
	}
	if text != "" {
		m.input += text
		m.setCurrentFilter(m.input)
	}
	return m, nil
}

func (m *Model) handleFolderKey(key, text string) (tea.Model, tea.Cmd) {
	switch key {
	case "esc":
		m.mode = modeNormal
		m.input = ""
		m.status = "Folder creation cancelled"
		return m, nil
	case "backspace":
		m.input = removeLastRune(m.input)
		return m, nil
	case "enter":
		remotePath, err := domain.JoinRemotePath(m.remoteDir, m.input)
		if err != nil {
			m.status = safeError(err)
			return m, nil
		}
		if _, err := domain.DirectoryMarkerTitle(remotePath); err != nil {
			m.status = safeError(err)
			return m, nil
		}
		parts, _ := domain.ValidateRemotePath(remotePath)
		if m.tree.DirectoryMarker(parts) != nil {
			m.status = "This remote folder is already persistent"
			return m, nil
		}
		m.mode = modeNormal
		m.input = ""
		m.status = "Creating remote folder…"
		return m, m.createDirectory(parts, remotePath)
	}
	if text != "" {
		m.input += text
	}
	return m, nil
}

func (m *Model) handleTransferConfirmation(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "y", "Y", "shift+y", "enter":
		pending := m.pending
		m.pending = nil
		m.mode = modeNormal
		m.status = "Transferring…"
		return m, m.execute(pending)
	case "n", "N", "shift+n", "esc":
		m.pending = nil
		m.mode = modeNormal
		m.status = "Transfer cancelled"
	}
	return m, nil
}

func (m *Model) handleRemovalConfirmation(key string) (tea.Model, tea.Cmd) {
	switch key {
	case "y", "Y", "shift+y", "enter":
		removal := m.removal
		m.removal = nil
		m.mode = modeNormal
		if removal.kind == archiveDocument {
			m.status = "Archiving remote document…"
		} else {
			m.status = "Removing empty remote folder…"
		}
		return m, m.executeRemoval(removal)
	case "n", "N", "shift+n", "esc":
		m.removal = nil
		m.mode = modeNormal
		m.status = "Removal cancelled"
	}
	return m, nil
}

func (m *Model) beginCreateDirectory() {
	if m.focus != FocusRemote {
		m.status = "Focus the remote pane to create a folder"
		return
	}
	if m.remoteLoading {
		m.status = "Wait for document metadata to finish loading"
		return
	}
	m.remoteFilter = ""
	m.mode = modeNewFolder
	m.input = ""
	m.status = "Type one folder name and press Enter"
}

func (m *Model) loadDocuments() tea.Cmd {
	return func() tea.Msg {
		documents, err := m.client.ListDocuments(m.ctx, m.config.VaultID)
		if err != nil {
			return documentsLoadedMsg{err: err}
		}
		return documentsLoadedMsg{tree: domain.BuildTree(documents)}
	}
}

func localTreeKey(path string) string {
	path = filepath.Clean(path)
	if path == "." {
		return ""
	}
	return filepath.ToSlash(path)
}

func remoteTreeKey(path []string) string { return strings.Join(path, "/") }

func (m *Model) localRows() ([]treeRow, error) {
	includeCollapsedDescendants := m.localFilter != ""
	rows := make([]treeRow, 0)
	var walk func(string, string, bool) error
	walk = func(dir, parentKey string, nested bool) error {
		entries, err := m.root.List(dir)
		if err != nil {
			if !nested {
				return err
			}
			rows = append(rows, treeRow{
				key:       parentKey + "\x00unreadable",
				parentKey: parentKey,
				name:      "(cannot read directory)",
				invalid:   true,
			})
			return nil
		}
		for index := range entries {
			entry := &entries[index]
			path := filepath.Join(dir, entry.Name)
			key := localTreeKey(path)
			rows = append(rows, treeRow{
				key:       key,
				parentKey: parentKey,
				name:      entry.Name,
				localPath: path,
				local:     entry,
				isDir:     entry.IsDir,
				isSymlink: entry.IsSymlink,
			})
			if entry.IsDir && !entry.IsSymlink && (includeCollapsedDescendants || m.localExpanded[key]) {
				if err := walk(path, key, true); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(".", "", false); err != nil {
		return nil, err
	}
	return filterAndShapeTreeRows(rows, m.localFilter), nil
}

func (m *Model) remoteRows() []treeRow {
	includeCollapsedDescendants := m.remoteFilter != ""
	rows := make([]treeRow, 0)
	invalidIndex := 0
	var walk func([]string, string)
	walk = func(dir []string, parentKey string) {
		entries := m.tree.Entries(dir)
		for index := range entries {
			entry := &entries[index]
			path := append(append([]string(nil), dir...), entry.Name)
			key := remoteTreeKey(path)
			if entry.Invalid {
				key = fmt.Sprintf("\x00invalid:%d", invalidIndex)
				invalidIndex++
				path = nil
			}
			rows = append(rows, treeRow{
				key:        key,
				parentKey:  parentKey,
				name:       entry.Name,
				remotePath: path,
				remote:     entry,
				isDir:      entry.IsDir,
				invalid:    entry.Invalid,
			})
			if entry.IsDir && (includeCollapsedDescendants || m.remoteExpanded[key]) {
				walk(path, key)
			}
		}
	}
	walk(nil, "")
	return filterAndShapeTreeRows(rows, m.remoteFilter)
}

// filterAndShapeTreeRows preserves every matching node and its ancestors. A
// filter temporarily reveals descendants without mutating the expanded maps.
func filterAndShapeTreeRows(rows []treeRow, filter string) []treeRow {
	if filter != "" {
		byKey := make(map[string]treeRow, len(rows))
		include := make(map[string]bool, len(rows))
		for _, row := range rows {
			byKey[row.key] = row
			if matchesFilter(row.name, filter) {
				for key := row.key; key != ""; {
					include[key] = true
					key = byKey[key].parentKey
				}
			}
		}
		filtered := make([]treeRow, 0, len(rows))
		for _, row := range rows {
			if include[row.key] {
				filtered = append(filtered, row)
			}
		}
		rows = filtered
	}
	return shapeTreeRows(rows)
}

func shapeTreeRows(rows []treeRow) []treeRow {
	byKey := make(map[string]treeRow, len(rows))
	lastChild := make(map[string]int, len(rows))
	for index, row := range rows {
		byKey[row.key] = row
		lastChild[row.parentKey] = index
	}
	for index := range rows {
		rows[index].last = lastChild[rows[index].parentKey] == index
		ancestors := make([]bool, 0)
		for parentKey := rows[index].parentKey; parentKey != ""; {
			parent, ok := byKey[parentKey]
			if !ok {
				break
			}
			ancestors = append([]bool{parent.last}, ancestors...)
			parentKey = parent.parentKey
		}
		rows[index].ancestorLasts = ancestors
	}
	return rows
}

func matchesFilter(name, filter string) bool {
	return strings.Contains(strings.ToLower(name), strings.ToLower(filter))
}

func (m *Model) moveCursor(delta int) {
	if m.focus == FocusLocal {
		rows, err := m.localRows()
		if err != nil || len(rows) == 0 {
			return
		}
		m.localCursor = clamp(m.localCursor+delta, 0, len(rows)-1)
		return
	}
	rows := m.remoteRows()
	if len(rows) > 0 {
		m.remoteCursor = clamp(m.remoteCursor+delta, 0, len(rows)-1)
	}
}

func (m *Model) clampCursors() {
	if rows, err := m.localRows(); err == nil {
		m.localCursor = clampCursor(m.localCursor, len(rows))
	}
	m.remoteCursor = clampCursor(m.remoteCursor, len(m.remoteRows()))
}

func clampCursor(cursor, entries int) int {
	if entries == 0 {
		return 0
	}
	return clamp(cursor, 0, entries-1)
}

func (m *Model) openSelected() {
	if m.focus == FocusLocal {
		row, ok, err := m.selectedLocalRow()
		if err != nil || !ok {
			return
		}
		if row.isDir && !row.isSymlink {
			if m.localExpanded[row.key] {
				delete(m.localExpanded, row.key)
				m.selectLocalRow(row.key)
				m.status = "Local folder closed"
			} else {
				m.localExpanded[row.key] = true
				m.localDir = row.localPath
				m.status = "Local folder opened; it is the download destination"
			}
			return
		}
		m.status = "Selected entry is a file; press F5 to copy it"
		return
	}
	row, ok := m.selectedRemoteRow()
	if !ok {
		return
	}
	if row.isDir {
		if m.remoteExpanded[row.key] {
			delete(m.remoteExpanded, row.key)
			m.selectRemoteRow(row.key)
			m.status = "Remote folder closed; it remains the upload destination"
		} else {
			m.remoteExpanded[row.key] = true
			m.remoteDir = append([]string(nil), row.remotePath...)
			m.status = "Remote folder opened; it is the upload destination"
		}
		return
	}
	m.status = "No document preview; press F5 to copy the selected file"
}

func (m *Model) closeOrSelectParent() {
	if m.focus == FocusLocal {
		row, ok, err := m.selectedLocalRow()
		if err != nil || !ok {
			return
		}
		if row.isDir && !row.isSymlink && m.localExpanded[row.key] {
			delete(m.localExpanded, row.key)
			m.selectLocalRow(row.key)
			m.status = "Local folder closed"
			return
		}
		m.selectLocalRow(row.parentKey)
		return
	}
	row, ok := m.selectedRemoteRow()
	if !ok {
		return
	}
	if row.isDir && m.remoteExpanded[row.key] {
		delete(m.remoteExpanded, row.key)
		m.selectRemoteRow(row.key)
		m.status = "Remote folder closed"
		return
	}
	m.selectRemoteRow(row.parentKey)
}

func (m *Model) activateParentDirectory() {
	if m.focus == FocusLocal {
		if m.localDir == "." {
			return
		}
		m.localDir = filepath.Dir(m.localDir)
		if m.localDir == "" {
			m.localDir = "."
		}
		m.selectLocalRow(localTreeKey(m.localDir))
		m.status = "Local download destination moved to parent"
		return
	}
	if len(m.remoteDir) == 0 {
		return
	}
	m.remoteDir = m.remoteDir[:len(m.remoteDir)-1]
	m.selectRemoteRow(remoteTreeKey(m.remoteDir))
	m.status = "Remote upload destination moved to parent"
}

func (m *Model) selectedLocalRow() (treeRow, bool, error) {
	rows, err := m.localRows()
	if err != nil || len(rows) == 0 {
		return treeRow{}, false, err
	}
	row := rows[clamp(m.localCursor, 0, len(rows)-1)]
	return row, row.local != nil, nil
}

func (m *Model) selectedRemoteRow() (treeRow, bool) {
	rows := m.remoteRows()
	if len(rows) == 0 {
		return treeRow{}, false
	}
	row := rows[clamp(m.remoteCursor, 0, len(rows)-1)]
	return row, row.remote != nil
}

func (m *Model) selectedLocal() (localfs.Entry, bool, error) {
	row, ok, err := m.selectedLocalRow()
	if err != nil || !ok {
		return localfs.Entry{}, false, err
	}
	return *row.local, true, nil
}

func (m *Model) selectedRemote() (domain.Entry, bool) {
	row, ok := m.selectedRemoteRow()
	if !ok {
		return domain.Entry{}, false
	}
	return *row.remote, true
}

func (m *Model) selectLocalRow(key string) {
	if key == "" {
		m.localCursor = 0
		return
	}
	rows, err := m.localRows()
	if err != nil {
		return
	}
	for index, row := range rows {
		if row.key == key {
			m.localCursor = index
			return
		}
	}
}

func (m *Model) selectRemoteRow(key string) {
	if key == "" {
		m.remoteCursor = 0
		return
	}
	for index, row := range m.remoteRows() {
		if row.key == key {
			m.remoteCursor = index
			return
		}
	}
}

func (m *Model) prepareTransfer() (tea.Model, tea.Cmd) {
	if m.remoteLoading {
		m.status = "Wait for document metadata to finish loading"
		return m, nil
	}
	if m.focus == FocusLocal {
		return m.prepareUpload()
	}
	return m.prepareDownload()
}

func (m *Model) prepareUpload() (tea.Model, tea.Cmd) {
	entry, ok, err := m.selectedLocal()
	if err != nil || !ok {
		m.status = "Select a local regular file to upload"
		return m, nil
	}
	if err := m.root.CheckSourceFile(entry.Path); err != nil {
		m.status = safeError(err)
		return m, nil
	}
	remotePath, err := domain.JoinRemotePath(m.remoteDir, entry.Name)
	if err != nil {
		m.status = safeError(err)
		return m, nil
	}
	pending := &transfer{direction: upload, localFile: entry.Path, remotePath: remotePath}
	if existing := m.tree.FindDocument(remotePath); existing != nil {
		pending.document = existing
		pending.overwrite = true
		m.pending = pending
		m.mode = modeConfirmTransfer
		m.status = "Confirm overwrite"
		return m, nil
	}
	m.status = fmt.Sprintf("Uploading %s → /%s…", displayName(entry.Name), displayName(remotePath))
	return m, m.execute(pending)
}

func (m *Model) prepareDownload() (tea.Model, tea.Cmd) {
	entry, ok := m.selectedRemote()
	if !ok {
		m.status = "Select a remote document to download"
		return m, nil
	}
	if entry.IsDir {
		m.status = "Open a folder or select a document"
		return m, nil
	}
	if entry.Invalid || entry.Document == nil {
		m.status = "This document has an invalid or duplicate virtual path and is read-only"
		return m, nil
	}
	destination, err := m.root.Resolve(filepath.Join(m.localDir, domain.LeafName(entry.Document.Title)))
	if err != nil {
		m.status = safeError(err)
		return m, nil
	}
	exists, err := m.root.CheckDestination(destination)
	if err != nil {
		m.status = safeError(err)
		return m, nil
	}
	pending := &transfer{direction: download, localFile: destination, remotePath: entry.Document.Title, document: entry.Document, overwrite: exists}
	if exists {
		m.pending = pending
		m.mode = modeConfirmTransfer
		m.status = "Confirm overwrite"
		return m, nil
	}
	m.status = "Downloading…"
	return m, m.execute(pending)
}

func (m *Model) execute(transfer *transfer) tea.Cmd {
	return func() tea.Msg {
		var err error
		if transfer.direction == upload {
			if transfer.document == nil {
				err = m.client.CreateDocument(m.ctx, m.config.VaultID, transfer.localFile, transfer.remotePath)
			} else {
				err = m.client.EditDocument(m.ctx, m.config.VaultID, transfer.document.ID, transfer.localFile, transfer.remotePath)
			}
		} else {
			err = m.client.DownloadDocument(m.ctx, m.config.VaultID, transfer.document.ID, transfer.localFile, transfer.overwrite)
		}
		return transferFinishedMsg{err: err}
	}
}

func (m *Model) createDirectory(parts []string, remotePath string) tea.Cmd {
	return func() tea.Msg {
		err := m.client.CreateDirectoryMarker(m.ctx, m.config.VaultID, remotePath)
		return directoryFinishedMsg{dir: parts, created: true, err: err}
	}
}

func (m *Model) prepareRemoval() (tea.Model, tea.Cmd) {
	if m.focus != FocusRemote {
		m.status = "Remote items can be archived only from the remote pane"
		return m, nil
	}
	if m.remoteLoading {
		m.status = "Wait for document metadata to finish loading"
		return m, nil
	}
	row, ok := m.selectedRemoteRow()
	if !ok {
		m.status = "Select a remote item to archive or remove"
		return m, nil
	}
	entry := row.remote
	if !row.isDir {
		if row.invalid || entry.Document == nil {
			m.status = "This document has an invalid or duplicate virtual path and is read-only"
			return m, nil
		}
		m.removal = &removal{kind: archiveDocument, document: entry.Document, path: entry.Document.Title}
		m.mode = modeConfirmRemoval
		m.status = "Confirm document archive"
		return m, nil
	}

	dir := row.remotePath
	marker := m.tree.EmptyDirectoryMarker(dir)
	if marker == nil {
		if m.tree.DirectoryMarker(dir) != nil {
			m.status = "Remote folder must be empty before it can be removed"
		} else {
			m.status = "Only folders created by opfm can be removed"
		}
		return m, nil
	}
	m.removal = &removal{kind: deleteDirectoryMarker, document: marker, path: marker.Title}
	m.mode = modeConfirmRemoval
	m.status = "Confirm permanent folder removal"
	return m, nil
}

func (m *Model) executeRemoval(removal *removal) tea.Cmd {
	return func() tea.Msg {
		if removal.kind == archiveDocument {
			return removalFinishedMsg{err: m.client.ArchiveDocument(m.ctx, m.config.VaultID, removal.document.ID)}
		}
		parts, _ := domain.ValidateRemotePath(strings.TrimSuffix(removal.path, "/"))
		return directoryFinishedMsg{dir: parts, created: false, err: m.client.DeleteDirectoryMarker(m.ctx, m.config.VaultID, removal.document.ID)}
	}
}

func (m *Model) showDetails() {
	if m.focus == FocusLocal {
		row, ok, err := m.selectedLocalRow()
		if err != nil || !ok {
			m.status = "Select a local item to view details"
			return
		}
		entry := row.local
		kind := "regular file"
		if entry.IsDir {
			kind = "directory"
		}
		if entry.IsSymlink {
			kind = "symbolic link (copy refused)"
		}
		m.details = &detail{title: "Local details", lines: []string{
			"Name: " + displayName(entry.Name),
			"Path: " + displayName(filepath.ToSlash(row.localPath)),
			"Type: " + kind,
			"Size: " + formatSize(entry.Size),
			"Modified: " + formatLocalTime(entry.Modified),
			"Mode: " + entry.Mode.String(),
		}}
		m.mode = modeDetails
		return
	}

	row, ok := m.selectedRemoteRow()
	if !ok {
		m.status = "Select a remote item to view details"
		return
	}
	entry := row.remote
	if row.isDir {
		dir := row.remotePath
		persistence := "inferred from Documents"
		if marker := m.tree.DirectoryMarker(dir); marker != nil {
			persistence = "persistent opfm directory marker"
		}
		m.details = &detail{title: "Remote folder details", lines: []string{
			"Name: " + displayName(entry.Name),
			"Path: /" + displayName(strings.Join(dir, "/")),
			"Type: " + persistence,
		}}
		m.mode = modeDetails
		return
	}

	if entry.Document == nil {
		m.status = "Select a remote item to view details"
		return
	}
	state := "ready"
	if entry.Invalid {
		state = "read-only: invalid or duplicate virtual path"
	}
	doc := entry.Document
	m.details = &detail{title: "Remote document details", lines: []string{
		"Name: " + displayName(entry.Name),
		"Virtual path: /" + displayName(doc.Title),
		"Size: " + formatSize(doc.Size),
		"Updated: " + formatRemoteTime(doc.UpdatedAt),
		"Created: " + formatRemoteTime(doc.CreatedAt),
		"Item ID: " + displayName(doc.ID),
		"State: " + state,
	}}
	m.mode = modeDetails
}

func (m *Model) currentFilter() string {
	if m.focus == FocusLocal {
		return m.localFilter
	}
	return m.remoteFilter
}

func (m *Model) setCurrentFilter(value string) {
	if m.focus == FocusLocal {
		m.localFilter = value
		m.localCursor = 0
		return
	}
	m.remoteFilter = value
	m.remoteCursor = 0
}

func (m *Model) focusName() string {
	if m.focus == FocusLocal {
		return "local"
	}
	return "remote"
}

const (
	minimumTerminalWidth  = 80
	minimumTerminalHeight = 20
	logoMinimumWidth      = 100
)

const opfmLogo = `   ____  ____  ________  ___
  / __ \/ __ \/ ____/  |/  /
 / / / / /_/ / /_  / /|_/ /
/ /_/ / ____/ __/ / /  / /
\____/_/   /_/   /_/  /_/`

func (m *Model) View() tea.View {
	if m.width > 0 && (m.width < minimumTerminalWidth || m.height < minimumTerminalHeight) {
		view := tea.NewView("Terminal too small: opfm requires at least 80×20.\n")
		view.AltScreen = true
		return view
	}

	width := m.width
	if width == 0 {
		width = 120
	}
	height := m.height
	if height == 0 {
		height = 30
	}
	header := m.renderHeader(width)
	actions := m.renderActions(width)
	footer := m.renderFooter(width)
	bodyHeight := max(3, height-lipgloss.Height(header)-lipgloss.Height(actions)-lipgloss.Height(footer)-3)
	modal := m.renderModal()
	body := ""
	if modal != "" {
		body = lipgloss.Place(width, bodyHeight, lipgloss.Center, lipgloss.Center, modal)
	} else {
		body = m.renderPanels(width, bodyHeight)
	}
	content := strings.Join([]string{header, actions, body, footer}, "\n")

	view := tea.NewView(content)
	view.AltScreen = true
	return view
}

func (m *Model) renderHeader(width int) string {
	styles := m.styles()
	account := m.context.AccountName
	if account == "" {
		account = m.config.AccountID
	}
	vault := m.context.VaultName
	if vault == "" {
		vault = m.config.VaultID
	}
	session := "CONNECTED"
	if m.authenticating {
		session = "AUTHENTICATING"
	}
	localPath := displayName(m.localDir)
	remotePath := m.remotePath()

	leftWidth := width
	showLogo := width >= logoMinimumWidth
	logoLines := strings.Split(opfmLogo, "\n")
	if showLogo {
		logoWidth := 0
		for _, line := range logoLines {
			logoWidth = max(logoWidth, lipgloss.Width(line))
		}
		leftWidth = max(44, width-logoWidth-3)
	}
	lines := []string{
		m.contextLine(leftWidth, "ACCOUNT", account, styles.contextValue),
		m.contextLine(leftWidth, "VAULT", vault, styles.contextValue),
		m.sessionLine(leftWidth, session),
		m.pathLine(leftWidth, localPath, remotePath),
	}
	if !showLogo {
		return strings.Join(lines, "\n")
	}
	lineCount := max(len(lines), len(logoLines))
	headerLines := make([]string, 0, lineCount)
	for index := 0; index < lineCount; index++ {
		line := ""
		if index < len(lines) {
			line = lines[index]
		}
		line = padDisplay(line, leftWidth)
		logo := ""
		if index < len(logoLines) {
			logo = styles.logo.Render(logoLines[index])
		}
		headerLines = append(headerLines, line+"   "+logo)
	}
	return strings.Join(headerLines, "\n")
}

func (m *Model) contextLine(width int, label, value string, valueStyle lipgloss.Style) string {
	styles := m.styles()
	prefix := styles.contextLabel.Render(label + ": ")
	return prefix + valueStyle.Render(fit(displayName(value), max(1, width-lipgloss.Width(prefix))))
}

func (m *Model) sessionLine(width int, session string) string {
	styles := m.styles()
	return fitStyled(styles.contextLabel.Render("SESSION: ")+styles.session.Render(session), width)
}

func (m *Model) pathLine(width int, localPath, remotePath string) string {
	styles := m.styles()
	localPrefix := styles.contextLabel.Render("LOCAL: ")
	remotePrefix := styles.contextLabel.Render("  REMOTE: ")
	available := max(6, width-lipgloss.Width(localPrefix)-lipgloss.Width(remotePrefix))
	localWidth := max(3, available/2)
	remoteWidth := max(3, available-localWidth)
	return localPrefix + styles.localPath.Render(fit(localPath, localWidth)) + remotePrefix + styles.remotePath.Render(fit(remotePath, remoteWidth))
}

func (m *Model) renderActions(width int) string {
	styles := m.styles()
	shortcut := func(key, label string) string {
		return styles.shortcutKey.Render("<"+key+">") + " " + styles.shortcutLabel.Render(label)
	}
	var actions []string
	switch m.mode {
	case modeFilter:
		return fitStyled(shortcut("/", "FILTER "+strings.ToUpper(m.focusName())+": ")+styles.input.Render(displayName(m.input)+"█")+"  "+shortcut("Enter", "keep")+"  "+shortcut("Esc", "clear"), width)
	case modeNewFolder:
		path := "/" + strings.Join(m.remoteDir, "/") + folderSeparator(m.remoteDir, m.input) + displayName(m.input) + "█"
		return fitStyled(shortcut("n", "NEW FOLDER: ")+styles.input.Render(path)+"  "+shortcut("Enter", "create")+"  "+shortcut("Esc", "cancel"), width)
	default:
		if m.focus == FocusLocal {
			actions = []string{
				shortcut("F5", "Upload"), shortcut("Enter", "Toggle"), shortcut("Backspace", "Parent"), shortcut("/", "Filter"),
				shortcut("d", "Details"), shortcut("r", "Refresh"), shortcut("?", "Help"),
			}
		} else {
			actions = []string{
				shortcut("F5", "Download"), shortcut("n", "Folder"), shortcut("Ctrl+D", "Archive"), shortcut("Enter", "Toggle"),
				shortcut("/", "Filter"), shortcut("d", "Details"), shortcut("r", "Refresh"), shortcut("?", "Help"),
			}
		}
	}
	return joinStyled(width, "  ", actions)
}

func folderSeparator(dir []string, value string) string {
	if len(dir) == 0 || value == "" {
		return ""
	}
	return "/"
}

func (m *Model) renderFooter(width int) string {
	styles := m.styles()
	localCount := 0
	if rows, err := m.localRows(); err == nil {
		localCount = len(rows)
	}
	remoteCount := len(m.remoteRows())
	localTag := styles.footerTag(m.focus == FocusLocal).Render("<local>")
	remoteTag := styles.footerTag(m.focus == FocusRemote).Render("<remote>")
	counts := styles.muted.Render(fmt.Sprintf("L:%d  R:%d", localCount, remoteCount))
	statusWidth := max(8, width-lipgloss.Width(localTag)-lipgloss.Width(remoteTag)-lipgloss.Width(counts)-5)
	status := styles.status.Render(fit(displayName(m.status), statusWidth))
	line := localTag + " " + remoteTag + "  " + status
	line += strings.Repeat(" ", max(1, width-lipgloss.Width(line)-lipgloss.Width(counts))) + counts
	return padDisplay(line, width)
}

func (m *Model) renderPanels(width, height int) string {
	panelWidth := max(36, (width-1)/2)
	local := m.renderLocalPanel(panelWidth, height)
	remote := m.renderRemotePanel(panelWidth, height)
	return padBlock(lipgloss.JoinHorizontal(lipgloss.Top, local, " ", remote), width)
}

func (m *Model) renderLocalPanel(width, height int) string {
	rows, err := m.localRows()
	title := fmt.Sprintf("LOCAL TREE  DEST %s  [%d]", displayName(m.localDir), len(rows))
	if err != nil {
		return m.renderFrame(width, height, title, []string{m.styles().error.Render("Cannot read directory")}, m.focus == FocusLocal)
	}
	return m.renderFrame(width, height, title, m.renderTreeLines(width-2, height-2, rows, m.localCursor, true), m.focus == FocusLocal)
}

func (m *Model) renderRemotePanel(width, height int) string {
	rows := m.remoteRows()
	title := fmt.Sprintf("REMOTE TREE  DEST %s  [%d]", m.remotePath(), len(rows))
	if m.remoteLoading {
		return m.renderFrame(width, height, title, []string{m.styles().muted.Render("Loading document metadata…")}, m.focus == FocusRemote)
	}
	return m.renderFrame(width, height, title, m.renderTreeLines(width-2, height-2, rows, m.remoteCursor, false), m.focus == FocusRemote)
}

func (m *Model) remotePath() string {
	if len(m.remoteDir) == 0 {
		return "/"
	}
	return "/" + strings.Join(m.remoteDir, "/")
}

func (m *Model) ensureRemoteDestination() {
	for len(m.remoteDir) > 0 && m.tree.Entries(m.remoteDir) == nil {
		m.remoteDir = m.remoteDir[:len(m.remoteDir)-1]
	}
}

func (m *Model) handleRemovedRemoteDirectory(dir []string) {
	delete(m.remoteExpanded, remoteTreeKey(dir))
	if sameRemotePath(m.remoteDir, dir) {
		m.remoteDir = m.remoteDir[:len(m.remoteDir)-1]
		m.selectRemoteRow(remoteTreeKey(m.remoteDir))
	}
}

func (m *Model) selectCreatedRemoteFolder(created []string) bool {
	if len(created) == 0 || len(created) != len(m.remoteDir)+1 {
		return false
	}
	for index, part := range m.remoteDir {
		if created[index] != part {
			return false
		}
	}
	if len(m.remoteDir) > 0 {
		m.remoteExpanded[remoteTreeKey(m.remoteDir)] = true
	}
	key := remoteTreeKey(created)
	rows := m.remoteRows()
	for index, row := range rows {
		if row.key == key && row.isDir {
			m.remoteCursor = index
			return true
		}
	}
	return false
}

const (
	closedFolderIcon = "󰉋"
	openFolderIcon   = "󰝰"
	documentIcon     = "󰈙"
	symlinkIcon      = "󰌷"
	invalidIcon      = "󰧺"
	destinationIcon  = "󰜷"
	cursorIcon       = "▌"
)

func (m *Model) renderTreeLines(width, height int, rows []treeRow, cursor int, local bool) []string {
	styles := m.styles()
	contentWidth := max(1, width-1)
	if len(rows) == 0 {
		return padLines([]string{" " + styles.muted.Render("(empty)")}, height)
	}
	cursor = clampCursor(cursor, len(rows))
	visible := max(1, height)
	showPosition := len(rows) > visible
	if showPosition && visible > 1 {
		visible--
	}
	start, end := treeWindow(cursor, len(rows), visible)
	lines := make([]string, 0, height)
	for index := start; index < end; index++ {
		plain, line := m.renderTreeRow(rows[index], local)
		if rows[index].invalid {
			line = styles.error.Render(padDisplay(fit(plain, contentWidth), contentWidth))
		} else {
			line = padDisplay(fitStyled(line, contentWidth), contentWidth)
		}
		gutter := " "
		if index == cursor {
			gutter = styles.cursor.Render(cursorIcon)
		}
		lines = append(lines, gutter+line)
	}
	if showPosition {
		lines = append(lines, " "+styles.muted.Render(fmt.Sprintf("… %d/%d", cursor+1, len(rows))))
	}
	return padLines(lines, height)
}

func (m *Model) renderTreeRow(row treeRow, local bool) (string, string) {
	styles := m.styles()
	var prefix strings.Builder
	for _, last := range row.ancestorLasts {
		if last {
			prefix.WriteString("  ")
		} else {
			prefix.WriteString("│ ")
		}
	}
	if row.last {
		prefix.WriteString("└─ ")
	} else {
		prefix.WriteString("├─ ")
	}

	icon, iconStyle, nameStyle := documentIcon, styles.file, styles.file
	if row.invalid {
		icon, iconStyle, nameStyle = invalidIcon, styles.error, styles.error
	} else if row.isSymlink {
		icon, iconStyle, nameStyle = symlinkIcon, styles.symlink, styles.symlink
	} else if row.isDir {
		expanded := m.remoteExpanded[row.key]
		if local {
			expanded = m.localExpanded[row.key]
		}
		if expanded {
			icon = openFolderIcon
		} else {
			icon = closedFolderIcon
		}
		iconStyle, nameStyle = styles.directory, styles.directory
	}

	marker := ""
	if !local && row.isDir && sameRemotePath(row.remotePath, m.remoteDir) {
		marker = "  " + destinationIcon + " DEST"
	}
	plain := prefix.String() + icon + " " + displayName(row.name) + marker
	styled := styles.treeGuide.Render(prefix.String()) + iconStyle.Render(icon+" ") + nameStyle.Render(displayName(row.name))
	if marker != "" {
		styled += styles.destination.Render(marker)
	}
	return plain, styled
}

func sameRemotePath(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func treeWindow(cursor, count, visible int) (int, int) {
	visible = max(1, visible)
	if count <= visible {
		return 0, count
	}
	start := clamp(cursor-visible/2, 0, count-visible)
	return start, start + visible
}

func (m *Model) renderFrame(width, height int, title string, content []string, active bool) string {
	styles := m.styles()
	width = max(6, width)
	height = max(3, height)
	border, titleStyle := styles.frame(active), styles.frameTitle(active)
	title = fit(displayName(title), max(1, width-6))
	title = " " + title + " "
	topFill := max(0, width-3-lipgloss.Width(title))
	lines := []string{
		border.Render("┌─") + titleStyle.Render(title) + border.Render(strings.Repeat("─", topFill)+"┐"),
	}
	for _, line := range padLines(content, height-2) {
		lines = append(lines, border.Render("│")+padDisplay(line, width-2)+border.Render("│"))
	}
	lines = append(lines, border.Render("└"+strings.Repeat("─", width-2)+"┘"))
	return strings.Join(lines, "\n")
}

func padLines(lines []string, height int) []string {
	if len(lines) > height {
		return lines[:height]
	}
	for len(lines) < height {
		lines = append(lines, "")
	}
	return lines
}

func padBlock(value string, width int) string {
	lines := strings.Split(value, "\n")
	for index, line := range lines {
		lines[index] = padDisplay(line, width)
	}
	return strings.Join(lines, "\n")
}

func padDisplay(value string, width int) string {
	padding := max(0, width-lipgloss.Width(value))
	return value + strings.Repeat(" ", padding)
}

func fitStyled(value string, width int) string {
	if lipgloss.Width(value) <= width {
		return value
	}
	return fit(ansiEscape.ReplaceAllString(value, ""), width)
}

func joinStyled(width int, separator string, parts []string) string {
	var builder strings.Builder
	for _, part := range parts {
		candidate := part
		if builder.Len() > 0 {
			candidate = separator + candidate
		}
		if lipgloss.Width(builder.String()+candidate) > width {
			break
		}
		builder.WriteString(candidate)
	}
	return builder.String()
}

func (m *Model) renderModal() string {
	styles := m.styles()
	switch m.mode {
	case modeHelp:
		return m.renderHelp()
	case modeDetails:
		if m.details == nil {
			return ""
		}
		return styles.modal.Render(m.details.title + "\n" + strings.Join(m.details.lines, "\n") + "\n\nPress d, Enter, or Esc to close")
	case modeConfirmTransfer:
		if m.pending == nil {
			return ""
		}
		action := "Upload"
		from, to := m.pending.localFile, "/"+m.pending.remotePath
		if m.pending.direction == download {
			action = "Download"
			from, to = "/"+m.pending.remotePath, m.pending.localFile
		}
		text := fmt.Sprintf("Overwrite confirmation\n%s: %s → %s\n\nPress y/Enter to overwrite, n/Esc to cancel", action, displayName(from), displayName(to))
		return styles.modal.Render(text)
	case modeConfirmRemoval:
		if m.removal == nil {
			return ""
		}
		if m.removal.kind == archiveDocument {
			text := fmt.Sprintf("Archive remote document\n/%s\n\nThe Document will move to the 1Password Archive and can be recovered there.\nPress y/Enter to archive, n/Esc to cancel", displayName(m.removal.path))
			return styles.dangerModal.Render(text)
		}
		text := fmt.Sprintf("Remove empty remote folder\n/%s\n\nThis permanently deletes only the opfm directory marker.\nPress y/Enter to remove, n/Esc to cancel", displayName(strings.TrimSuffix(m.removal.path, "/")))
		return styles.dangerModal.Render(text)
	}
	return ""
}

func (m *Model) renderHelp() string {
	styles := m.styles()
	type binding struct {
		key   string
		label string
	}
	rows := [][2]binding{
		{{"Tab", "Switch pane"}, {"↑/↓, j/k", "Move selection"}},
		{{"Enter, →, l", "Toggle folder"}, {"←, h", "Close / select parent"}},
		{{"Backspace", "Destination parent"}, {"/", "Filter current tree"}},
		{{"d", "Safe details"}, {"F5", "Copy selected file"}},
		{{"n", "New child folder (remote)"}, {"Ctrl+D", "Archive / delete marker"}},
		{{"r", "Refresh remote"}, {"s", "Sign in"}},
		{{"?", "Close help"}, {"q, Ctrl+C", "Quit"}},
	}
	const columnWidth = 34
	bindingText := func(item binding) string {
		return styles.shortcutKey.Render("<"+item.key+">") + " " + styles.shortcutLabel.Render(item.label)
	}
	lines := []string{styles.titleActive.Render("Keyboard shortcuts"), ""}
	for _, row := range rows {
		left := padDisplay(bindingText(row[0]), columnWidth)
		lines = append(lines, left+"  "+bindingText(row[1]))
	}
	return styles.modal.Render(strings.Join(lines, "\n"))
}

func (m *Model) styles() uiStyles {
	return uiStyles{
		contextLabel:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")),
		contextValue:    lipgloss.NewStyle().Foreground(lipgloss.Color("255")),
		session:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46")),
		localPath:       lipgloss.NewStyle().Foreground(lipgloss.Color("81")),
		remotePath:      lipgloss.NewStyle().Foreground(lipgloss.Color("13")),
		logo:            lipgloss.NewStyle().Foreground(lipgloss.Color("#8fb6ff")),
		shortcutKey:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")),
		shortcutLabel:   lipgloss.NewStyle().Foreground(lipgloss.Color("255")),
		input:           lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51")),
		frameActive:     lipgloss.NewStyle().Foreground(lipgloss.Color("#8fb6ff")),
		frameIdle:       lipgloss.NewStyle().Foreground(lipgloss.Color("#3e4958")),
		titleActive:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#a5c4ff")),
		titleIdle:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#7b879d")),
		treeGuide:       lipgloss.NewStyle().Foreground(lipgloss.Color("#3e4958")),
		directory:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#8fb6ff")),
		file:            lipgloss.NewStyle().Foreground(lipgloss.Color("#b9b7cf")),
		symlink:         lipgloss.NewStyle().Foreground(lipgloss.Color("#9ba7bb")),
		destination:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#5dd68b")),
		cursor:          lipgloss.NewStyle().Foreground(lipgloss.Color("#5dd68b")),
		muted:           lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		status:          lipgloss.NewStyle().Foreground(lipgloss.Color("220")),
		footerTagActive: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")).Underline(true),
		footerTagIdle:   lipgloss.NewStyle().Foreground(lipgloss.Color("245")),
		error:           lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("203")),
		modal:           lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("51")).Padding(1, 2),
		dangerModal:     lipgloss.NewStyle().Border(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("203")).Padding(1, 2),
	}
}

type uiStyles struct {
	contextLabel    lipgloss.Style
	contextValue    lipgloss.Style
	session         lipgloss.Style
	localPath       lipgloss.Style
	remotePath      lipgloss.Style
	logo            lipgloss.Style
	shortcutKey     lipgloss.Style
	shortcutLabel   lipgloss.Style
	input           lipgloss.Style
	frameActive     lipgloss.Style
	frameIdle       lipgloss.Style
	titleActive     lipgloss.Style
	titleIdle       lipgloss.Style
	treeGuide       lipgloss.Style
	directory       lipgloss.Style
	file            lipgloss.Style
	symlink         lipgloss.Style
	destination     lipgloss.Style
	cursor          lipgloss.Style
	muted           lipgloss.Style
	status          lipgloss.Style
	footerTagActive lipgloss.Style
	footerTagIdle   lipgloss.Style
	error           lipgloss.Style
	modal           lipgloss.Style
	dangerModal     lipgloss.Style
}

func (s uiStyles) frame(active bool) lipgloss.Style {
	if active {
		return s.frameActive
	}
	return s.frameIdle
}

func (s uiStyles) frameTitle(active bool) lipgloss.Style {
	if active {
		return s.titleActive
	}
	return s.titleIdle
}

func (s uiStyles) footerTag(active bool) lipgloss.Style {
	if active {
		return s.footerTagActive
	}
	return s.footerTagIdle
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func safeError(err error) string {
	if opclient.IsAuthenticationError(err) {
		return "1Password session expired; press s to sign in"
	}
	return displayName(err.Error())
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func displayName(value string) string {
	const maxRunes = 120
	var builder strings.Builder
	count := 0
	for _, character := range value {
		if count == maxRunes {
			builder.WriteRune('…')
			break
		}
		if unicode.IsControl(character) {
			builder.WriteRune('�')
		} else {
			builder.WriteRune(character)
		}
		count++
	}
	return builder.String()
}

func fit(value string, width int) string {
	if width <= 0 {
		return ""
	}
	value = displayName(value)
	if utf8.RuneCountInString(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	runes := []rune(value)
	return string(runes[:width-1]) + "…"
}

func removeLastRune(value string) string {
	runes := []rune(value)
	if len(runes) == 0 {
		return ""
	}
	return string(runes[:len(runes)-1])
}

func formatSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exponent := int64(unit), 0
	for n := size / unit; n >= unit && exponent < 5; n /= unit {
		div *= unit
		exponent++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exponent])
}

func formatLocalTime(value time.Time) string {
	if value.IsZero() {
		return "—"
	}
	return value.Local().Format("2006-01-02")
}

func formatRemoteTime(value string) string {
	if value == "" {
		return "—"
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return fit(value, 12)
	}
	return parsed.Local().Format("2006-01-02")
}
