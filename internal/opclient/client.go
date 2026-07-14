// Package opclient is the only layer that invokes the 1Password CLI.
package opclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"

	"github.com/debonzi/op-file-manager/internal/domain"
)

type Result struct {
	Stdout []byte
	Stderr []byte
}

type Runner interface {
	Run(ctx context.Context, env []string, binary string, stdin io.Reader, args ...string) (Result, error)
}

type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, env []string, binary string, stdin io.Reader, args ...string) (Result, error) {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Env = env
	cmd.Stdin = stdin
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}, err
}

// CommandError deliberately avoids printing stderr, since a CLI diagnostic can
// include user metadata. The raw text is retained only long enough to classify
// authentication failures.
type CommandError struct {
	Operation string
	cause     error
	stderr    string
}

func (e *CommandError) Error() string {
	return e.Operation + " failed; refresh or authenticate and try again"
}
func (e *CommandError) Unwrap() error { return e.cause }

func IsAuthenticationError(err error) bool {
	var commandErr *CommandError
	if !errors.As(err, &commandErr) {
		return false
	}
	message := strings.ToLower(commandErr.stderr)
	return strings.Contains(message, "not signed in") ||
		strings.Contains(message, "sign in") ||
		strings.Contains(message, "session") ||
		strings.Contains(message, "authentication")
}

type Client struct {
	binary    string
	runner    Runner
	accountID string

	mu             sync.RWMutex
	session        []byte
	sessionEnvName string
}

func New(binary string) *Client {
	if binary == "" {
		binary = "op"
	}
	return &Client{binary: binary, runner: ExecRunner{}}
}

func NewWithRunner(binary string, runner Runner) *Client {
	client := New(binary)
	client.runner = runner
	return client
}

// SetAccount scopes every CLI call to one configured account. Account IDs are
// metadata, not credentials, and are stored in the XDG configuration file.
func (c *Client) SetAccount(accountID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.accountID = accountID
}

func (c *Client) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearSessionLocked()
}

func (c *Client) clearSessionLocked() {
	for i := range c.session {
		c.session[i] = 0
	}
	c.session = nil
	c.sessionEnvName = ""
}

func (c *Client) Check(ctx context.Context) error {
	_, err := c.run(ctx, "check 1Password CLI", "document", "--help")
	return err
}

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) WhoAmI(ctx context.Context) (Account, error) {
	var wire accountWire
	if err := c.json(ctx, "read signed-in account", &wire, "whoami", "--format", "json"); err != nil {
		return Account{}, err
	}
	account := Account{ID: firstNonEmpty(wire.ID, wire.AccountUUID, wire.AccountID), Name: firstNonEmpty(wire.Name, wire.URL)}
	if account.ID == "" {
		return Account{}, errors.New("read signed-in account: 1Password CLI did not return an account identifier")
	}
	return account, nil
}

// whoami emits account_uuid on current 1Password CLI releases, while some
// earlier and service-account responses use id or account_id.
type accountWire struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	URL         string `json:"url"`
	AccountUUID string `json:"account_uuid"`
	AccountID   string `json:"account_id"`
	Shorthand   string `json:"shorthand"`
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

type Vault struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (c *Client) ListVaults(ctx context.Context) ([]Vault, error) {
	var vaults []Vault
	if err := c.json(ctx, "list vaults", &vaults, "vault", "list", "--format", "json"); err != nil {
		return nil, err
	}
	return vaults, nil
}

func (c *Client) CreateVault(ctx context.Context, name string) (Vault, error) {
	var vault Vault
	if err := c.json(ctx, "create vault", &vault, "vault", "create", name, "--format", "json"); err != nil {
		return Vault{}, err
	}
	return vault, nil
}

func (c *Client) ListDocuments(ctx context.Context, vaultID string) ([]domain.Document, error) {
	var items []documentWire
	if err := c.json(ctx, "list documents", &items, "document", "list", "--vault", vaultID, "--format", "json"); err != nil {
		return nil, err
	}
	documents := make([]domain.Document, 0, len(items))
	for _, item := range items {
		fileName := item.FileName
		var size int64
		if len(item.Files) > 0 {
			if fileName == "" {
				fileName = item.Files[0].Name
			}
			size = item.Files[0].Size
		}
		if fileName == "" && item.Title != "" {
			fileName = path.Base(item.Title)
		}
		document := domain.Document{
			ID:        item.ID,
			Title:     item.Title,
			FileName:  fileName,
			CreatedAt: item.CreatedAt,
			UpdatedAt: item.UpdatedAt,
			Size:      size,
		}
		// document list does not reliably include tags on every CLI version.
		// Confirm the dedicated marker tag before suppressing an item from the
		// normal virtual-file listing.
		if strings.HasSuffix(item.Title, "/") && item.ID != "" {
			tags, err := c.itemTags(ctx, vaultID, item.ID)
			if err != nil {
				return nil, err
			}
			document.Tags = tags
		}
		documents = append(documents, document)
	}
	return documents, nil
}

