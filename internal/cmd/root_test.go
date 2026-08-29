package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-github/v81/github"

	"github.com/mpepping/ghpr/internal/githubapi"
)

func TestVersionDoesNotRequireToken(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), []string{"--version"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteArgs() error = %v", err)
	}
	if got := stdout.String(); !strings.Contains(got, "ghpr dev") {
		t.Fatalf("version output = %q", got)
	}
}

func TestHelpReturnsSuccess(t *testing.T) {
	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), []string{"--help"}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("ExecuteArgs() error = %v", err)
	}
	got := stdout.String()
	for _, want := range []string{"Usage: ghpr", "GH_TOKEN", "config.yml", "-scope", "-merge-method", "-dry-run", "-json"} {
		if !strings.Contains(got, want) {
			t.Fatalf("help output is missing %q:\n%s", want, got)
		}
	}
}

func TestTokenIsRequired(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GH_CONFIG_DIR", t.TempDir())
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")

	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "GH_TOKEN") {
		t.Fatalf("ExecuteArgs() error = %v, want missing token", err)
	}
}

func TestValidationErrors(t *testing.T) {
	isolateConfig(t)

	cases := []struct {
		args []string
		want string
	}{
		{[]string{"--limit", "1001"}, "between 1 and 1000"},
		{[]string{"--limit", "0"}, "between 1 and 1000"},
		{[]string{"--scope", "everything"}, "invalid scope"},
		{[]string{"--merge-method", "fast-forward"}, "invalid merge method"},
		{[]string{"--owner", "not valid!"}, "invalid GitHub owner"},
		{[]string{"extra-argument"}, "unexpected argument"},
	}
	for _, testCase := range cases {
		var stdout, stderr bytes.Buffer
		err := ExecuteArgs(context.Background(), testCase.args, &stdout, &stderr)
		if err == nil || !strings.Contains(err.Error(), testCase.want) {
			t.Errorf("ExecuteArgs(%v) error = %v, want %q", testCase.args, err, testCase.want)
		}
	}
}

// Flags must win over the configuration file, which must win over defaults.
func TestResolvePrecedence(t *testing.T) {
	writeConfig(t, `
owner: from-config
limit: 250
scope: involved
merge_method: rebase
delete_branch: true
filter: from-config-filter
`)
	t.Setenv("NO_COLOR", "")
	t.Setenv("GH_HOST", "")

	t.Run("config supplies the defaults", func(t *testing.T) {
		resolved := mustResolve(t, nil)
		if resolved.owner != "from-config" || resolved.limit != 250 {
			t.Fatalf("owner=%q limit=%d", resolved.owner, resolved.limit)
		}
		if resolved.scope != githubapi.ScopeInvolved || resolved.mergeMethod != githubapi.MergeMethodRebase {
			t.Fatalf("scope=%q method=%q", resolved.scope, resolved.mergeMethod)
		}
		if !resolved.deleteBranch || resolved.filter != "from-config-filter" {
			t.Fatalf("deleteBranch=%t filter=%q", resolved.deleteBranch, resolved.filter)
		}
	})

	t.Run("flags override the config", func(t *testing.T) {
		resolved := mustResolve(t, []string{
			"--owner", "from-flag",
			"--limit", "10",
			"--scope", "authored",
			"--merge-method", "merge",
			"--filter", "from-flag-filter",
		})
		if resolved.owner != "from-flag" || resolved.limit != 10 {
			t.Fatalf("owner=%q limit=%d", resolved.owner, resolved.limit)
		}
		if resolved.scope != githubapi.ScopeAuthored || resolved.mergeMethod != githubapi.MergeMethodMerge {
			t.Fatalf("scope=%q method=%q", resolved.scope, resolved.mergeMethod)
		}
		if resolved.filter != "from-flag-filter" {
			t.Fatalf("filter=%q", resolved.filter)
		}
	})
}

func TestResolveDefaultsWithoutConfig(t *testing.T) {
	isolateConfig(t)
	t.Setenv("NO_COLOR", "")
	t.Setenv("GH_HOST", "")

	resolved := mustResolve(t, nil)
	if resolved.limit != 1000 || resolved.scope != githubapi.ScopeOwned || resolved.mergeMethod != githubapi.MergeMethodSquash {
		t.Fatalf("unexpected defaults: %#v", resolved)
	}
	if resolved.owner != "" || resolved.dryRun || resolved.noColor {
		t.Fatalf("unexpected defaults: %#v", resolved)
	}
}

// NO_COLOR is the cross-tool convention and must be honoured.
func TestNoColorSources(t *testing.T) {
	isolateConfig(t)
	t.Setenv("GH_HOST", "")

	t.Setenv("NO_COLOR", "1")
	if !mustResolve(t, nil).noColor {
		t.Error("NO_COLOR was ignored")
	}

	t.Setenv("NO_COLOR", "")
	if !mustResolve(t, []string{"--no-color"}).noColor {
		t.Error("--no-color was ignored")
	}
	if mustResolve(t, nil).noColor {
		t.Error("color should be enabled by default")
	}
}

