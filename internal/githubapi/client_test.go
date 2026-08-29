package githubapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
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

	pulls, err := client.ListOpenPullRequests(context.Background(), SearchOptions{Owner: owner, Limit: 100})
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
	_, err := client.ListOpenPullRequests(context.Background(), SearchOptions{Owner: "owner is:open", Limit: 10})
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

	pulls, err := testClient(t, server).ListOpenPullRequests(context.Background(), SearchOptions{Owner: "acme", Limit: 10})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(pulls) != 0 {
		t.Fatalf("ListOpenPullRequests() returned %d items, want 0", len(pulls))
	}
}

// Regression test: PerPage must not shrink on the last page. GitHub derives the
// offset from (page-1)*per_page, so a smaller final page would re-request rows
// that were already collected and silently skip the ones after them.
func TestListOpenPullRequestsPaginatesBeyondOnePage(t *testing.T) {
	t.Parallel()

	const total = 150
	var server *httptest.Server
	var perPageValues []string
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/users/acme":
			writeJSON(t, writer, map[string]any{"login": "acme", "type": "Organization"})
		case "/search/issues":
			query := request.URL.Query()
			perPageValues = append(perPageValues, query.Get("per_page"))
			perPage, _ := strconv.Atoi(query.Get("per_page"))
			page, _ := strconv.Atoi(query.Get("page"))
			if perPage <= 0 {
				perPage = 30
			}
			if page <= 0 {
				page = 1
			}

			// Mirror GitHub's offset arithmetic.
			start := (page - 1) * perPage
			end := min(total, start+perPage)
			items := make([]map[string]any, 0, max(0, end-start))
			for number := start + 1; number <= end; number++ {
				items = append(items, map[string]any{
					"number":         number,
					"title":          fmt.Sprintf("PR %d", number),
					"html_url":       fmt.Sprintf("https://github.com/acme/widgets/pull/%d", number),
					"repository_url": server.URL + "/repos/acme/widgets",
					"user":           map[string]any{"login": "someone"},
					"pull_request":   map[string]any{"url": "x"},
				})
			}
			if end < total {
				writer.Header().Set("Link", fmt.Sprintf(`<%s/search/issues?page=%d>; rel="next"`, server.URL, page+1))
			}
			writeJSON(t, writer, map[string]any{"total_count": total, "incomplete_results": false, "items": items})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	pulls, err := testClient(t, server).ListOpenPullRequests(context.Background(), SearchOptions{Owner: "acme", Limit: total})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(pulls) != total {
		t.Fatalf("got %d pull requests, want %d", len(pulls), total)
	}
	for _, perPage := range perPageValues {
		if perPage != "100" {
			t.Fatalf("per_page values = %v, want every page to use 100", perPageValues)
		}
	}

	seen := make(map[string]bool, len(pulls))
	for _, pr := range pulls {
		if seen[pr.Key()] {
			t.Fatalf("duplicate pull request %s", pr.Key())
		}
		seen[pr.Key()] = true
	}
	for number := 1; number <= total; number++ {
		if !seen[fmt.Sprintf("acme/widgets#%d", number)] {
			t.Fatalf("pull request #%d is missing from the results", number)
		}
	}
}

// A limit that is not a multiple of the page size must still stop exactly at
// the limit.
func TestListOpenPullRequestsHonoursLimit(t *testing.T) {
	t.Parallel()

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/users/acme":
			writeJSON(t, writer, map[string]any{"login": "acme", "type": "User"})
		case "/search/issues":
			items := make([]map[string]any, 0, 100)
			for number := 1; number <= 100; number++ {
				items = append(items, map[string]any{
					"number":         number,
					"repository_url": server.URL + "/repos/acme/widgets",
					"pull_request":   map[string]any{"url": "x"},
				})
			}
			writeJSON(t, writer, map[string]any{"total_count": 100, "incomplete_results": false, "items": items})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	pulls, err := testClient(t, server).ListOpenPullRequests(context.Background(), SearchOptions{Owner: "acme", Limit: 42})
	if err != nil {
		t.Fatalf("ListOpenPullRequests() error = %v", err)
	}
	if len(pulls) != 42 {
		t.Fatalf("got %d pull requests, want 42", len(pulls))
	}
}

