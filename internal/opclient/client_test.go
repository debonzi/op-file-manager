package opclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/debonzi/op-file-manager/internal/domain"
)

type fakeRunner struct {
	result Result
	err    error
	args   []string
	env    []string
	stdin  []byte
	run    func(args []string) (Result, error)
}

func TestMain(m *testing.M) {
	if os.Getenv("OPFM_SESSION_COMMAND_HELPER") == "1" {
		fmt.Fprint(os.Stdout, "test-session-token")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func (f *fakeRunner) Run(_ context.Context, env []string, _ string, stdin io.Reader, args ...string) (Result, error) {
	f.args = append([]string(nil), args...)
	f.env = append([]string(nil), env...)
	f.stdin = nil
	if stdin != nil {
		f.stdin, _ = io.ReadAll(stdin)
	}
	if f.run != nil {
		return f.run(args)
	}
	return f.result, f.err
}

func TestListDocumentsUsesJSONWithoutLeakingOutput(t *testing.T) {
	runner := &fakeRunner{result: Result{Stdout: []byte(`[{"id":"doc-1","title":"project/.env","updated_at":"2026-07-14T00:00:00Z"}]`)}}
	client := NewWithRunner("fake-op", runner)
	documents, err := client.ListDocuments(context.Background(), "vault-1")
	if err != nil {
		t.Fatalf("ListDocuments() error = %v", err)
	}
	if len(documents) != 1 || documents[0].FileName != ".env" {
		t.Fatalf("ListDocuments() = %#v", documents)
	}
	if got := strings.Join(runner.args, " "); got != "document list --vault vault-1 --format json" {
		t.Fatalf("op arguments = %q", got)
	}
}

func TestWhoAmIUsesAccountUUIDFromCurrentCLISchema(t *testing.T) {
	runner := &fakeRunner{result: Result{Stdout: []byte(`{"account_uuid":"account-id","url":"example.1password.com"}`)}}
	client := NewWithRunner("fake-op", runner)
	account, err := client.WhoAmI(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if account.ID != "account-id" || account.Name != "example.1password.com" {
		t.Fatalf("WhoAmI() = %#v", account)
	}
}

func TestDownloadOnlyAddsForceAfterCallerConfirmation(t *testing.T) {
	runner := &fakeRunner{}
	client := NewWithRunner("fake-op", runner)
	if err := client.DownloadDocument(context.Background(), "vault", "document", "/safe/file", false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(runner.args, " "), "--force") {
		t.Fatalf("download arguments unexpectedly force overwrite: %#v", runner.args)
	}
	if err := client.DownloadDocument(context.Background(), "vault", "document", "/safe/file", true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(runner.args, " "), "--force") {
		t.Fatalf("confirmed download lacks force: %#v", runner.args)
	}
}

func TestCommandErrorDoesNotExposeCLIStderr(t *testing.T) {
	runner := &fakeRunner{result: Result{Stderr: []byte("SECRET=never-print")}, err: errors.New("exit status 1")}
	client := NewWithRunner("fake-op", runner)
	err := client.Check(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaked CLI stderr: %v", err)
	}
}

func TestConfiguredAccountIsPassedInEnvironment(t *testing.T) {
	runner := &fakeRunner{result: Result{Stdout: []byte(`[]`)}}
	client := NewWithRunner("fake-op", runner)
	client.SetAccount("account-id")
	if _, err := client.ListVaults(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(runner.env, "\n"), "OP_ACCOUNT=account-id") {
		t.Fatalf("environment did not scope command to configured account: %#v", runner.env)
	}
}

func TestSessionEnvironmentNameUsesConfiguredAccountShorthand(t *testing.T) {
	runner := &fakeRunner{result: Result{Stdout: []byte(`[
		{"account_uuid":"other-account","shorthand":"OTHER"},
		{"account_uuid":"account-id","shorthand":"QMMF3DPBPBA7TIDJNVVXFRJM24"}
	]`)}}
	client := NewWithRunner("fake-op", runner)
	name, err := client.sessionEnvironmentName(context.Background(), "account-id")
	if err != nil {
		t.Fatal(err)
	}
	if name != "OP_SESSION_QMMF3DPBPBA7TIDJNVVXFRJM24" {
		t.Fatalf("session environment name = %q", name)
	}
	if got := strings.Join(runner.args, " "); got != "account list --format json" {
		t.Fatalf("op arguments = %q", got)
	}
}

func TestSessionEnvironmentNameRejectsUnknownOrInvalidAccounts(t *testing.T) {
	runner := &fakeRunner{result: Result{Stdout: []byte(`[{"account_uuid":"account-id","shorthand":"not-valid!"}]`)}}
	client := NewWithRunner("fake-op", runner)
	if _, err := client.sessionEnvironmentName(context.Background(), "account-id"); err == nil {
		t.Fatal("sessionEnvironmentName() accepted an invalid shorthand")
	}

	runner.result = Result{Stdout: []byte(`[]`)}
	if _, err := client.sessionEnvironmentName(context.Background(), "account-id"); err == nil {
		t.Fatal("sessionEnvironmentName() accepted an unregistered account")
	}
}

func TestPrivateSessionUsesAccountSpecificVariable(t *testing.T) {
	t.Setenv("OP_SESSION", "legacy-token")
	t.Setenv("OP_SESSION_INHERITED", "inherited-token")
	client := New("fake-op")
	client.SetAccount("account-id")
	client.setSession("OP_SESSION_QMMF3DPBPBA7TIDJNVVXFRJM24", []byte("private-token"))

	env := strings.Join(client.environment(), "\n")
	for _, unwanted := range []string{"OP_SESSION=legacy-token", "OP_SESSION_INHERITED=inherited-token", "OP_SESSION=private-token"} {
		if strings.Contains(env, unwanted) {
			t.Fatalf("environment leaked or used an incorrect session variable: %q", unwanted)
		}
	}
	if !strings.Contains(env, "OP_SESSION_QMMF3DPBPBA7TIDJNVVXFRJM24=private-token") {
		t.Fatalf("environment lacks account-specific session: %#v", client.environment())
	}
	if !strings.Contains(env, "OP_ACCOUNT=account-id") {
		t.Fatalf("environment lacks configured account: %#v", client.environment())
	}
}

func TestParseSessionExport(t *testing.T) {
	name, token, err := parseSessionExport([]byte("export OP_SESSION_QMMF3DPBPBA7TIDJNVVXFRJM24=\"session-token\""))
	if err != nil {
		t.Fatal(err)
	}
	if name != "OP_SESSION_QMMF3DPBPBA7TIDJNVVXFRJM24" || string(token) != "session-token" {
		t.Fatalf("parseSessionExport() = %q, %q", name, token)
	}
}

func TestParseSessionExportRejectsUnexpectedOutputWithoutLeakingIt(t *testing.T) {
	const secret = "session-token-that-must-not-appear"
	_, _, err := parseSessionExport([]byte("export OP_SESSION_QMMF3DPBPBA7TIDJNVVXFRJM24=" + secret))
	if err == nil {
		t.Fatal("parseSessionExport() accepted an unquoted token")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("parseSessionExport() leaked a token: %v", err)
	}
}

func TestSignInCommandSelectsTheSafeSessionFlow(t *testing.T) {
	client := New("fake-op")
	configured := client.SignInCommand(context.Background(), "account-id")
	if !configured.rawToken || strings.Join(configured.args, " ") != "signin --raw --account account-id" {
		t.Fatalf("configured SignInCommand = %#v", configured)
	}
	initial := client.SignInCommand(context.Background(), "")
	if initial.rawToken || strings.Join(initial.args, " ") != "signin" {
		t.Fatalf("initial SignInCommand = %#v", initial)
	}
	add := client.AddAccountAndSignInCommand(context.Background())
	if add.rawToken || strings.Join(add.args, " ") != "account add --signin" {
		t.Fatalf("AddAccountAndSignInCommand = %#v", add)
	}
}

func TestSessionCommandRunStoresRawTokenUnderResolvedName(t *testing.T) {
	t.Setenv("OPFM_SESSION_COMMAND_HELPER", "1")
	runner := &fakeRunner{result: Result{Stdout: []byte(`[{"account_uuid":"account-id","shorthand":"QMMF3DPBPBA7TIDJNVVXFRJM24"}]`)}}
	client := NewWithRunner(os.Args[0], runner)
	command := client.SignInCommand(context.Background(), "account-id")
	var stdout bytes.Buffer
	command.SetStdout(&stdout)
	command.SetStderr(io.Discard)
	if err := command.Run(); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("session token was written to terminal stdout: %q", stdout.String())
	}
	env := strings.Join(client.environment(), "\n")
	if !strings.Contains(env, "OP_SESSION_QMMF3DPBPBA7TIDJNVVXFRJM24=test-session-token") {
		t.Fatalf("environment lacks resolved session variable: %#v", client.environment())
	}
}

func TestCreateVaultUsesJSONAndTheProvidedName(t *testing.T) {
	runner := &fakeRunner{result: Result{Stdout: []byte(`{"id":"vault-id","name":"Project Secrets"}`)}}
	client := NewWithRunner("fake-op", runner)
	vault, err := client.CreateVault(context.Background(), "Project Secrets")
	if err != nil {
		t.Fatal(err)
	}
	if vault.ID != "vault-id" || vault.Name != "Project Secrets" {
		t.Fatalf("CreateVault() = %#v", vault)
	}
	if got := strings.Join(runner.args, " "); got != "vault create Project Secrets --format json" {
		t.Fatalf("op arguments = %q", got)
	}
}

func TestListDocumentsConfirmsDirectoryMarkerTag(t *testing.T) {
	var calls [][]string
	runner := &fakeRunner{run: func(args []string) (Result, error) {
		calls = append(calls, append([]string(nil), args...))
		if strings.Join(args[:2], " ") == "document list" {
			return Result{Stdout: []byte(`[{"id":"marker","title":"projects/api/"},{"id":"file","title":"projects/api/.env"}]`)}, nil
		}
		return Result{Stdout: []byte(`{"tags":["opfm:directory-marker"]}`)}, nil
	}}
	client := NewWithRunner("fake-op", runner)
	documents, err := client.ListDocuments(context.Background(), "vault-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(documents) != 2 || len(documents[0].Tags) != 1 || documents[0].Tags[0] != domain.DirectoryMarkerTag {
		t.Fatalf("ListDocuments() = %#v", documents)
	}
	if len(calls) != 2 || strings.Join(calls[1], " ") != "item get marker --vault vault-1 --format json" {
		t.Fatalf("op calls = %#v", calls)
	}
}

func TestCreateDirectoryMarkerUsesEmptyStandardInput(t *testing.T) {
	runner := &fakeRunner{}
	client := NewWithRunner("fake-op", runner)
	if err := client.CreateDirectoryMarker(context.Background(), "vault", "projects/api"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.args, " "); got != "document create - --vault vault --title projects/api/ --file-name .opfm-directory --tags opfm:directory-marker --format json" {
		t.Fatalf("op arguments = %q", got)
	}
	if !bytes.Equal(runner.stdin, []byte{}) {
		t.Fatalf("marker stdin = %q, want empty", runner.stdin)
	}
}

func TestDeleteDirectoryMarkerDoesNotArchive(t *testing.T) {
	runner := &fakeRunner{}
	client := NewWithRunner("fake-op", runner)
	if err := client.DeleteDirectoryMarker(context.Background(), "vault", "marker"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.args, " "); got != "document delete marker --vault vault" {
		t.Fatalf("op arguments = %q", got)
	}
}

func TestArchiveDocumentUsesRecoverableArchive(t *testing.T) {
	runner := &fakeRunner{}
	client := NewWithRunner("fake-op", runner)
	if err := client.ArchiveDocument(context.Background(), "vault", "document"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(runner.args, " "); got != "document delete document --vault vault --archive" {
		t.Fatalf("op arguments = %q", got)
	}
}