func TestHostFallsBackToGHHostEnvironment(t *testing.T) {
	isolateConfig(t)
	t.Setenv("NO_COLOR", "")
	t.Setenv("GH_HOST", "github.example.com")

	if got := mustResolve(t, nil).host; got != "github.example.com" {
		t.Fatalf("host = %q, want the GH_HOST value", got)
	}
	if got := mustResolve(t, []string{"--host", "flag.example.com"}).host; got != "flag.example.com" {
		t.Fatalf("host = %q, want the flag to win", got)
	}
}

func TestBrokenConfigIsReported(t *testing.T) {
	writeConfig(t, "owner: [unclosed\n")

	var stdout, stderr bytes.Buffer
	err := ExecuteArgs(context.Background(), nil, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "parse ghpr configuration") {
		t.Fatalf("error = %v, want a configuration parse error", err)
	}
}

func TestPrintJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/users/acme":
			writeJSON(t, writer, map[string]any{"login": "acme", "type": "Organization"})
		case "/search/issues":
			writeJSON(t, writer, map[string]any{
				"total_count": 1,
				"items": []map[string]any{{
					"number":         7,
					"title":          "Bump alpine",
					"html_url":       "https://github.com/acme/widgets/pull/7",
					"repository_url": "https://api.github.com/repos/acme/widgets",
					"updated_at":     "2026-07-31T10:00:00Z",
					"draft":          true,
					"user":           map[string]any{"login": "dependabot[bot]"},
					"pull_request":   map[string]any{"url": "x"},
				}},
			})
		case "/graphql":
			writeJSON(t, writer, map[string]any{"data": map[string]any{
				"pr0": map[string]any{
					"mergeable":      "CONFLICTING",
					"reviewDecision": "CHANGES_REQUESTED",
					"commits": map[string]any{"nodes": []any{map[string]any{
						"commit": map[string]any{"statusCheckRollup": map[string]any{"state": "FAILURE"}},
					}}},
				},
			}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	client := clientFor(t, server)
	search := githubapi.SearchOptions{Owner: "acme", Scope: githubapi.ScopeOwned, Limit: 10}
	if err := printJSON(context.Background(), client, search, &stdout); err != nil {
		t.Fatalf("printJSON() error = %v", err)
	}

	var decoded []jsonPullRequest
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, stdout.String())
	}
	if len(decoded) != 1 {
		t.Fatalf("decoded %d records, want 1", len(decoded))
	}

	got := decoded[0]
	if got.Repository != "acme/widgets" || got.Number != 7 || got.Author != "dependabot[bot]" || !got.Draft {
		t.Fatalf("unexpected record: %#v", got)
	}
	if got.Checks != "failure" || got.Mergeable != "conflicting" || got.Review != "changes requested" {
		t.Fatalf("merge readiness missing from the record: %#v", got)
	}
	if !got.Blocked {
		t.Error("a conflicting pull request with failing checks must be reported as blocked")
	}
	if got.UpdatedAt.IsZero() {
		t.Error("updatedAt was not populated")
	}
}

func TestPrintJSONEmptyListIsValidJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/users/acme":
			writeJSON(t, writer, map[string]any{"login": "acme", "type": "User"})
		case "/search/issues":
			writeJSON(t, writer, map[string]any{"total_count": 0, "items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	var stdout bytes.Buffer
	search := githubapi.SearchOptions{Owner: "acme", Limit: 10}
	if err := printJSON(context.Background(), clientFor(t, server), search, &stdout); err != nil {
		t.Fatalf("printJSON() error = %v", err)
	}
	var decoded []jsonPullRequest
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("empty output is not valid JSON: %v (%q)", err, stdout.String())
	}
	if len(decoded) != 0 {
		t.Fatalf("want an empty array, got %d records", len(decoded))
	}
}

// isolateConfig points the loader at an empty directory so tests never read
// the developer's own configuration.
func isolateConfig(t *testing.T) {
	t.Helper()
	t.Setenv("GHPR_CONFIG", filepath.Join(t.TempDir(), "config.yml"))
}

func writeConfig(t *testing.T, contents string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GHPR_CONFIG", path)
}

func mustResolve(t *testing.T, args []string) settings {
	t.Helper()
	var opts options
	flags := newFlagSet(&opts, io.Discard)
	if err := flags.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	resolved, err := resolve(&opts, flags)
	if err != nil {
		t.Fatalf("resolve(%v) error = %v", args, err)
	}
	return resolved
}

func clientFor(t *testing.T, server *httptest.Server) *githubapi.Client {
	t.Helper()
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	client := github.NewClient(server.Client())
	client.BaseURL = baseURL
	client.UploadURL = baseURL
	return githubapi.NewClientWithGitHub(client)
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}
