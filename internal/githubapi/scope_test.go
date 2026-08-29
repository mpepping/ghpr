package githubapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestParseScope(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]Scope{
		"owned":            ScopeOwned,
		"REVIEW-REQUESTED": ScopeReviewRequested,
		" involved ":       ScopeInvolved,
		"authored":         ScopeAuthored,
	} {
		got, err := ParseScope(input)
		if err != nil || got != want {
			t.Errorf("ParseScope(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseScope("everything"); err == nil {
		t.Error("ParseScope accepted an unknown scope")
	}
	if !ScopeOwned.RequiresOwner() || ScopeInvolved.RequiresOwner() {
		t.Error("only the owned scope requires an owner")
	}
}

func TestScopeBuildsTheSearchQuery(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		options SearchOptions
		want    string
	}{
		{
			"owned user",
			SearchOptions{Owner: "acme", Scope: ScopeOwned},
			"is:pr is:open user:acme archived:false",
		},
		{
			"review requested without an owner",
			SearchOptions{Scope: ScopeReviewRequested},
			"is:pr is:open review-requested:@me archived:false",
		},
		{
			"review requested narrowed by owner",
			SearchOptions{Owner: "acme", Scope: ScopeReviewRequested},
			"is:pr is:open review-requested:@me user:acme archived:false",
		},
		{
			"involved",
			SearchOptions{Scope: ScopeInvolved},
			"is:pr is:open involves:@me archived:false",
		},
		{
			"authored",
			SearchOptions{Scope: ScopeAuthored},
			"is:pr is:open author:@me archived:false",
		},
		{
			"empty scope defaults to owned",
			SearchOptions{Owner: "acme"},
			"is:pr is:open user:acme archived:false",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var query string
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				switch request.URL.Path {
				case "/users/acme":
					writeJSON(t, writer, map[string]any{"login": "acme", "type": "User"})
				case "/search/issues":
					query = request.URL.Query().Get("q")
					writeJSON(t, writer, map[string]any{"total_count": 0, "items": []any{}})
				default:
					http.NotFound(writer, request)
				}
			}))
			defer server.Close()

			options := testCase.options
			options.Limit = 10
			if _, err := testClient(t, server).ListOpenPullRequests(context.Background(), options); err != nil {
				t.Fatalf("ListOpenPullRequests() error = %v", err)
			}
			if query != testCase.want {
				t.Errorf("query = %q, want %q", query, testCase.want)
			}
		})
	}
}

func TestOwnedScopeRequiresAnOwner(t *testing.T) {
	t.Parallel()

	client := NewClientWithGitHub(nil)
	_, err := client.ListOpenPullRequests(context.Background(), SearchOptions{Scope: ScopeOwned, Limit: 10})
	if err == nil || !strings.Contains(err.Error(), "owner is required") {
		t.Fatalf("error = %v, want a missing owner error", err)
	}
}
