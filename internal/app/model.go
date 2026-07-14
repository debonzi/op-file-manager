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
	wide              bool
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
		ctx:           ctx,
		client:        client,
		config:        cfg,
		root:          root,
		context:       info,
		localDir:      ".",
		tree:          domain.BuildTree(nil),
		remoteLoading: true,
		status:        "Loading document metadata…",
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
		m.tree = msg.tree
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
	case "w":
		m.wide = !m.wide
		if m.wide {
			m.status = "Wide table mode enabled"
		} else {
			m.status = "Compact table mode enabled"
		}
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
	case "backspace", "left", "h":
		m.upDirectory()
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

func (m *Model) localEntries() ([]localfs.Entry, error) {
	return m.root.List(m.localDir)
}

func (m *Model) filteredLocalEntries() ([]localfs.Entry, error) {
	entries, err := m.localEntries()
	if err != nil {
		return nil, err
	}
	if m.localFilter == "" {
		return entries, nil
	}
	filtered := make([]localfs.Entry, 0, len(entries))
	for _, entry := range entries {
		if matchesFilter(entry.Name, m.localFilter) {
			filtered = append(filtered, entry)
		}
	}
	return filtered, nil
}

func (m *Model) remoteEntries() []domain.Entry { return m.tree.Entries(m.remoteDir) }

