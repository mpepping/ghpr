package githubapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-github/v81/github"
)

func TestParseMergeMethod(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]MergeMethod{
		"squash":   MergeMethodSquash,
		"MERGE":    MergeMethodMerge,
		" rebase ": MergeMethodRebase,
	} {
		got, err := ParseMergeMethod(input)
		if err != nil || got != want {
			t.Errorf("ParseMergeMethod(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseMergeMethod("fast-forward"); err == nil {
		t.Error("ParseMergeMethod accepted an unknown method")
	}
	if got := MergeMethodRebase.GraphQL(); got != "REBASE" {
		t.Errorf("GraphQL() = %q, want REBASE", got)
	}
}

// The configured method must reach both the auto-merge mutation and the direct
// merge fallback.
func TestApproveAndMergeUsesConfiguredMergeMethod(t *testing.T) {
	t.Parallel()

	var graphQLMethod, restMethod string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "POST /repos/acme/widgets/pulls/5/reviews":
			writeJSON(t, writer, map[string]any{"id": 1})
		case "GET /repos/acme/widgets/pulls/5":
			writeJSON(t, writer, map[string]any{"number": 5, "node_id": "PR_node"})
		case "POST /graphql":
			var body graphQLRequest
			decodeJSON(t, request, &body)
			graphQLMethod, _ = body.Variables["mergeMethod"].(string)
			writeJSON(t, writer, map[string]any{"errors": []map[string]any{{"message": "auto-merge disabled"}}})
		case "PUT /repos/acme/widgets/pulls/5/merge":
			var body map[string]any
			decodeJSON(t, request, &body)
			restMethod, _ = body["merge_method"].(string)
			writeJSON(t, writer, map[string]any{"merged": true})
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testClient(t, server, WithMergeMethod(MergeMethodRebase))
	if _, err := client.ApproveAndMerge(context.Background(), PullRequest{Owner: "acme", Repo: "widgets", Number: 5}); err != nil {
		t.Fatalf("ApproveAndMerge() error = %v", err)
	}
	if graphQLMethod != "REBASE" {
		t.Errorf("auto-merge used %q, want REBASE", graphQLMethod)
	}
	if restMethod != "rebase" {
		t.Errorf("direct merge used %q, want rebase", restMethod)
	}
}

func TestApproveAndMergeDeletesBranchAfterDirectMerge(t *testing.T) {
	t.Parallel()

	var deleted string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/repos/acme/widgets/pulls/6/reviews":
			writeJSON(t, writer, map[string]any{"id": 1})
		case request.URL.Path == "/repos/acme/widgets/pulls/6":
			writeJSON(t, writer, map[string]any{
				"number":  6,
				"node_id": "PR_node",
				"head": map[string]any{
					"ref":  "feature-branch",
					"repo": map[string]any{"full_name": "acme/widgets"},
				},
			})
		case request.URL.Path == "/graphql":
			writeJSON(t, writer, map[string]any{"errors": []map[string]any{{"message": "no"}}})
		case request.URL.Path == "/repos/acme/widgets/pulls/6/merge":
			writeJSON(t, writer, map[string]any{"merged": true})
		case request.Method == http.MethodDelete && strings.HasPrefix(request.URL.Path, "/repos/acme/widgets/git/refs/"):
			deleted = strings.TrimPrefix(request.URL.Path, "/repos/acme/widgets/git/refs/")
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testClient(t, server, WithDeleteBranch(true))
	outcome, err := client.ApproveAndMerge(context.Background(), PullRequest{Owner: "acme", Repo: "widgets", Number: 6})
	if err != nil {
		t.Fatalf("ApproveAndMerge() error = %v", err)
	}
	if outcome != MergeOutcomeMerged {
		t.Fatalf("outcome = %q", outcome)
	}
	if deleted != "heads/feature-branch" {
		t.Fatalf("deleted ref = %q, want heads/feature-branch", deleted)
	}
}

// Deleting a branch that lives in a fork would touch someone else's repository.
func TestDeleteBranchSkipsForks(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/repos/acme/widgets/pulls/7/reviews":
			writeJSON(t, writer, map[string]any{"id": 1})
		case request.URL.Path == "/repos/acme/widgets/pulls/7":
			writeJSON(t, writer, map[string]any{
				"number":  7,
				"node_id": "PR_node",
				"head": map[string]any{
					"ref":  "patch-1",
					"repo": map[string]any{"full_name": "contributor/widgets"},
				},
			})
		case request.URL.Path == "/graphql":
			writeJSON(t, writer, map[string]any{"errors": []map[string]any{{"message": "no"}}})
		case request.URL.Path == "/repos/acme/widgets/pulls/7/merge":
			writeJSON(t, writer, map[string]any{"merged": true})
		case request.Method == http.MethodDelete:
			t.Error("a fork's branch must never be deleted")
			writer.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	client := testClient(t, server, WithDeleteBranch(true))
	_, err := client.ApproveAndMerge(context.Background(), PullRequest{Owner: "acme", Repo: "widgets", Number: 7})
	if err == nil || !strings.Contains(err.Error(), "fork") {
		t.Fatalf("error = %v, want a fork warning", err)
	}
}

func TestApproveOnly(t *testing.T) {
	t.Parallel()

	var approved bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Method + " " + request.URL.Path {
		case "GET /user":
			writeJSON(t, writer, map[string]any{"login": "reviewer"})
		case "POST /repos/acme/widgets/pulls/3/reviews":
			approved = true
			assertJSONField(t, request, "event", "APPROVE")
			writeJSON(t, writer, map[string]any{"id": 1})
		case "PUT /repos/acme/widgets/pulls/3/merge":
			t.Error("approve must not merge")
			http.NotFound(writer, request)
		default:
			http.NotFound(writer, request)
		}
	}))
	defer server.Close()

	pr := PullRequest{Owner: "acme", Repo: "widgets", Number: 3, Author: "someone"}
	if err := testClient(t, server).Approve(context.Background(), pr); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	if !approved {
		t.Fatal("no approving review was submitted")
	}
}

