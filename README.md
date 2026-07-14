# opfm

`opfm` is a Linux terminal file manager for [1Password CLI](https://www.1password.dev/cli/). It projects Documents in one configured vault into a virtual tree whose paths are stored in Document titles, and copies individual regular files to or from an isolated local root.

> [!WARNING]
> **Active development:** opfm is under active development and may undergo significant, including breaking, changes. It is not yet considered stable.

## Requirements

- Linux
- Terminal with at least 80×20 cells
- Terminal using a Nerd Font (for file and folder icons)
- Go 1.25 or later to build
- 1Password CLI with `document` commands available
- A manually configured 1Password account for headless sign-in, or an active `op` session

## Build and start

```sh
go build -o opfm ./cmd/opfm
./opfm init
./opfm /path/to/project
```

`opfm init` signs in through the installed `op` CLI when needed, lets you choose an existing vault or create a new one, and stores the selected account and vault IDs in `$XDG_CONFIG_HOME/opfm/config.toml` (or `~/.config/opfm/config.toml`). It never stores a session token.

At runtime, `opfm` inherits an existing 1Password CLI session or signs in through the installed `op` CLI. For manual sessions it resolves the account shorthand and supplies the returned token only to child `op` processes as `OP_SESSION_<shorthand>`; the token is held only in memory.

## Navigation

| Key | Action |
| --- | --- |
| `Tab` | Switch panel |
| arrows or `j`/`k` | Select entry |
| `Enter`, `→`, or `l` | Toggle the selected folder; opening it also makes it the active destination for that panel |
| `←` or `h` | Close the selected folder, or select its parent when it is already closed |
| `Backspace` | Move the active local/remote destination to its parent |
| `/` | Filter the focused tree; matching entries and their ancestors remain visible; `Esc` clears it |
| `d` | Show safe metadata for the selected item |
| `F5` | Copy selected file to the other panel's active destination |
| `n` (remote panel) | Create a child folder in the active remote destination |
| `Ctrl+D` (remote panel) | Archive a Document, or permanently remove a selected empty marker folder |
| `r` | Refresh remote metadata |
| `s` | Sign in through 1Password CLI |
| `?` | Show keyboard help |
| `q` | Quit |

An existing destination is never replaced until `y` or `Enter` confirms the modal. Each pane is a rooted tree: opening a local folder selects the destination for downloads, while opening a remote folder selects the destination for uploads and new folders. A remote destination stays active when its branch is later closed and is marked `DEST` in the tree.

## Appearance

opfm always inherits the terminal background, including terminal transparency. The main view keeps account, vault, session, and both active paths visible above two framed Neo-tree-style file trees. The tree uses periwinkle folders, lavender files, muted blue-gray guides, and a green left gutter for the selected row; it never paints an opaque background. Nerd Font icons distinguish open/closed folders, documents, symbolic links, and invalid items; a compact ASCII opfm mark appears at wide terminal sizes.

## Remote-path convention

Every file created by opfm is a 1Password Document. Its title is its virtual, relative POSIX path, such as `projects/api/dev/.env`; the Document file name is `.env`. While the local pane has focus, `F5` uploads the selected local file to the directory currently open in the remote pane, so it is not necessary (or useful) to encode a remote path in the local filename.

Paths may contain up to 200 Unicode code points and cannot be absolute or include empty, `.` or `..` segments. Documents with invalid or duplicate titles remain visible in the remote pane but are read-only in opfm. Directories containing files are inferred from titles; empty directories need the explicit marker described below.

To persist an empty remote directory, focus the remote pane, open its parent in the tree, and press `n`. opfm creates a zero-content Document titled `directory/`, with the file name `.opfm-directory` and the tag `opfm:directory-marker`. Those marker Documents are never displayed as files. You can create a nested structure one level at a time; after each successful creation opfm keeps the parent as the active destination, expands it, and selects the new directory. `Ctrl+D` permanently deletes only a selected marker directory that is empty—never a regular Document, an implied directory, or a directory containing children. On a regular remote Document, `Ctrl+D` archives it in 1Password instead of deleting it permanently.

## Safety boundaries

- The local pane cannot leave the root passed to `opfm`.
- Only regular local files can be uploaded; symbolic links are refused.
- Downloads refuse to overwrite symbolic links and use 1Password CLI's `0600` file mode.
- The TUI lists metadata only. It never previews Document content, writes content to logs, or uses a shell to invoke `op`.

## Development

```sh
make test
make vet
make build
```

The test suite uses a fake `op` runner; it does not access a 1Password account.