func (m *Model) filteredRemoteEntries() []domain.Entry {
	entries := m.remoteEntries()
	if m.remoteFilter == "" {
		return entries
	}
	filtered := make([]domain.Entry, 0, len(entries))
	for _, entry := range entries {
		if matchesFilter(entry.Name, m.remoteFilter) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func matchesFilter(name, filter string) bool {
	return strings.Contains(strings.ToLower(name), strings.ToLower(filter))
}

func (m *Model) moveCursor(delta int) {
	if m.focus == FocusLocal {
		entries, err := m.filteredLocalEntries()
		if err != nil || len(entries) == 0 {
			return
		}
		m.localCursor = clamp(m.localCursor+delta, 0, len(entries)-1)
		return
	}
	entries := m.filteredRemoteEntries()
	if len(entries) > 0 {
		m.remoteCursor = clamp(m.remoteCursor+delta, 0, len(entries)-1)
	}
}

func (m *Model) clampCursors() {
	if entries, err := m.filteredLocalEntries(); err == nil {
		m.localCursor = clampCursor(m.localCursor, len(entries))
	}
	m.remoteCursor = clampCursor(m.remoteCursor, len(m.filteredRemoteEntries()))
}

func clampCursor(cursor, entries int) int {
	if entries == 0 {
		return 0
	}
	return clamp(cursor, 0, entries-1)
}

func (m *Model) openSelected() {
	if m.focus == FocusLocal {
		entry, ok, err := m.selectedLocal()
		if err != nil || !ok {
			return
		}
		if entry.IsDir && !entry.IsSymlink {
			m.localDir = filepath.Join(m.localDir, entry.Name)
			m.localCursor = 0
			m.localFilter = ""
			return
		}
		m.status = "Selected entry is a file; press F5 to copy it"
		return
	}
	entry, ok := m.selectedRemote()
	if !ok {
		return
	}
	if entry.IsDir {
		m.remoteDir = append(m.remoteDir, entry.Name)
		m.remoteCursor = 0
		m.remoteFilter = ""
		return
	}
	m.status = "No document preview; press F5 to copy the selected file"
}

func (m *Model) upDirectory() {
	if m.focus == FocusLocal {
		if m.localDir != "." {
			m.localDir = filepath.Dir(m.localDir)
			if m.localDir == "" {
				m.localDir = "."
			}
			m.localCursor = 0
			m.localFilter = ""
		}
		return
	}
	if len(m.remoteDir) > 0 {
		m.remoteDir = m.remoteDir[:len(m.remoteDir)-1]
		m.remoteCursor = 0
		m.remoteFilter = ""
	}
}

func (m *Model) selectedLocal() (localfs.Entry, bool, error) {
	entries, err := m.filteredLocalEntries()
	if err != nil || len(entries) == 0 {
		return localfs.Entry{}, false, err
	}
	return entries[clamp(m.localCursor, 0, len(entries)-1)], true, nil
}

func (m *Model) selectedRemote() (domain.Entry, bool) {
	entries := m.filteredRemoteEntries()
	if len(entries) == 0 {
		return domain.Entry{}, false
	}
	return entries[clamp(m.remoteCursor, 0, len(entries)-1)], true
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
	entry, ok := m.selectedRemote()
	if !ok {
		m.status = "Select a remote item to archive or remove"
		return m, nil
	}
	if !entry.IsDir {
		if entry.Invalid || entry.Document == nil {
			m.status = "This document has an invalid or duplicate virtual path and is read-only"
			return m, nil
		}
		m.removal = &removal{kind: archiveDocument, document: entry.Document, path: entry.Document.Title}
		m.mode = modeConfirmRemoval
		m.status = "Confirm document archive"
		return m, nil
	}

	dir := append(append([]string(nil), m.remoteDir...), entry.Name)
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
		return directoryFinishedMsg{created: false, err: m.client.DeleteDirectoryMarker(m.ctx, m.config.VaultID, removal.document.ID)}
	}
}

func (m *Model) showDetails() {
	if m.focus == FocusLocal {
		entry, ok, err := m.selectedLocal()
		if err != nil || !ok {
			m.status = "Select a local item to view details"
			return
		}
		kind := "regular file"
		if entry.IsDir {
			kind = "directory"
		}
		if entry.IsSymlink {
			kind = "symbolic link (copy refused)"
		}
		m.details = &detail{title: "Local details", lines: []string{
			"Name: " + displayName(entry.Name),
			"Path: " + displayName(filepath.Join(m.localDir, entry.Name)),
			"Type: " + kind,
			"Size: " + formatSize(entry.Size),
			"Modified: " + formatLocalTime(entry.Modified),
			"Mode: " + entry.Mode.String(),
		}}
		m.mode = modeDetails
		return
	}

	entry, ok := m.selectedRemote()
	if !ok {
		m.status = "Select a remote item to view details"
		return
	}
	if entry.IsDir {
		dir := append(append([]string(nil), m.remoteDir...), entry.Name)
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

const opfmLogo = `     .----------.
    /   OPFM   \
   |   VAULT   |
    '----o----'`

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
	for index := range lines {
		lines[index] = padDisplay(lines[index], leftWidth)
		logo := ""
		if index < len(logoLines) {
			logo = styles.logo.Render(logoLines[index])
		}
		lines[index] += "   " + logo
	}
	return strings.Join(lines, "\n")
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
				shortcut("F5", "Upload"), shortcut("Enter", "Open"), shortcut("Backspace", "Up"), shortcut("/", "Filter"),
				shortcut("d", "Details"), shortcut("r", "Refresh"), shortcut("?", "Help"),
			}
		} else {
			actions = []string{
				shortcut("F5", "Download"), shortcut("n", "Folder"), shortcut("Ctrl+D", "Archive"), shortcut("Enter", "Open"),
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
	if entries, err := m.filteredLocalEntries(); err == nil {
		localCount = len(entries)
	}
	remoteCount := len(m.filteredRemoteEntries())
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
	entries, err := m.filteredLocalEntries()
	title := fmt.Sprintf("LOCAL %s [%d]", displayName(m.localDir), len(entries))
	if err != nil {
		return m.renderFrame(width, height, title, []string{m.styles().error.Render("Cannot read directory")}, m.focus == FocusLocal)
	}
	rows := make([]tableRow, 0, len(entries))
	for _, entry := range entries {
		kind := "FILE"
		if entry.IsDir {
			kind = "DIR"
		}
		if entry.IsSymlink {
			kind = "LINK"
		}
		size := formatSize(entry.Size)
		if entry.IsDir {
			size = "—"
		}
		rows = append(rows, tableRow{kind: kind, name: entry.Name, size: size, modified: formatLocalTime(entry.Modified), extra: entry.Mode.String()})
	}
	return m.renderFrame(width, height, title, m.renderTableLines(width-2, height-2, rows, m.localCursor, "MODIFIED", "MODE"), m.focus == FocusLocal)
}

func (m *Model) renderRemotePanel(width, height int) string {
	title := fmt.Sprintf("REMOTE %s [%d]", m.remotePath(), len(m.filteredRemoteEntries()))
	if m.remoteLoading {
		return m.renderFrame(width, height, title, []string{m.styles().muted.Render("Loading document metadata…")}, m.focus == FocusRemote)
	}
	entries := m.filteredRemoteEntries()
	rows := make([]tableRow, 0, len(entries))
	for _, entry := range entries {
		kind := "DOC"
		size, modified, extra := "—", "—", "—"
		if entry.IsDir {
			kind = "DIR"
		} else if entry.Invalid {
			kind = "INVALID"
		}
		if entry.Document != nil {
			size = formatSize(entry.Document.Size)
			modified = formatRemoteTime(entry.Document.UpdatedAt)
			extra = formatRemoteTime(entry.Document.CreatedAt)
		}
		rows = append(rows, tableRow{kind: kind, name: entry.Name, size: size, modified: modified, extra: extra, invalid: entry.Invalid})
	}
	return m.renderFrame(width, height, title, m.renderTableLines(width-2, height-2, rows, m.remoteCursor, "UPDATED", "CREATED"), m.focus == FocusRemote)
}

func (m *Model) remotePath() string {
	if len(m.remoteDir) == 0 {
		return "/"
	}
	return "/" + strings.Join(m.remoteDir, "/")
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
	name := created[len(created)-1]
	for index, entry := range m.filteredRemoteEntries() {
		if entry.IsDir && entry.Name == name {
			m.remoteCursor = index
			return true
		}
	}
	return false
}

type tableRow struct {
	kind     string
	name     string
	size     string
	modified string
	extra    string
	invalid  bool
}

func (m *Model) renderTableLines(width, height int, rows []tableRow, cursor int, timeLabel, extraLabel string) []string {
	styles := m.styles()
	wide := m.wide && width >= 58
	lines := []string{styles.tableHeader.Render(padDisplay(tableHeader(width, wide, timeLabel, extraLabel), width))}
	if height <= 1 {
		return lines
	}
	if len(rows) == 0 {
		lines = append(lines, styles.muted.Render("(empty)"))
		return padLines(lines, height)
	}
	visible := height - 1
	showPosition := len(rows) > visible
	if showPosition && visible > 1 {
		visible--
	}
	start, end := tableWindow(cursor, len(rows), visible)
	for index := start; index < end; index++ {
		line := padDisplay(renderTableRow(width, wide, rows[index]), width)
		if index == cursor {
			line = styles.selected.Render(line)
		} else if rows[index].invalid {
			line = styles.error.Render(line)
		} else {
			line = styles.tableData.Render(line)
		}
		lines = append(lines, line)
	}
	if showPosition {
		lines = append(lines, styles.muted.Render(fmt.Sprintf("… %d/%d", cursor+1, len(rows))))
	}
	return padLines(lines, height)
}

func tableHeader(width int, wide bool, timeLabel, extraLabel string) string {
	if wide {
		nameWidth := max(8, width-42)
		return fmt.Sprintf("%-5s %-*s %9s %-12s %-12s", "TYPE", nameWidth, "NAME", "SIZE", timeLabel, extraLabel)
	}
	nameWidth := max(6, width-29)
	return fmt.Sprintf("%-5s %-*s %9s %-12s", "TYPE", nameWidth, "NAME", "SIZE", timeLabel)
}

func renderTableRow(width int, wide bool, row tableRow) string {
	if wide {
		nameWidth := max(8, width-42)
		return fmt.Sprintf("%-5s %-*s %9s %-12s %-12s", fit(row.kind, 5), nameWidth, fit(row.name, nameWidth), fit(row.size, 9), fit(row.modified, 12), fit(row.extra, 12))
	}
	nameWidth := max(6, width-29)
	return fmt.Sprintf("%-5s %-*s %9s %-12s", fit(row.kind, 5), nameWidth, fit(row.name, nameWidth), fit(row.size, 9), fit(row.modified, 12))
}

func tableWindow(cursor, count, visible int) (int, int) {
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
		text := strings.Join([]string{
			"Keyboard help",
			"Tab switch pane  •  arrows/jk move  •  Enter/right/l open directory",
			"Backspace/left/h parent  •  / filter  •  w compact/wide",
			"F5 copy selected file  •  d details  •  Ctrl+D archive remote document",
			"Remote: n new folder; Ctrl+D deletes an empty opfm marker folder permanently",
			"r refresh  •  s sign in  •  q/Ctrl+C quit  •  Esc close",
		}, "\n")
		return styles.modal.Render(text)
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

func (m *Model) styles() uiStyles {
	return uiStyles{
		contextLabel:    lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")),
		contextValue:    lipgloss.NewStyle().Foreground(lipgloss.Color("255")),
		session:         lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46")),
		localPath:       lipgloss.NewStyle().Foreground(lipgloss.Color("81")),
		remotePath:      lipgloss.NewStyle().Foreground(lipgloss.Color("13")),
		logo:            lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("220")),
		shortcutKey:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39")),
		shortcutLabel:   lipgloss.NewStyle().Foreground(lipgloss.Color("255")),
		input:           lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51")),
		frameActive:     lipgloss.NewStyle().Foreground(lipgloss.Color("51")),
		frameIdle:       lipgloss.NewStyle().Foreground(lipgloss.Color("24")),
		titleActive:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51")),
		titleIdle:       lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("245")),
		tableHeader:     lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("46")),
		tableData:       lipgloss.NewStyle().Foreground(lipgloss.Color("81")),
		selected:        lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("51")).Underline(true),
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
	tableHeader     lipgloss.Style
	tableData       lipgloss.Style
	selected        lipgloss.Style
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
