# ghpr

`ghpr` is a terminal UI for reviewing and managing open pull requests across repositories owned by a GitHub user or organization.

## Features

- Find open pull requests across all non-archived repositories owned by a GitHub account.
- Select one or many pull requests in a terminal UI.
- Show merge readiness at a glance: CI / checks status, review decision, merge conflicts, age and author.
- Filter the list by repository, title, author or number without reloading.
- Refresh the list and all statuses from GitHub without restarting.
- View the highlighted pull request's colorized diff in a built-in pager.
- Open the highlighted pull request on GitHub in the default browser.
- Approve and squash-merge selected pull requests.
  - `ghpr` first tries to enable GitHub auto-merge.
  - If auto-merge is unavailable, it attempts a direct squash merge.
- Request changes with a shared review message.
- Close selected pull requests with an optional comment.
- Keep processing a batch when an individual pull request fails, reporting each result as it happens.

> [!WARNING]
> Merge and close are destructive actions. Take time to review PR's.

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

| Key                  | Action                                              |
| -------------------- | --------------------------------------------------- |
| `↑` / `↓`, `j` / `k` | Navigate                                            |
| `space`              | Select or deselect the current pull request         |
| `a`                  | Select or deselect all *visible* pull requests      |
| `/`                  | Filter the list                                     |
| `esc`                | Clear the active filter                             |
| `R` / `ctrl+r`       | Reload pull requests and statuses from GitHub       |
| `d`                  | Open the highlighted pull request's diff            |
| `w`                  | Open the highlighted pull request in a browser      |
| `m`                  | Approve and squash-merge selected pull requests     |
| `c`                  | Close selected pull requests                        |
| `r`                  | Request changes on selected pull requests           |
| `y` / `enter`        | Confirm an action                                   |
| `n` / `esc`          | Cancel a prompt                                     |
| `q`                  | Quit                                                |

### Columns

```text
      REPOSITORY                 PR      CI RV AGE  AUTHOR           TITLE
> [x] mpepping/ghpr              #12     ✓  ✓  2h   mpepping         Skip approval on self-created PRs
  [ ] mpepping/docker-jitsi-meet #340    ✗  ⚠  3d   dependabot[bot]  Bump alpine from 3.19 to 3.20 in /base
```

`CI` is the status-check rollup for the latest commit:

| Glyph | Meaning                     |
| ----- | --------------------------- |
| `✓`   | All checks passed           |
| `…`   | Checks are still running    |
| `✗`   | At least one check failed   |
| `?`   | Status could not be read    |
| `–`   | No checks configured        |
| `·`   | Still loading               |

`RV` is merge readiness. A merge conflict outranks the review decision, because it blocks the merge regardless of approvals:

| Glyph | Meaning                                    |
| ----- | ------------------------------------------ |
| `⚠`   | Conflicts with the base branch             |
| `✓`   | Approved                                   |
| `✗`   | Changes requested                          |
| `○`   | Review required by branch protection       |
| `–`   | No review decision (no protection rule)    |
| `·`   | Still loading                              |

`AGE` is the time since the pull request was last updated; anything older than 30 days is highlighted. The `AGE` and `AUTHOR` columns are dropped automatically on narrow terminals.

### Filtering

Press `/` and type to narrow the list as you type. Terms are matched case-insensitively against the repository, pull request number, title and author, and multiple terms are combined with AND:

```text
/dependabot go      # Dependabot pull requests mentioning "go"
/ghpr               # everything in the ghpr repository
/#42                # pull request number 42
/draft              # draft pull requests
```

`enter` keeps the filter active, `esc` cancels the edit, and `esc` in the list clears an active filter. `a` selects only the pull requests that currently pass the filter, which makes "filter, select all, merge" a safe bulk workflow. Selections are kept when the filter changes, so you can build a selection across several filters before acting.

In the diff viewer, use `space` or `Page Down` to move one page down, `Page Up` to move one page up, and the arrow keys for line-by-line scrolling. Press `esc` or `q` to return to the pull request list.

While a batch runs, `ghpr` reports each pull request as it completes, so a long run shows live progress (`2/17`), the item currently being processed, and any failures as they occur.

GitHub does not allow users to review their own pull requests. `ghpr` therefore skips the approval step for pull requests you authored yourself and squash-merges them directly. Requesting changes on your own pull request is reported as a per-item failure and the pull request remains selected.

## Development

```sh
make fmt
make test
make lint
make build
```

## License

MIT
