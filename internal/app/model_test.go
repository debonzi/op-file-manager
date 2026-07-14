package app

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/debonzi/op-file-manager/internal/config"
	"github.com/debonzi/op-file-manager/internal/domain"
	"github.com/debonzi/op-file-manager/internal/localfs"
	"github.com/debonzi/op-file-manager/internal/opclient"
)

type recordingRunner struct {
	args  []string
	stdin []byte
}

func (r *recordingRunner) Run(_ context.Context, _ []string, _ string, stdin io.Reader, args ...string) (opclient.Result, error) {
	r.args = append([]string(nil), args...)
	r.stdin = nil
	if stdin != nil {
		r.stdin, _ = io.ReadAll(stdin)
	}
	return opclient.Result{}, nil
}

func newTestModel(t *testing.T, client *opclient.Client, root localfs.Root) *Model {
	t.Helper()
	return New(context.Background(), client, config.Config{Version: 1, AccountID: "account", VaultID: "vault"}, root, ContextInfo{AccountName: "Account", VaultName: "Vault"})
}

func TestUploadToExistingRemotePathRequiresConfirmation(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("SECRET=x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.tree = domain.BuildTree([]domain.Document{{ID: "doc", Title: ".env"}})
	model.remoteLoading = false
	_, command := model.prepareUpload()
	if model.pending == nil || !model.pending.overwrite || model.mode != modeConfirmTransfer || command != nil {
		t.Fatalf("prepareUpload() pending = %#v, mode = %v, command = %#v", model.pending, model.mode, command)
	}
}

func TestOpeningRemoteDocumentExplainsThatThereIsNoPreview(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.focus = FocusRemote
	model.remoteLoading = false
	model.tree = domain.BuildTree([]domain.Document{{ID: "doc", Title: ".env"}})
	model.openSelected()
	if model.status != "No document preview; press F5 to copy the selected file" {
		t.Fatalf("status = %q", model.status)
	}
}

func TestCreateRemoteFolderUsesCurrentRemoteDirectory(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	model := newTestModel(t, opclient.NewWithRunner("op", runner), root)
	model.focus = FocusRemote
	model.remoteLoading = false
	model.remoteDir = []string{"projects"}
	model.tree = domain.BuildTree([]domain.Document{{ID: "file", Title: "projects/.env"}})

	model.beginCreateDirectory()
	model.handleFolderKey("", "api")
	_, command := model.handleFolderKey("enter", "")
	if command == nil {
		t.Fatal("folder creation did not return a command")
	}
	message := command()
	if got := strings.Join(runner.args, " "); got != "document create - --vault vault --title projects/api/ --file-name .opfm-directory --tags opfm:directory-marker --format json" {
		t.Fatalf("op arguments = %q", got)
	}
	model.Update(message)
	if got := strings.Join(model.remoteDir, "/"); got != "projects" {
		t.Fatalf("remoteDir = %q", got)
	}
	if got := strings.Join(model.pendingCreatedDir, "/"); got != "projects/api" {
		t.Fatalf("pendingCreatedDir = %q", got)
	}
	model.Update(documentsLoadedMsg{tree: domain.BuildTree([]domain.Document{
		{ID: "file", Title: "projects/.env"},
		{ID: "marker", Title: "projects/api/", Tags: []string{domain.DirectoryMarkerTag}},
	})})
	selected, ok := model.selectedRemote()
	if !ok || !selected.IsDir || selected.Name != "api" {
		t.Fatalf("selected remote entry = %#v", selected)
	}
	if model.pendingCreatedDir != nil || model.status != "Remote folder created; selected /projects/api" {
		t.Fatalf("pending = %#v, status = %q", model.pendingCreatedDir, model.status)
	}
	if !model.remoteExpanded["projects"] {
		t.Fatal("parent folder was not expanded to reveal the created folder")
	}
}

func TestCreatedFolderSelectionIsClearedWhenRefreshFails(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.pendingCreatedDir = []string{"projects", "api"}
	model.Update(documentsLoadedMsg{err: os.ErrPermission})
	if model.pendingCreatedDir != nil {
		t.Fatalf("pendingCreatedDir was retained after a refresh failure: %#v", model.pendingCreatedDir)
	}
}

func TestCtrlDArchivesRemoteDocument(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	model := newTestModel(t, opclient.NewWithRunner("op", runner), root)
	model.focus = FocusRemote
	model.remoteLoading = false
	model.tree = domain.BuildTree([]domain.Document{{ID: "document", Title: ".env"}})

	_, command := model.handleKey("ctrl+d", "")
	if command != nil || model.mode != modeConfirmRemoval || model.removal == nil || model.removal.kind != archiveDocument {
		t.Fatalf("archive confirmation = %#v, mode = %v, command = %#v", model.removal, model.mode, command)
	}
	_, command = model.handleKey("y", "")
	if command == nil {
		t.Fatal("archive did not return a command")
	}
	command()
	if got := strings.Join(runner.args, " "); got != "document delete document --vault vault --archive" {
		t.Fatalf("op arguments = %q", got)
	}
}

func TestCtrlDDoesNotRemoveLocalFiles(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	_, command := model.handleKey("ctrl+d", "")
	if command != nil || model.removal != nil || model.mode != modeNormal {
		t.Fatalf("local removal state = %#v, mode = %v, command = %#v", model.removal, model.mode, command)
	}
	if model.status != "Remote items can be archived only from the remote pane" {
		t.Fatalf("status = %q", model.status)
	}
}

func TestCtrlDDeletesOnlyEmptyDirectoryMarker(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	model := newTestModel(t, opclient.NewWithRunner("op", runner), root)
	model.focus = FocusRemote
	model.remoteLoading = false
	model.remoteDir = []string{"projects"}
	model.tree = domain.BuildTree([]domain.Document{{ID: "marker", Title: "projects/api/", Tags: []string{domain.DirectoryMarkerTag}}})
	model.remoteExpanded["projects"] = true
	model.remoteCursor = 1

	_, command := model.handleKey("ctrl+d", "")
	if command != nil || model.mode != modeConfirmRemoval || model.removal == nil || model.removal.kind != deleteDirectoryMarker {
		t.Fatalf("marker confirmation = %#v, mode = %v, command = %#v", model.removal, model.mode, command)
	}
	_, command = model.handleKey("y", "")
	if command == nil {
		t.Fatal("marker removal did not return a command")
	}
	command()
	if got := strings.Join(runner.args, " "); got != "document delete marker --vault vault" {
		t.Fatalf("op arguments = %q", got)
	}
}

func TestFilterIsScopedToFocusedPaneAndEscapeClearsIt(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "match.env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.tree = domain.BuildTree([]domain.Document{{ID: "remote", Title: "match.env"}})

	model.handleKey("/", "")
	model.handleKey("", "match")
	if model.localFilter != "match" || model.remoteFilter != "" || model.mode != modeFilter {
		t.Fatalf("filters = local %q remote %q mode %v", model.localFilter, model.remoteFilter, model.mode)
	}
	model.handleKey("esc", "")
	if model.localFilter != "" || model.mode != modeNormal {
		t.Fatalf("filter was not cleared: %#v", model)
	}
}

func TestViewRendersContextualTreeAndActions(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := New(context.Background(), opclient.New("op"), config.Config{Version: 1, AccountID: "account", VaultID: "vault"}, root, ContextInfo{AccountName: "Personal", VaultName: "Secrets"})
	model.width, model.height = 100, 20
	model.remoteLoading = false
	model.tree = domain.BuildTree([]domain.Document{{ID: "doc", Title: ".env", Size: 12, UpdatedAt: "2026-07-14T00:00:00Z"}})
	content := ansiEscapePattern.ReplaceAllString(model.View().Content, "")
	for _, wanted := range []string{"ACCOUNT: Personal", "VAULT: Secrets", "SESSION: CONNECTED", "LOCAL:", "REMOTE:", "LOCAL TREE  DEST .  [1]", "REMOTE TREE  DEST /  [1]", documentIcon, "<F5> Upload", "<Enter> Toggle", "<d> Details", "____  ____  ________", "<local>", "<remote>"} {
		if !strings.Contains(content, wanted) {
			t.Fatalf("view does not contain %q:\n%s", wanted, content)
		}
	}
}

func TestTreeCursorUsesGutterWithoutPaintingTheSelectedRow(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "folder"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	rows, err := model.localRows()
	if err != nil {
		t.Fatal(err)
	}
	lines := model.renderTreeLines(48, 4, rows, 0, true)
	if len(lines) == 0 {
		t.Fatal("tree did not render a selected row")
	}
	line := lines[0]
	if plain := ansiEscapePattern.ReplaceAllString(line, ""); !strings.HasPrefix(plain, cursorIcon+"└─ "+closedFolderIcon+" folder") {
		t.Fatalf("cursor gutter line = %q", plain)
	}
	if !strings.Contains(line, model.styles().cursor.Render(cursorIcon)) {
		t.Fatalf("cursor did not use the green gutter style: %q", line)
	}
	if !strings.Contains(line, model.styles().directory.Render(closedFolderIcon+" ")) {
		t.Fatalf("selected folder lost its normal directory color: %q", line)
	}
	if strings.Contains(line, "\x1b[4m") || strings.Contains(line, "\x1b[48;") {
		t.Fatalf("selected row used underline or background: %q", line)
	}
}

func TestHelpModalUsesAlignedShortcutGrid(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.mode = modeHelp
	content := ansiEscapePattern.ReplaceAllString(model.renderModal(), "")
	for _, wanted := range []string{
		"Keyboard shortcuts",
		"<Tab> Switch pane",
		"<↑/↓, j/k> Move selection",
		"<Backspace> Destination parent",
		"<Ctrl+D> Archive / delete marker",
		"<q, Ctrl+C> Quit",
	} {
		if !strings.Contains(content, wanted) {
			t.Fatalf("help modal does not contain %q:\n%s", wanted, content)
		}
	}
	if strings.Contains(content, " • ") {
		t.Fatalf("help modal retained the old inline format:\n%s", content)
	}
}

func TestRemoteTreeToggleKeepsCollapsedDestination(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "secret.env"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordingRunner{}
	model := newTestModel(t, opclient.NewWithRunner("op", runner), root)
	model.focus = FocusRemote
	model.remoteLoading = false
	model.tree = domain.BuildTree([]domain.Document{{ID: "doc", Title: "projects/api/.env"}})

	model.openSelected() // projects
	model.remoteCursor = 1
	model.openSelected() // projects/api
	if got := strings.Join(model.remoteDir, "/"); got != "projects/api" {
		t.Fatalf("opened remote destination = %q", got)
	}
	model.openSelected() // close projects/api
	if got := strings.Join(model.remoteDir, "/"); got != "projects/api" {
		t.Fatalf("collapsed remote destination = %q", got)
	}
	if model.remoteExpanded["projects/api"] {
		t.Fatal("collapsed destination remained expanded")
	}

	model.focus = FocusLocal
	_, command := model.prepareUpload()
	if command == nil {
		t.Fatal("upload did not start")
	}
	command()
	if got := strings.Join(runner.args, " "); !strings.Contains(got, "--title projects/api/secret.env") {
		t.Fatalf("upload did not use collapsed destination: %q", got)
	}

	model.width, model.height = 120, 20
	content := ansiEscapePattern.ReplaceAllString(model.View().Content, "")
	if !strings.Contains(content, destinationIcon+" DEST") {
		t.Fatalf("tree view does not mark destination:\n%s", content)
	}
}

func TestTreeFilterIncludesAncestorsWithoutChangingExpansion(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.tree = domain.BuildTree([]domain.Document{
		{ID: "target", Title: "projects/api/target.env"},
		{ID: "other", Title: "other.env"},
	})
	model.remoteFilter = "target"
	rows := model.remoteRows()
	if len(rows) != 3 {
		t.Fatalf("filtered row count = %d: %#v", len(rows), rows)
	}
	if got := []string{rows[0].name, rows[1].name, rows[2].name}; strings.Join(got, "/") != "projects/api/target.env" {
		t.Fatalf("filtered rows = %#v", rows)
	}
	if len(model.remoteExpanded) != 0 {
		t.Fatalf("filter mutated expanded folders: %#v", model.remoteExpanded)
	}
	model.remoteFilter = ""
	rows = model.remoteRows()
	if len(rows) != 2 || rows[0].name != "projects" || rows[1].name != "other.env" {
		t.Fatalf("cleared filter rows = %#v", rows)
	}
}

func TestRemoteTreePreservesSelectionAcrossRefresh(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.remoteLoading = false
	model.remoteExpanded["projects"] = true
	model.tree = domain.BuildTree([]domain.Document{{ID: "first", Title: "projects/api/.env"}})
	model.remoteCursor = 1 // projects/api

	model.Update(documentsLoadedMsg{tree: domain.BuildTree([]domain.Document{
		{ID: "first", Title: "projects/api/.env"},
		{ID: "second", Title: "other.env"},
	})})
	selected, ok := model.selectedRemote()
	if !ok || !selected.IsDir || selected.Name != "api" {
		t.Fatalf("refresh selection = %#v", selected)
	}
}

func TestLocalTreeToggleKeepsDownloadDestination(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "downloads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "downloads", "note.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.openSelected()
	if model.localDir != "downloads" || !model.localExpanded["downloads"] {
		t.Fatalf("opened local tree = dir %q expanded %#v", model.localDir, model.localExpanded)
	}
	model.openSelected()
	if model.localDir != "downloads" || model.localExpanded["downloads"] {
		t.Fatalf("collapsed local tree = dir %q expanded %#v", model.localDir, model.localExpanded)
	}
}

func TestLocalTreeNeverExpandsSymlink(t *testing.T) {
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "target"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(directory, "linked")); err != nil {
		t.Fatal(err)
	}
	root, err := localfs.NewRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.openSelected()
	if model.localExpanded["linked"] {
		t.Fatal("symbolic link was expanded")
	}
}

