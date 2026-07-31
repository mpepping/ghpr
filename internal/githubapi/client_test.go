package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/go-github/v81/github"
)

func TestCurrentOwnerAndListOpenPullRequests(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/user":
			writeJSON(t, writer, map[string]any{"login": "mpepping"})
		case "/users/mpepping":
			writeJSON(t, writer, map[string]any{"login": "mpepping", "type": "User"})
		case "/search/issues":
			if got, want := request.URL.Query().Get("q"), "is:pr is:open user:mpepping archived:false"; got != want {
				t.Errorf("search query = %q, want %q", got, want)
			}
			writeJSON(t, writer, map[string]any{
				"total_count":        1,
				"incomplete_results": false,
				"items": []map[string]any{{
					"number":         42,
					"title":          "Update a dependency",
					"html_url":       "https://github.com/mpepping/widgets/pull/42",
					"repository_url": server.URL + "/repos/mpepping/widgets",
					"updated_at":     "2026-07-31T10:00:00Z",
					"draft":          true,
					"user":           map[string]any{"login": "dependabot[bot]"},
					"pull_request":   map[string]any{"url": server.URL + "/repos/mpepping/widgets/pulls/42"},
				}},
			})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testClient(t, server)
	owner, err := client.CurrentOwner(context.Background())
	if err != nil {
		t.Fatalf("CurrentOwner() error = %v", err)
	}
	if owner != "mpepping" {
		t.Fatalf("CurrentOwner() = %q, want mpepping", owner)
	}

	pulls, err := client.ListOpenPullRequests(context.Background(), owner, 100)
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(pulls) != 1 {
		t.Fatalf("ListOpenPullRequests() returned %d items, want 1", len(pulls))
	}
	pull := pulls[0]
	if pull.Key() != "mpepping/widgets#42" || pull.Author != "dependabot[bot]" || !pull.Draft {
		t.Fatalf("unexpected pull request: %#v", pull)
	}
}

func TestListOpenPullRequestsRejectsInvalidOwner(t *testing.T) {
	t.Parallel()

	client := NewClientWithGitHub(github.NewClient(nil))
	_, err := client.ListOpenPullRequests(context.Background(), "owner is:open", 10)
	if err == nil || !strings.Contains(err.Error(), "invalid GitHub owner") {
		t.Fatalf("ListOpenPullRequests() error = %v, want invalid owner error", err)
	}
}