func TestApproveRejectsSelfAuthoredPullRequest(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/user" {
			writeJSON(t, writer, map[string]any{"login": "mpepping"})
			return
		}
		t.Errorf("unexpected request %s", request.URL.Path)
	}))
	defer server.Close()

	pr := PullRequest{Owner: "mpepping", Repo: "ghpr", Number: 1, Author: "mpepping"}
	err := testClient(t, server).Approve(context.Background(), pr)
	if err == nil || !strings.Contains(err.Error(), "your own") {
		t.Fatalf("Approve() error = %v, want a self-review error", err)
	}
}

func TestComment(t *testing.T) {
	t.Parallel()

	var body string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method+" "+request.URL.Path == "POST /repos/acme/widgets/issues/4/comments" {
			var payload map[string]any
			decodeJSON(t, request, &payload)
			body, _ = payload["body"].(string)
			writeJSON(t, writer, map[string]any{"id": 1})
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	client := testClient(t, server)
	pr := PullRequest{Owner: "acme", Repo: "widgets", Number: 4}
	if err := client.Comment(context.Background(), pr, "  looks good  "); err != nil {
		t.Fatalf("Comment() error = %v", err)
	}
	if body != "looks good" {
		t.Fatalf("comment body = %q, want it trimmed", body)
	}
	if err := client.Comment(context.Background(), pr, "   "); err == nil {
		t.Fatal("an empty comment must be rejected")
	}
}

func TestUpdateBranch(t *testing.T) {
	t.Parallel()

	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method+" "+request.URL.Path == "PUT /repos/acme/widgets/pulls/8/update-branch" {
			called = true
			// GitHub answers 202 Accepted for this endpoint.
			writer.WriteHeader(http.StatusAccepted)
			_, _ = writer.Write([]byte(`{"message":"Updating pull request branch."}`))
			return
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	pr := PullRequest{Owner: "acme", Repo: "widgets", Number: 8}
	if err := testClient(t, server).UpdateBranch(context.Background(), pr); err != nil {
		t.Fatalf("UpdateBranch() error = %v, want 202 Accepted to count as success", err)
	}
	if !called {
		t.Fatal("update-branch endpoint was not called")
	}
}

// Dry run must not send a single state-changing request.
func TestDryRunPerformsNoWrites(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet {
			t.Errorf("dry run sent a %s request to %s", request.Method, request.URL.Path)
		}
		http.NotFound(writer, request)
	}))
	defer server.Close()

	client := testClient(t, server, WithDryRun(true))
	pr := PullRequest{Owner: "acme", Repo: "widgets", Number: 9}
	ctx := context.Background()

	outcome, err := client.ApproveAndMerge(ctx, pr)
	if err != nil || outcome != MergeOutcomeDryRun {
		t.Fatalf("ApproveAndMerge() = %q, %v", outcome, err)
	}
	for name, err := range map[string]error{
		"approve":         client.Approve(ctx, pr),
		"close":           client.Close(ctx, pr, "bye"),
		"request changes": client.RequestChanges(ctx, pr, "please fix"),
		"comment":         client.Comment(ctx, pr, "hello"),
		"update branch":   client.UpdateBranch(ctx, pr),
	} {
		if err != nil {
			t.Errorf("dry run %s returned %v", name, err)
		}
	}
	if !client.DryRun() {
		t.Error("DryRun() should report true")
	}
}

// Validation still applies in dry run: a missing reason is a user error.
func TestDryRunStillValidatesInput(t *testing.T) {
	t.Parallel()

	client := NewClientWithGitHub(github.NewClient(nil), WithDryRun(true))
	if err := client.RequestChanges(context.Background(), PullRequest{}, "  "); err == nil {
		t.Error("an empty reason must be rejected even in dry run")
	}
	if err := client.Comment(context.Background(), PullRequest{}, ""); err == nil {
		t.Error("an empty comment must be rejected even in dry run")
	}
}