func TestViewRequiresAtLeast80x20(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.width, model.height = 80, 19
	if got := ansiEscapePattern.ReplaceAllString(model.View().Content, ""); !strings.Contains(got, "requires at least 80×20") {
		t.Fatalf("small terminal view = %q", got)
	}
	model.height = 20
	if got := ansiEscapePattern.ReplaceAllString(model.View().Content, ""); strings.Contains(got, "requires at least 80×20") {
		t.Fatalf("minimum supported terminal was rejected: %q", got)
	}
}

func TestLogoIsResponsiveAndPanelsAreFramed(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.remoteLoading = false
	model.width, model.height = 120, 24
	wide := ansiEscapePattern.ReplaceAllString(model.View().Content, "")
	for _, wanted := range []string{"____  ____  ________", "/ __ \\/ __ \\/ ____", "\\____/_/   /_/   /_/", "┌─ LOCAL", "┌─ REMOTE", "└"} {
		if !strings.Contains(wide, wanted) {
			t.Fatalf("wide view does not contain %q:\n%s", wanted, wide)
		}
	}

	model.width = logoMinimumWidth - 1
	compact := ansiEscapePattern.ReplaceAllString(model.View().Content, "")
	if strings.Contains(compact, "____  ____  ________") {
		t.Fatalf("compact view unexpectedly rendered the logo:\n%s", compact)
	}
}

