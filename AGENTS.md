# op-file-manager

`opfm` is a Linux terminal file manager for 1Password Documents. It is written
in Go with Bubble Tea and presents a local filesystem pane alongside a virtual
remote tree stored in one 1Password vault.

## Architecture and data model

- `internal/opclient` is the only package that invokes the `op` CLI.
- `internal/app` owns the TUI and must never read, preview, or print document
  contents.
- The remote hierarchy is virtual: Documents use slash-separated titles, and
  persistent folders are tagged marker Documents (`opfm:directory-marker`).
- `internal/config` stores XDG configuration containing only account/vault IDs.
  Never persist session tokens or document contents.

## 1Password authentication

- `OP_ACCOUNT` chooses the configured account.
- Manual CLI sessions use `OP_SESSION_<account shorthand>`, not a generic
  `OP_SESSION` variable. Keep the token only in process memory, remove stale
  inherited session variables for child `op` commands, and clear it on close.
- Resolve a configured account shorthand with `op account list --format json`.
- Do not use `eval` to process CLI output. If a normal sign-in command emits an
  export statement, parse it strictly and keep it private.
- Do not print CLI raw output, tokens, secret material, or document contents in
  the UI, logs, errors, tests, or commit messages.

## Change and safety rules

- Preserve unrelated user changes in a dirty worktree.
- Remote document deletion is archival; only an empty folder marker can be
  removed permanently after confirmation.
- Treat remote upload, download, archive, and marker removal as real external
  mutations. Exercise them manually only when the user has authorized it.

## Validation

Run before handoff:

```sh
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/opfm
```

Use `tmux` for interactive checks. Manual authentication requires user input;
never request, capture, or echo a password or session token.
