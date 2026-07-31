# ghpr

`ghpr` is a terminal UI for reviewing and managing open pull requests across repositories owned by a GitHub user or organization.

## Features

- Find open pull requests across all non-archived repositories owned by a GitHub account.
- Select one or many pull requests in a terminal UI.
- View the highlighted pull request's colorized diff in a built-in pager.
- Open the highlighted pull request on GitHub in the default browser.
- Approve and squash-merge selected pull requests.
  - `ghpr` first tries to enable GitHub auto-merge.
  - If auto-merge is unavailable, it attempts a direct squash merge.
- Request changes with a shared review message.
- Close selected pull requests with an optional comment.
- Keep processing a batch when an individual pull request fails.

> [!WARNING]
> Merge and close are destructive actions. Take time to review.

## Installation

### Homebrew

Once a release is available:

```sh
brew install mpepping/tap/ghpr
```

### Go

Go 1.26.1 or newer is required.

```sh
go install github.com/mpepping/ghpr/cmd/ghpr@latest
```

### From source

```sh
git clone https://github.com/mpepping/ghpr.git
cd ghpr
make build
./ghpr --help
```

## Authentication

`ghpr` first reads the token for the active `github.com` account from the GitHub CLI `hosts.yml` file. The path is resolved in this order:

1. `$GH_CONFIG_DIR/hosts.yml`
2. `$XDG_CONFIG_HOME/gh/hosts.yml`
3. `$HOME/.config/gh/hosts.yml`

Both the current multi-account format and the legacy `oauth_token` format are supported. The `gh` executable is not invoked.

Only when `hosts.yml` does not exist, `ghpr` falls back to `GH_TOKEN` and then `GITHUB_TOKEN`:

```sh
export GH_TOKEN="github_pat_..."
ghpr
```

If an existing `hosts.yml` is malformed or has no token for the active `github.com` user, `ghpr` reports the configuration error instead of silently using another credential.

For private repositories, the token must be able to read those repositories. Approving, merging, commenting, and closing also require the corresponding pull-request, contents, and issues write permissions. A classic personal access token normally needs the `repo` scope.

When `--owner` is omitted, `ghpr` uses the login associated with the selected token.

## Usage

```text
Usage: ghpr [options]

Options:
  -limit int
        maximum number of open pull requests to load (1-1000) (default 1000)
  -owner string
        GitHub user or organization that owns the repositories
  -version
        print version information and exit
```

Examples:

```sh
# Repositories owned by the authenticated user
ghpr

# Repositories owned by a specific user or organization
ghpr --owner mpepping

# Limit the initial search
ghpr --owner mpepping --limit 100
```

### TUI keys

| Key                  | Action                                          |
| -------------------- | ----------------------------------------------- |
| `↑` / `↓`, `j` / `k` | Navigate                                        |
| `space`              | Select or deselect the current pull request     |
| `d`                  | Open the highlighted pull request's diff        |
| `w`                  | Open the highlighted pull request in a browser  |
| `a`                  | Select or deselect all                          |
| `m`                  | Approve and squash-merge selected pull requests |
| `c`                  | Close selected pull requests                    |
| `r`                  | Request changes on selected pull requests       |
| `y` / `enter`        | Confirm an action                               |
| `n` / `esc`          | Cancel a prompt                                 |
| `q`                  | Quit                                            |

In the diff viewer, use `space` or `Page Down` to move one page down, `Page Up` to move one page up, and the arrow keys for line-by-line scrolling. Press `esc` or `q` to return to the pull request list.

GitHub does not allow users to approve their own pull requests. Such an action is reported as a per-item failure and the pull request remains selected.

## Development

```sh
make fmt
make test
make lint
make build
```

## License

MIT