func TestDiff(t *testing.T) {
	t.Parallel()

	const want = "diff --git a/file.go b/file.go\n-old\n+new\n"
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/repos/acme/widgets/pulls/10" {
			http.NotFound(writer, request)
			return
		}
		if got := request.Header.Get("Accept"); got != "application/vnd.github.v3.diff" {
			t.Errorf("Accept header = %q, want diff media type", got)
		}
		writer.Header().Set("Content-Type", "text/plain")
		_, _ = writer.Write([]byte(want))
	}))
	defer server.Close()

	got, err := testClient(t, server).Diff(context.Background(), PullRequest{Owner: "acme", Repo: "widgets", Number: 10})
	if err != nil {
		t.Fatalf("Diff() error = %v", err)
	}
	if got != want {
		t.Fatalf("Diff() = %q, want %q", got, want)
	}
}

func TestPullRequestStates(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/graphql" {
			http.NotFound(writer, request)
			return
		}
		var body graphQLRequest
		decodeJSON(t, request, &body)
		for _, fragment := range []string{"statusCheckRollup", "commits(last: 1)", "mergeable", "reviewDecision"} {
			if !strings.Contains(body.Query, fragment) {
				t.Errorf("query is missing %q: %s", fragment, body.Query)
			}
		}
		if got := body.Variables["url1"]; got != "https://github.com/acme/widgets/pull/2" {
			t.Errorf("url1 variable = %v", got)
		}
		writeJSON(t, writer, map[string]any{"data": map[string]any{
			"pr0": map[string]any{
				"mergeable":      "MERGEABLE",
				"reviewDecision": "APPROVED",
				"commits":        rollup("SUCCESS"),
			},
			"pr1": map[string]any{
				"mergeable":      "CONFLICTING",
				"reviewDecision": "REVIEW_REQUIRED",
				"commits":        rollup("PENDING"),
			},
			"pr2": map[string]any{
				"mergeable":      "UNKNOWN",
				"reviewDecision": "CHANGES_REQUESTED",
				"commits":        rollup("ERROR"),
			},
			"pr3": map[string]any{
				"mergeable":      "MERGEABLE",
				"reviewDecision": nil,
				"commits":        map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": nil}}}},
			},
		}})
	}))
	defer server.Close()

	pulls := testPulls(4)
	states, err := testClient(t, server).PullRequestStates(context.Background(), pulls)
	if err != nil {
		t.Fatalf("PullRequestStates() error = %v", err)
	}
	want := []PullRequestState{
		{Build: BuildStatusSuccess, Mergeable: MergeableClean, Review: ReviewDecisionApproved},
		{Build: BuildStatusPending, Mergeable: MergeableConflicting, Review: ReviewDecisionReviewRequired},
		{Build: BuildStatusFailure, Mergeable: MergeableUnknown, Review: ReviewDecisionChangesRequested},
		{Build: BuildStatusNone, Mergeable: MergeableClean, Review: ReviewDecisionNone},
	}
	for index, pr := range pulls {
		if got := states[pr.Key()]; got != want[index] {
			t.Errorf("state for %s = %#v, want %#v", pr.Key(), got, want[index])
		}
	}
}

// A GraphQL error for one alias must not discard the data returned for the
// others, and the message must reach the caller instead of being swallowed.
func TestPullRequestStatesReturnsPartialResultsAndError(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/graphql" {
			http.NotFound(writer, request)
			return
		}
		writeJSON(t, writer, map[string]any{
			"data": map[string]any{
				"pr0": map[string]any{"mergeable": "MERGEABLE", "reviewDecision": "APPROVED", "commits": rollup("SUCCESS")},
				"pr1": nil,
			},
			"errors": []map[string]any{{
				"message": "Resource not accessible by integration",
				"path":    []any{"pr1"},
			}},
		})
	}))
	defer server.Close()

	pulls := testPulls(2)
	states, err := testClient(t, server).PullRequestStates(context.Background(), pulls)
	if err == nil || !strings.Contains(err.Error(), "not accessible") {
		t.Fatalf("PullRequestStates() error = %v, want the GraphQL message", err)
	}
	if got := states[pulls[0].Key()]; got.Build != BuildStatusSuccess || got.Review != ReviewDecisionApproved {
		t.Errorf("healthy pull request lost its state: %#v", got)
	}
	if got := states[pulls[1].Key()]; got.Build != BuildStatusUnknown {
		t.Errorf("failed pull request state = %#v, want unknown build", got)
	}
}