func TestRemoteFocusUsesRemoteContextualActions(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.focus = FocusRemote
	model.remoteLoading = false
	model.width, model.height = 120, 20
	content := ansiEscapePattern.ReplaceAllString(model.View().Content, "")
	for _, wanted := range []string{"<F5> Download", "<n> Folder", "<Ctrl+D> Archive"} {
		if !strings.Contains(content, wanted) {
			t.Fatalf("remote action bar does not contain %q:\n%s", wanted, content)
		}
	}
}

func TestViewDoesNotPaintAnOpaqueBackground(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := New(context.Background(), opclient.New("op"), config.Config{Version: 1, AccountID: "account", VaultID: "vault"}, root, ContextInfo{})
	model.width, model.height = 100, 20
	model.remoteLoading = false
	if strings.Contains(model.View().Content, "\x1b[48;") {
		t.Fatal("view emitted an ANSI background color")
	}
}

var ansiEscapePattern = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func TestDetailsNeverContainDocumentContent(t *testing.T) {
	root, err := localfs.NewRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	model := newTestModel(t, opclient.New("op"), root)
	model.focus = FocusRemote
	model.tree = domain.BuildTree([]domain.Document{{ID: "doc", Title: ".env", Size: 12, UpdatedAt: "2026-07-14T00:00:00Z"}})
	model.showDetails()
	if model.mode != modeDetails || model.details == nil {
		t.Fatal("details did not open")
	}
	text := strings.Join(model.details.lines, "\n")
	if strings.Contains(text, "SECRET") || !strings.Contains(text, "Virtual path") {
		t.Fatalf("unsafe or incomplete details: %q", text)
	}
}