func TestListOpenPullRequestsUsesOrganizationQualifier(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/users/acme":
			writeJSON(t, writer, map[string]any{"login": "acme", "type": "Organization"})
		case "/search/issues":
			if got, want := request.URL.Query().Get("q"), "is:pr is:open org:acme archived:false"; got != want {
				t.Errorf("search query = %q, want %q", got, want)
			}
			writeJSON(t, writer, map[string]any{"total_count": 0, "incomplete_results": false, "items": []any{}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	pulls, err := testClient(t, server).ListOpenPullRequests(context.Background(), "acme", 10)
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(pulls) != 0 {
		t.Fatalf("ListOpenPullRequests() returned %d items, want 0", len(pulls))
	}
}

func TestApproveAndMergeEnablesAutoMerge(t *testing.T) {
	t.Parallel()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		switch request.Method + " " + request.URL.Path {
		case "POST /repos/acme/widgets/pulls/7/reviews":
			assertJSONField(t, request, "event", "APPROVE")
			writeJSON(t, writer, map[string]any{"id": 1})
		case "GET /repos/acme/widgets/pulls/7":
			writeJSON(t, writer, map[string]any{"number": 7, "node_id": "PR_node"})
		case "POST /graphql":
			var body struct {
				Query     string         `json:"query"`
				Variables map[string]any `json:"variables"`
			}
			decodeJSON(t, request, &body)
			if !strings.Contains(body.Query, "enablePullRequestAutoMerge") {
				t.Errorf("GraphQL query does not enable auto-merge: %q", body.Query)
			}
			if body.Variables["pullRequestId"] != "PR_node" {
				t.Errorf("pullRequestId = %v, want PR_node", body.Variables["pullRequestId"])
			}
			writeJSON(t, writer, map[string]any{"data": map[string]any{"enablePullRequestAutoMerge": map[string]any{}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	outcome, err := testClient(t, server).ApproveAndMerge(context.Background(), PullRequest{Owner: "acme", Repo: "widgets", Number: 7})
	if err != nil {
		t.Fatalf("ApproveAndMerge() error = %v", err)
	}
	if outcome != MergeOutcomeScheduled {
		t.Fatalf("ApproveAndMerge() = %q, want %q", outcome, MergeOutcomeScheduled)
	}
	if got, want := strings.Join(requests, ", "), "POST /repos/acme/widgets/pulls/7/reviews, GET /repos/acme/widgets/pulls/7, POST /graphql"; got != want {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}

func TestApproveAndMergeFallsBackToDirectSquashMerge(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /repos/acme/widgets/pulls/8/reviews":
			writeJSON(t, writer, map[string]any{"id": 1})
		case "GET /repos/acme/widgets/pulls/8":
			writeJSON(t, writer, map[string]any{"number": 8, "node_id": "PR_node"})
		case "POST /graphql":
			writeJSON(t, writer, map[string]any{"errors": []map[string]any{{"message": "auto-merge is disabled"}}})
		case "PUT /repos/acme/widgets/pulls/8/merge":
			assertJSONField(t, request, "merge_method", "squash")
			writeJSON(t, writer, map[string]any{"merged": true, "message": "merged"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	outcome, err := testClient(t, server).ApproveAndMerge(context.Background(), PullRequest{Owner: "acme", Repo: "widgets", Number: 8})
	if err != nil {
		t.Fatalf("ApproveAndMerge() error = %v", err)
	}
	if outcome != MergeOutcomeMerged {
		t.Fatalf("ApproveAndMerge() = %q, want %q", outcome, MergeOutcomeMerged)
	}
}

func TestRequestChangesAndClose(t *testing.T) {
	t.Parallel()

	var sawReview, sawComment, sawClose bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /repos/acme/widgets/pulls/9/reviews":
			sawReview = true
			assertJSONField(t, request, "event", "REQUEST_CHANGES")
			writeJSON(t, writer, map[string]any{"id": 1})
		case "POST /repos/acme/widgets/issues/9/comments":
			sawComment = true
			assertJSONField(t, request, "body", "No longer needed")
			writeJSON(t, writer, map[string]any{"id": 2})
		case "PATCH /repos/acme/widgets/issues/9":
			sawClose = true
			assertJSONField(t, request, "state", "closed")
			writeJSON(t, writer, map[string]any{"number": 9, "state": "closed"})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testClient(t, server)
	pull := PullRequest{Owner: "acme", Repo: "widgets", Number: 9}
	if err := client.RequestChanges(context.Background(), pull, "Please add tests"); err != nil {
		t.Fatalf("RequestChanges() error = %v", err)
	}
	if err := client.Close(context.Background(), pull, "No longer needed"); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !sawReview || !sawComment || !sawClose {
		t.Fatalf("requests seen: review=%t comment=%t close=%t", sawReview, sawComment, sawClose)
	}
}

func testClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := github.NewClient(server.Client())
	client.BaseURL = baseURL
	client.UploadURL = baseURL
	return NewClientWithGitHub(client)
}

func writeJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func decodeJSON(t *testing.T, request *http.Request, target any) {
	t.Helper()
	defer request.Body.Close()
	if err := json.NewDecoder(request.Body).Decode(target); err != nil {
		t.Fatalf("decode request JSON: %v", err)
	}
}

func assertJSONField(t *testing.T, request *http.Request, field, want string) {
	t.Helper()
	var body map[string]any
	decodeJSON(t, request, &body)
	if got := fmt.Sprint(body[field]); got != want {
		t.Errorf("request field %q = %q, want %q", field, got, want)
	}
}