func TestPullRequestStatesRejectsOversizedBatch(t *testing.T) {
	t.Parallel()

	pulls := make([]PullRequest, MaxStateBatch+1)
	_, err := NewClientWithGitHub(github.NewClient(nil)).PullRequestStates(context.Background(), pulls)
	if err == nil || !strings.Contains(err.Error(), "maximum") {
		t.Fatalf("PullRequestStates() error = %v, want batch limit error", err)
	}
}

func rollup(state string) map[string]any {
	return map[string]any{"nodes": []any{map[string]any{"commit": map[string]any{"statusCheckRollup": map[string]any{"state": state}}}}}
}

func testPulls(count int) []PullRequest {
	pulls := make([]PullRequest, count)
	for index := range pulls {
		pulls[index] = PullRequest{
			Owner:  "acme",
			Repo:   "widgets",
			Number: index + 1,
			URL:    fmt.Sprintf("https://github.com/acme/widgets/pull/%d", index+1),
		}
	}
	return pulls
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

func TestApproveAndMergeSkipsSelfAuthoredApproval(t *testing.T) {
	t.Parallel()

	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests = append(requests, request.Method+" "+request.URL.Path)
		switch request.Method + " " + request.URL.Path {
		case "GET /user":
			writeJSON(t, writer, map[string]any{"login": "mpepping"})
		case "POST /repos/mpepping/neovim/pulls/1/reviews":
			t.Error("self-authored pull request must not be approved")
			http.Error(writer, "unprocessable entity", http.StatusUnprocessableEntity)
		case "GET /repos/mpepping/neovim/pulls/1":
			writeJSON(t, writer, map[string]any{"number": 1, "node_id": "PR_node"})
		case "POST /graphql":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"enablePullRequestAutoMerge": map[string]any{}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	pull := PullRequest{Owner: "mpepping", Repo: "neovim", Number: 1, Author: "MPepping"}
	outcome, err := testClient(t, server).ApproveAndMerge(context.Background(), pull)
	if err != nil {
		t.Fatalf("ApproveAndMerge() error = %v", err)
	}
	if outcome != MergeOutcomeScheduled {
		t.Fatalf("ApproveAndMerge() = %q, want %q", outcome, MergeOutcomeScheduled)
	}
	if got, want := strings.Join(requests, ", "), "GET /user, GET /repos/mpepping/neovim/pulls/1, POST /graphql"; got != want {
		t.Fatalf("requests = %q, want %q", got, want)
	}
}

func TestApproveAndMergeApprovesPullRequestsFromOtherAuthors(t *testing.T) {
	t.Parallel()

	var approved bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /user":
			writeJSON(t, writer, map[string]any{"login": "mpepping"})
		case "POST /repos/mpepping/neovim/pulls/2/reviews":
			approved = true
			assertJSONField(t, request, "event", "APPROVE")
			writeJSON(t, writer, map[string]any{"id": 1})
		case "GET /repos/mpepping/neovim/pulls/2":
			writeJSON(t, writer, map[string]any{"number": 2, "node_id": "PR_node"})
		case "POST /graphql":
			writeJSON(t, writer, map[string]any{"data": map[string]any{"enablePullRequestAutoMerge": map[string]any{}}})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	pull := PullRequest{Owner: "mpepping", Repo: "neovim", Number: 2, Author: "dependabot[bot]"}
	if _, err := testClient(t, server).ApproveAndMerge(context.Background(), pull); err != nil {
		t.Fatalf("ApproveAndMerge() error = %v", err)
	}
	if !approved {
		t.Fatal("pull request from another author was not approved")
	}
}

func TestRequestChangesRejectsSelfAuthoredPullRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method+" "+request.URL.Path == "GET /user" {
			writeJSON(t, writer, map[string]any{"login": "mpepping"})
			return
		}
		t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
		http.NotFound(writer, request)
	}))
	defer server.Close()

	pull := PullRequest{Owner: "mpepping", Repo: "neovim", Number: 1, Author: "mpepping"}
	err := testClient(t, server).RequestChanges(context.Background(), pull, "Please add tests")
	if err == nil || !strings.Contains(err.Error(), "your own pull request") {
		t.Fatalf("RequestChanges() error = %v, want self-review error", err)
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

func testClient(t *testing.T, server *httptest.Server, options ...Option) *Client {
	t.Helper()
	baseURL, err := url.Parse(server.URL + "/")
	if err != nil {
		t.Fatalf("parse test server URL: %v", err)
	}
	client := github.NewClient(server.Client())
	client.BaseURL = baseURL
	client.UploadURL = baseURL
	return NewClientWithGitHub(client, options...)
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