func (c *Client) itemTags(ctx context.Context, vaultID, itemID string) ([]string, error) {
	var wire struct {
		Tags []string `json:"tags"`
	}
	if err := c.json(ctx, "read document tags", &wire, "item", "get", itemID, "--vault", vaultID, "--format", "json"); err != nil {
		return nil, err
	}
	return wire.Tags, nil
}

type documentWire struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	FileName  string `json:"file_name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
	Files     []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

func (c *Client) CreateDocument(ctx context.Context, vaultID, localFile, remotePath string) error {
	return c.mutate(ctx, "create document", "document", "create", localFile,
		"--vault", vaultID,
		"--title", remotePath,
		"--file-name", domain.LeafName(remotePath),
		"--format", "json")
}

func (c *Client) EditDocument(ctx context.Context, vaultID, documentID, localFile, remotePath string) error {
	return c.mutate(ctx, "update document", "document", "edit", documentID, localFile,
		"--vault", vaultID,
		"--title", remotePath,
		"--file-name", domain.LeafName(remotePath),
		"--format", "json")
}

// CreateDirectoryMarker persists an otherwise-empty virtual directory as a
// zero-content Document. Its tag prevents arbitrary Documents from being
// treated as opfm metadata.
func (c *Client) CreateDirectoryMarker(ctx context.Context, vaultID, dirPath string) error {
	title, err := domain.DirectoryMarkerTitle(dirPath)
	if err != nil {
		return err
	}
	return c.mutateInput(ctx, "create directory marker", strings.NewReader(""), "document", "create", "-",
		"--vault", vaultID,
		"--title", title,
		"--file-name", domain.DirectoryMarkerFileName,
		"--tags", domain.DirectoryMarkerTag,
		"--format", "json")
}

// DeleteDirectoryMarker permanently removes only the explicit marker that
// represents an empty opfm directory. Callers must verify emptiness first.
func (c *Client) DeleteDirectoryMarker(ctx context.Context, vaultID, markerID string) error {
	if markerID == "" {
		return errors.New("directory marker has no identifier")
	}
	return c.mutate(ctx, "delete directory marker", "document", "delete", markerID, "--vault", vaultID)
}

// ArchiveDocument removes a regular Document from opfm's active view while
// keeping it recoverable in 1Password's Archive. Regular Documents are never
// permanently deleted by the TUI.
func (c *Client) ArchiveDocument(ctx context.Context, vaultID, documentID string) error {
	if documentID == "" {
		return errors.New("document has no identifier")
	}
	return c.mutate(ctx, "archive document", "document", "delete", documentID, "--vault", vaultID, "--archive")
}

func (c *Client) DownloadDocument(ctx context.Context, vaultID, documentID, destination string, overwrite bool) error {
	args := []string{"document", "get", documentID, "--vault", vaultID, "--out-file", destination, "--file-mode", "0600"}
	if overwrite {
		args = append(args, "--force")
	}
	return c.mutate(ctx, "download document", args...)
}

func (c *Client) SignIn(ctx context.Context, accountID string) error {
	command := c.SignInCommand(ctx, accountID)
	command.SetStdin(os.Stdin)
	command.SetStderr(os.Stderr)
	return command.Run()
}

// SignInCommand returns an interactive command suitable for tea.Exec. Session
// output stays private rather than being written to terminal scrollback.
func (c *Client) SignInCommand(ctx context.Context, accountID string) *SessionCommand {
	if accountID == "" {
		return c.newExportSessionCommand(ctx, "sign in", "signin")
	}
	args := []string{"signin", "--raw"}
	args = append(args, "--account", accountID)
	return c.newRawSessionCommand(ctx, "sign in", accountID, args...)
}

func (c *Client) AddAccountAndSignIn(ctx context.Context) error {
	command := c.AddAccountAndSignInCommand(ctx)
	command.SetStdin(os.Stdin)
	command.SetStderr(os.Stderr)
	return command.Run()
}

func (c *Client) AddAccountAndSignInCommand(ctx context.Context) *SessionCommand {
	return c.newExportSessionCommand(ctx, "add account and sign in", "account", "add", "--signin")
}

func (c *Client) newRawSessionCommand(ctx context.Context, operation, accountID string, args ...string) *SessionCommand {
	return &SessionCommand{client: c, ctx: ctx, operation: operation, accountID: accountID, rawToken: true, args: args}
}

func (c *Client) newExportSessionCommand(ctx context.Context, operation string, args ...string) *SessionCommand {
	return &SessionCommand{client: c, ctx: ctx, operation: operation, args: args}
}

// SessionCommand implements Bubble Tea's structural ExecCommand interface
// without importing the TUI package. SetStdout intentionally ignores the
// terminal writer so session output never reaches the scrollback.
type SessionCommand struct {
	client    *Client
	ctx       context.Context
	operation string
	accountID string
	rawToken  bool
	args      []string
	stdin     io.Reader
	stderr    io.Writer
	stdout    bytes.Buffer
}

func (c *SessionCommand) SetStdin(reader io.Reader)  { c.stdin = reader }
func (c *SessionCommand) SetStdout(io.Writer)        {}
func (c *SessionCommand) SetStderr(writer io.Writer) { c.stderr = writer }

func (c *SessionCommand) Run() error {
	var sessionEnvName string
	var err error
	if c.rawToken {
		sessionEnvName, err = c.client.sessionEnvironmentName(c.ctx, c.accountID)
		if err != nil {
			return err
		}
	}
	cmd := exec.CommandContext(c.ctx, c.client.binary, c.args...)
	cmd.Env = c.client.environment()
	cmd.Stdin = c.stdin
	cmd.Stderr = c.stderr
	cmd.Stdout = &c.stdout
	defer func() {
		for i := range c.stdout.Bytes() {
			c.stdout.Bytes()[i] = 0
		}
	}()
	if err := cmd.Run(); err != nil {
		return &CommandError{Operation: c.operation, cause: err}
	}

	output := bytes.TrimSpace(c.stdout.Bytes())
	if c.rawToken {
		if len(output) == 0 || bytes.ContainsAny(output, "\r\n") {
			return fmt.Errorf("%s: 1Password CLI did not return a single session token", c.operation)
		}
		c.client.setSession(sessionEnvName, output)
		return nil
	}
	if len(output) == 0 {
		// Desktop-app integration can authenticate without creating a session
		// token. The caller validates that path with a subsequent whoami call.
		c.client.clearSession()
		return nil
	}
	sessionEnvName, token, err := parseSessionExport(output)
	if err != nil {
		return fmt.Errorf("%s: 1Password CLI did not return a valid session export", c.operation)
	}
	c.client.setSession(sessionEnvName, token)
	return nil
}

var sessionExportPattern = regexp.MustCompile(`^export (OP_SESSION_[A-Za-z_][A-Za-z0-9_]*)="([^"\r\n]+)"$`)

func parseSessionExport(output []byte) (string, []byte, error) {
	matches := sessionExportPattern.FindSubmatch(output)
	if len(matches) != 3 {
		return "", nil, errors.New("invalid session export")
	}
	return string(matches[1]), append([]byte(nil), matches[2]...), nil
}

func (c *Client) sessionEnvironmentName(ctx context.Context, accountID string) (string, error) {
	var accounts []accountWire
	if err := c.json(ctx, "list configured 1Password accounts", &accounts, "account", "list", "--format", "json"); err != nil {
		return "", err
	}
	for _, account := range accounts {
		if accountID != firstNonEmpty(account.ID, account.AccountUUID, account.AccountID) {
			continue
		}
		if !sessionShorthandPattern.MatchString(account.Shorthand) {
			return "", errors.New("configured 1Password account has no valid session shorthand")
		}
		return "OP_SESSION_" + account.Shorthand, nil
	}
	return "", errors.New("configured 1Password account is not registered in the local CLI; run op account add")
}

var sessionShorthandPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c *Client) setSession(name string, token []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearSessionLocked()
	c.session = append(c.session, token...)
	c.sessionEnvName = name
}

func (c *Client) clearSession() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearSessionLocked()
}

func (c *Client) json(ctx context.Context, operation string, destination any, args ...string) error {
	result, err := c.run(ctx, operation, args...)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(result.Stdout, destination); err != nil {
		return fmt.Errorf("%s: invalid JSON from 1Password CLI", operation)
	}
	return nil
}

func (c *Client) mutate(ctx context.Context, operation string, args ...string) error {
	_, err := c.run(ctx, operation, args...)
	return err
}

func (c *Client) mutateInput(ctx context.Context, operation string, stdin io.Reader, args ...string) error {
	_, err := c.runInput(ctx, operation, stdin, args...)
	return err
}

func (c *Client) run(ctx context.Context, operation string, args ...string) (Result, error) {
	return c.runInput(ctx, operation, nil, args...)
}

func (c *Client) runInput(ctx context.Context, operation string, stdin io.Reader, args ...string) (Result, error) {
	result, err := c.runner.Run(ctx, c.environment(), c.binary, stdin, args...)
	if err != nil {
		return Result{}, &CommandError{Operation: operation, cause: err, stderr: string(result.Stderr)}
	}
	return result, nil
}

func (c *Client) environment() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	env := make([]string, 0, len(os.Environ())+2)
	for _, pair := range os.Environ() {
		switch {
		case strings.HasPrefix(pair, "OP_ACCOUNT="):
			continue
		case len(c.session) > 0 && (strings.HasPrefix(pair, "OP_SESSION=") || strings.HasPrefix(pair, "OP_SESSION_")):
			continue
		default:
			env = append(env, pair)
		}
	}
	if c.accountID != "" {
		env = append(env, "OP_ACCOUNT="+c.accountID)
	}
	if len(c.session) > 0 && c.sessionEnvName != "" {
		env = append(env, c.sessionEnvName+"="+string(c.session))
	}
	return env
}
