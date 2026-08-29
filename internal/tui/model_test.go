package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mpepping/ghpr/internal/githubapi"
)

type fakeService struct {
	merged       []string
	approved     []string
	closed       []string
	reviews      []string
	comments     []string
	updated      []string
	diffRequests []string
	diffs        map[string]string
	states       map[string]githubapi.PullRequestState
	statesErr    error
	fail         map[string]error

	listPulls []githubapi.PullRequest
	listErr   error
	listCalls int
	lastQuery githubapi.SearchOptions
}

func (f *fakeService) ListOpenPullRequests(_ context.Context, options githubapi.SearchOptions) ([]githubapi.PullRequest, error) {
	f.listCalls++
	f.lastQuery = options
	return f.listPulls, f.listErr
}

func (f *fakeService) PullRequestStates(_ context.Context, pulls []githubapi.PullRequest) (map[string]githubapi.PullRequestState, error) {
	result := make(map[string]githubapi.PullRequestState, len(pulls))
	for _, pr := range pulls {
		if state, ok := f.states[pr.Key()]; ok {
			result[pr.Key()] = state
		} else {
			result[pr.Key()] = githubapi.PullRequestState{Build: githubapi.BuildStatusNone, Mergeable: githubapi.MergeableUnknown}
		}
	}
	return result, f.statesErr
}

func (f *fakeService) Diff(_ context.Context, pr githubapi.PullRequest) (string, error) {
	f.diffRequests = append(f.diffRequests, pr.Key())
	if err := f.fail[pr.Key()]; err != nil {
		return "", err
	}
	return f.diffs[pr.Key()], nil
}

func (f *fakeService) Approve(_ context.Context, pr githubapi.PullRequest) error {
	f.approved = append(f.approved, pr.Key())
	return f.fail[pr.Key()]
}

func (f *fakeService) ApproveAndMerge(_ context.Context, pr githubapi.PullRequest) (githubapi.MergeOutcome, error) {
	f.merged = append(f.merged, pr.Key())
	if err := f.fail[pr.Key()]; err != nil {
		return "", err
	}
	return githubapi.MergeOutcomeMerged, nil
}

func (f *fakeService) RequestChanges(_ context.Context, pr githubapi.PullRequest, _ string) error {
	f.reviews = append(f.reviews, pr.Key())
	return f.fail[pr.Key()]
}

func (f *fakeService) Comment(_ context.Context, pr githubapi.PullRequest, body string) error {
	f.comments = append(f.comments, pr.Key()+":"+body)
	return f.fail[pr.Key()]
}

func (f *fakeService) UpdateBranch(_ context.Context, pr githubapi.PullRequest) error {
	f.updated = append(f.updated, pr.Key())
	return f.fail[pr.Key()]
}

func (f *fakeService) Close(_ context.Context, pr githubapi.PullRequest, _ string) error {
	f.closed = append(f.closed, pr.Key())
	return f.fail[pr.Key()]
}

func TestInitialLoadHappensInsideTheUI(t *testing.T) {
	t.Parallel()

	service := &fakeService{listPulls: samplePulls()}
	model := New(Options{Context: context.Background(), Service: service, Search: githubapi.SearchOptions{Owner: "acme", Limit: 100}})

	if model.mode != modeLoading {
		t.Fatalf("mode = %v, want loading", model.mode)
	}
	if !strings.Contains(model.View(), "Loading open pull requests") {
		t.Fatalf("loading view is missing the spinner message:\n%s", model.View())
	}

	// Init must kick off the fetch rather than blocking the caller.
	model = updateModel(t, model, pullsLoadedMsg{pulls: service.listPulls, initial: true})
	if model.mode != modeList || len(model.pulls) != 2 {
		t.Fatalf("mode=%v pulls=%d after load", model.mode, len(model.pulls))
	}
}

func TestInitialLoadFailureIsShown(t *testing.T) {
	t.Parallel()

	model := New(Options{Context: context.Background(), Service: &fakeService{}})
	model = updateModel(t, model, pullsLoadedMsg{err: errors.New("bad credentials"), initial: true})
	if model.mode != modeList || !strings.Contains(model.status, "bad credentials") {
		t.Fatalf("mode=%v status=%q", model.mode, model.status)
	}
}

func TestModelSelectsMultiplePullRequests(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model = updateModel(t, model, key(" "))
	model = updateModel(t, model, key("down"))
	model = updateModel(t, model, key(" "))

	if len(model.selected) != 2 {
		t.Fatalf("selected count = %d, want 2", len(model.selected))
	}
	model = updateModel(t, model, key("m"))
	if model.mode != modeConfirm || model.pending != actionMerge {
		t.Fatalf("mode/action = %v/%v, want confirm/merge", model.mode, model.pending)
	}

	updated, command := model.Update(key("y"))
	model = updated.(Model)
	if model.mode != modeRunning || command == nil {
		t.Fatalf("confirmation did not start action: mode=%v command=%v", model.mode, command)
	}
}

func TestFinishBatchRemovesSuccessAndKeepsFailure(t *testing.T) {
	t.Parallel()

	service := &fakeService{fail: map[string]error{"acme/two#2": errors.New("permission denied")}}
	model := newTestModel(service)
	for _, pr := range model.pulls {
		model.selected[pr.Key()] = true
	}

	model, finished := runBatchToCompletion(t, model, actionClose, "obsolete")
	model = model.finishBatch(finished)

	if len(service.closed) != 2 {
		t.Fatalf("Close() calls = %d, want 2", len(service.closed))
	}
	if len(model.pulls) != 1 || model.pulls[0].Key() != "acme/two#2" {
		t.Fatalf("remaining pulls = %#v, want failed pull only", model.pulls)
	}
	if len(model.visible) != 1 || model.visible[0].Key() != "acme/two#2" {
		t.Fatalf("visible pulls = %#v, want the filtered view to follow", model.visible)
	}
	if !model.selected["acme/two#2"] {
		t.Fatal("failed pull request should remain selected")
	}
	if !strings.Contains(model.status, "1 closed, 1 failed") {
		t.Fatalf("status = %q", model.status)
	}
}

func TestBatchReportsPerItemProgress(t *testing.T) {
	t.Parallel()

	service := &fakeService{fail: map[string]error{"acme/two#2": errors.New("boom")}}
	model := newTestModel(service)
	for _, pr := range model.pulls {
		model.selected[pr.Key()] = true
	}
	model = updateModel(t, model, key("m"))

	updated, command := model.Update(key("y"))
	model = updated.(Model)
	if model.mode != modeRunning || model.batchTotal != 2 || command == nil {
		t.Fatalf("batch did not start: mode=%v total=%d command=%v", model.mode, model.batchTotal, command)
	}

	var sawStart, sawPartial bool
	for step := 0; step < 10; step++ {
		message := <-model.batchEvents
		if _, ok := message.(batchItemStartedMsg); ok {
			sawStart = true
		}
		model = updateModel(t, model, message)
		if model.batchDone == 1 && model.mode == modeRunning {
			sawPartial = true
			if !strings.Contains(model.View(), "1/2") {
				t.Fatalf("progress view does not show 1/2:\n%s", model.View())
			}
		}
		if model.mode == modeList {
			break
		}
	}

	if !sawStart {
		t.Error("no per-item start event was delivered")
	}
	if !sawPartial {
		t.Error("intermediate progress was never visible")
	}
	if !strings.Contains(model.status, "1 merged, 1 failed") {
		t.Fatalf("final status = %q", model.status)
	}
}

// Every result must reach the session log, because the status line only has
// room for a summary.
func TestSessionLogRecordsEveryResult(t *testing.T) {
	t.Parallel()

	service := &fakeService{fail: map[string]error{"acme/two#2": errors.New("permission denied")}}
	model := newTestModel(service)
	for _, pr := range model.pulls {
		model.selected[pr.Key()] = true
	}
	model = updateModel(t, model, key("m"))
	model = updateModel(t, model, key("y"))
	for step := 0; step < 10 && model.mode == modeRunning; step++ {
		model = updateModel(t, model, <-model.batchEvents)
	}

	if len(model.log) != 2 {
		t.Fatalf("log has %d entries, want 2", len(model.log))
	}
	model = updateModel(t, model, key("L"))
	if model.mode != modeLog {
		t.Fatalf("mode = %v, want log", model.mode)
	}
	view := model.View()
	if !strings.Contains(view, "acme/one#1 merged") || !strings.Contains(view, "permission denied") {
		t.Fatalf("log view is missing entries:\n%s", view)
	}

	model = updateModel(t, model, key("esc"))
	if model.mode != modeList {
		t.Fatalf("esc did not leave the log: %v", model.mode)
	}
}

func TestApproveOnlyAndCommentAndUpdateBranchActions(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		key      string
		body     string
		expected func(*fakeService) []string
	}{
		{"approve", "A", "", func(f *fakeService) []string { return f.approved }},
		{"comment", "C", "ping", func(f *fakeService) []string { return f.comments }},
		{"update branch", "u", "", func(f *fakeService) []string { return f.updated }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			service := &fakeService{}
			model := newTestModel(service)
			model.selected["acme/one#1"] = true

			model = updateModel(t, model, key(testCase.key))
			if model.mode == modeComment {
				model = typeString(t, model, testCase.body)
				model = updateModel(t, model, key("ctrl+d"))
			}
			if model.mode != modeConfirm {
				t.Fatalf("mode = %v, want confirm", model.mode)
			}
			model = updateModel(t, model, key("y"))
			for step := 0; step < 6 && model.mode == modeRunning; step++ {
				model = updateModel(t, model, <-model.batchEvents)
			}

			if got := testCase.expected(service); len(got) != 1 {
				t.Fatalf("service calls = %v, want exactly one", got)
			}
		})
	}
}

// Approve and comment must not remove pull requests from the list: they are
// still open afterwards.
func TestNonRemovingActionsKeepPullRequests(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model.selected["acme/one#1"] = true
	model = updateModel(t, model, key("A"))
	model = updateModel(t, model, key("y"))
	for step := 0; step < 6 && model.mode == modeRunning; step++ {
		model = updateModel(t, model, <-model.batchEvents)
	}

	if len(model.pulls) != 2 {
		t.Fatalf("pulls = %d, want both to remain after approving", len(model.pulls))
	}
	if !strings.Contains(model.status, "1 approved") {
		t.Fatalf("status = %q", model.status)
	}
}

func TestCommentRequiresBody(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model.selected["acme/one#1"] = true
	model = updateModel(t, model, key("C"))
	if model.mode != modeComment {
		t.Fatalf("mode = %v, want comment", model.mode)
	}
	model = updateModel(t, model, key("ctrl+d"))
	if model.mode != modeComment || !strings.Contains(model.status, "required") {
		t.Fatalf("empty body should be rejected; mode=%v status=%q", model.mode, model.status)
	}
}

func TestRequestChangesRequiresReason(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model.selected[model.pulls[0].Key()] = true
	model = updateModel(t, model, key("r"))
	if model.mode != modeComment {
		t.Fatalf("mode = %v, want comment", model.mode)
	}
	model = updateModel(t, model, key("ctrl+d"))
	if model.mode != modeComment || !strings.Contains(model.status, "required") {
		t.Fatalf("empty reason should remain in comment mode; mode=%v status=%q", model.mode, model.status)
	}
}

// The message box is multi-line, so enter must insert a newline rather than
// submitting the form.
func TestCommentBoxAcceptsNewlines(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model.selected["acme/one#1"] = true
	model = updateModel(t, model, key("C"))
	model = typeString(t, model, "first")
	model = updateModel(t, model, key("enter"))
	model = typeString(t, model, "second")

	if model.mode != modeComment {
		t.Fatalf("enter left comment mode: %v", model.mode)
	}
	if got := model.input.Value(); !strings.Contains(got, "\n") {
		t.Fatalf("comment value = %q, want a newline", got)
	}

	model = updateModel(t, model, key("ctrl+d"))
	if model.mode != modeConfirm || !strings.Contains(model.comment, "first\nsecond") {
		t.Fatalf("mode=%v comment=%q", model.mode, model.comment)
	}
}

// The confirmation must name what is about to change, not just count it.
func TestConfirmationListsPullRequestsAndWarns(t *testing.T) {
	t.Parallel()

	service := &fakeService{states: map[string]githubapi.PullRequestState{
		"acme/two#2": {Build: githubapi.BuildStatusFailure, Mergeable: githubapi.MergeableConflicting},
	}}
	model := newTestModel(service)
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 120, Height: 30})
	model = updateModel(t, model, model.Init2StateCommand(t))
	model = updateModel(t, model, key("a"))
	model = updateModel(t, model, key("m"))

	view := model.View()
	if !strings.Contains(view, "acme/one#1") || !strings.Contains(view, "acme/two#2") {
		t.Fatalf("confirmation does not list the pull requests:\n%s", view)
	}
	if !strings.Contains(view, "squash-merge") {
		t.Fatalf("confirmation does not name the merge method:\n%s", view)
	}
	if !strings.Contains(view, "failing checks, conflicts or requested changes") {
		t.Fatalf("confirmation does not warn about blocked pull requests:\n%s", view)
	}
}

func TestConfirmationNamesTheConfiguredMergeMethod(t *testing.T) {
	t.Parallel()

	model := New(Options{
		Context:     context.Background(),
		Service:     &fakeService{},
		MergeMethod: githubapi.MergeMethodRebase,
	})
	model = updateModel(t, model, pullsLoadedMsg{pulls: samplePulls(), initial: true})
	model.selected["acme/one#1"] = true
	model = updateModel(t, model, key("m"))

	if !strings.Contains(model.View(), "rebase-merge") {
		t.Fatalf("confirmation should name the rebase method:\n%s", model.View())
	}
}

func TestDryRunIsAdvertisedAndKeepsPullRequests(t *testing.T) {
	t.Parallel()

	model := New(Options{Context: context.Background(), Service: &fakeService{}, DryRun: true})
	model = updateModel(t, model, pullsLoadedMsg{pulls: samplePulls(), initial: true})
	if !strings.Contains(model.View(), "DRY RUN") {
		t.Fatalf("header should advertise dry run:\n%s", model.View())
	}

	model.selected["acme/one#1"] = true
	model = updateModel(t, model, key("m"))
	if !strings.Contains(model.View(), "dry run") {
		t.Fatalf("confirmation should mention dry run:\n%s", model.View())
	}

	model = updateModel(t, model, key("y"))
	for step := 0; step < 6 && model.mode == modeRunning; step++ {
		model = updateModel(t, model, <-model.batchEvents)
	}
	// Nothing really happened, so the pull request must stay in the list.
	if len(model.pulls) != 2 {
		t.Fatalf("dry run removed pull requests: %d remain", len(model.pulls))
	}
}

func TestFilterNarrowsListAndSelectAllRespectsIt(t *testing.T) {
	t.Parallel()

	model := newTestModelWith(&fakeService{}, []githubapi.PullRequest{
		{Owner: "acme", Repo: "one", Number: 1, Title: "Bump go", Author: "dependabot[bot]", URL: "https://example.test/1"},
		{Owner: "acme", Repo: "two", Number: 2, Title: "Add feature", Author: "martijn", URL: "https://example.test/2"},
		{Owner: "acme", Repo: "three", Number: 3, Title: "Bump node", Author: "dependabot[bot]", URL: "https://example.test/3"},
	})

	model = updateModel(t, model, key("/"))
	if model.mode != modeFilter {
		t.Fatalf("mode = %v, want filter", model.mode)
	}
	model = typeString(t, model, "dependabot")
	if len(model.visible) != 2 {
		t.Fatalf("visible = %d, want 2 dependabot pull requests", len(model.visible))
	}

	model = updateModel(t, model, key("enter"))
	if model.mode != modeList || model.filter != "dependabot" {
		t.Fatalf("mode=%v filter=%q after enter", model.mode, model.filter)
	}

	model = updateModel(t, model, key("a"))
	if len(model.selected) != 2 {
		t.Fatalf("selected = %d, want only the 2 visible rows", len(model.selected))
	}
	if model.selected["acme/two#2"] {
		t.Error("a filtered-out pull request was selected")
	}

	model = updateModel(t, model, key("esc"))
	if model.filter != "" || len(model.visible) != 3 {
		t.Fatalf("esc did not clear the filter: filter=%q visible=%d", model.filter, len(model.visible))
	}
	if len(model.selected) != 2 {
		t.Fatalf("selection changed when clearing the filter: %d", len(model.selected))
	}
}

func TestFilterMatchesRepositoryAndNumberAndCancels(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model = updateModel(t, model, key("/"))
	model = typeString(t, model, "two")
	if len(model.visible) != 1 || model.visible[0].Key() != "acme/two#2" {
		t.Fatalf("repository filter failed: %#v", model.visible)
	}

	model = updateModel(t, model, key("esc"))
	if model.filter != "" || len(model.visible) != 2 {
		t.Fatalf("cancel did not restore the previous state: filter=%q visible=%d", model.filter, len(model.visible))
	}

	model = updateModel(t, model, key("/"))
	model = typeString(t, model, "#1")
	if len(model.visible) != 1 || model.visible[0].Key() != "acme/one#1" {
		t.Fatalf("number filter failed: %#v", model.visible)
	}
}

// A filter supplied on the command line must be active from the first frame.
func TestInitialFilterFromOptions(t *testing.T) {
	t.Parallel()

	model := New(Options{Context: context.Background(), Service: &fakeService{}, Filter: "two"})
	model = updateModel(t, model, pullsLoadedMsg{pulls: samplePulls(), initial: true})
	if len(model.visible) != 1 || model.visible[0].Key() != "acme/two#2" {
		t.Fatalf("startup filter was not applied: %#v", model.visible)
	}
}

func TestRefreshReloadsAndKeepsValidSelection(t *testing.T) {
	t.Parallel()

	service := &fakeService{listPulls: []githubapi.PullRequest{
		{Owner: "acme", Repo: "two", Number: 2, Title: "Second", URL: "https://example.test/2"},
		{Owner: "acme", Repo: "new", Number: 9, Title: "Fresh", URL: "https://example.test/9"},
	}}
	model := newTestModel(service)
	model.selected["acme/one#1"] = true
	model.selected["acme/two#2"] = true

	updated, command := model.Update(key("R"))
	model = updated.(Model)
	if !model.refreshing || command == nil {
		t.Fatalf("refresh did not start: refreshing=%t command=%v", model.refreshing, command)
	}

	model = updateModel(t, model, pullsLoadedMsg{pulls: service.listPulls})
	if model.refreshing {
		t.Error("refresh flag was not cleared")
	}
	if len(model.pulls) != 2 || model.pulls[1].Key() != "acme/new#9" {
		t.Fatalf("pulls after refresh = %#v", model.pulls)
	}
	if len(model.selected) != 1 || !model.selected["acme/two#2"] {
		t.Fatalf("selection after refresh = %#v, want only acme/two#2", model.selected)
	}
	if !strings.Contains(model.status, "Refreshed") {
		t.Fatalf("status = %q", model.status)
	}
}

func TestStaleStateResultsAreDiscardedAfterRefresh(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	stale := statesLoadedMsg{
		generation: model.generation,
		keys:       []string{"acme/one#1"},
		states:     map[string]githubapi.PullRequestState{"acme/one#1": {Build: githubapi.BuildStatusSuccess}},
	}

	model = updateModel(t, model, pullsLoadedMsg{pulls: model.pulls})
	model = updateModel(t, model, stale)

	if _, ok := model.states["acme/one#1"]; ok {
		t.Fatal("stale state result was applied after a refresh")
	}
}

func TestRefreshDuringBatchDoesNotOrphanIt(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model.selected["acme/one#1"] = true
	model = updateModel(t, model, key("m"))
	model = updateModel(t, model, key("y"))
	if model.mode != modeRunning {
		t.Fatalf("mode = %v, want running", model.mode)
	}

	model = updateModel(t, model, pullsLoadedMsg{pulls: model.pulls})

	for step := 0; step < 10 && model.mode == modeRunning; step++ {
		model = updateModel(t, model, <-model.batchEvents)
	}
	if model.mode == modeRunning {
		t.Fatal("batch events were discarded, UI stayed in running mode")
	}
	if !strings.Contains(model.status, "1 merged") {
		t.Fatalf("status = %q, want the batch result", model.status)
	}
}

func TestRefreshFailureIsReported(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model = updateModel(t, model, pullsLoadedMsg{err: errors.New("rate limited")})
	if model.refreshing {
		t.Error("refresh flag was not cleared after failure")
	}
	if !strings.Contains(model.status, "Unable to refresh") || !strings.Contains(model.status, "rate limited") {
		t.Fatalf("status = %q", model.status)
	}
	if len(model.pulls) != 2 {
		t.Fatalf("failed refresh must keep the existing list, got %d", len(model.pulls))
	}
}

// Merged pull requests must leave the pending state queue, otherwise ghpr
// keeps querying GitHub for rows that are gone.
func TestPruneQueueDropsFinishedPullRequests(t *testing.T) {
	t.Parallel()

	queue := samplePulls()
	queue = append(queue, githubapi.PullRequest{Owner: "acme", Repo: "three", Number: 3})
	next := 2
	pruned := pruneQueue(queue, map[string]bool{"acme/one#1": true}, &next)

	if len(pruned) != 2 || pruned[0].Key() != "acme/two#2" {
		t.Fatalf("pruned = %#v", pruned)
	}
	if next != 1 {
		t.Fatalf("cursor = %d, want 1 after removing an already-processed entry", next)
	}
}

func TestOpenHighlightedPullRequestInBrowser(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	var openedURL string
	model.openURL = func(target string) error {
		openedURL = target
		return nil
	}
	model = updateModel(t, model, key("down"))

	updated, command := model.Update(key("w"))
	model = updated.(Model)
	if command == nil || !strings.Contains(model.status, "Opening acme/two#2") {
		t.Fatalf("browser action did not start: command=%v status=%q", command, model.status)
	}

	model = updateModel(t, model, command())
	if openedURL != "https://example.test/2" {
		t.Fatalf("opened URL = %q, want highlighted PR URL", openedURL)
	}
	if !strings.Contains(model.status, "Opened acme/two#2") {
		t.Fatalf("status = %q, want success status", model.status)
	}
}

func TestStateColumnsLoadAsynchronously(t *testing.T) {
	t.Parallel()

	service := &fakeService{states: map[string]githubapi.PullRequestState{
		"acme/one#1": {Build: githubapi.BuildStatusSuccess, Mergeable: githubapi.MergeableClean, Review: githubapi.ReviewDecisionApproved},
		"acme/two#2": {Build: githubapi.BuildStatusFailure, Mergeable: githubapi.MergeableConflicting},
	}}
	model := newTestModel(service)
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 120, Height: 20})
	model = updateModel(t, model, model.Init2StateCommand(t))

	if got := model.states["acme/one#1"].Build; got != githubapi.BuildStatusSuccess {
		t.Fatalf("build status for one#1 = %q, want success", got)
	}
	if got := model.states["acme/two#2"].Mergeable; got != githubapi.MergeableConflicting {
		t.Fatalf("mergeable for two#2 = %q, want conflicting", got)
	}

	view := model.View()
	for _, want := range []string{"✓", "✗", "⚠", "AGE", "AUTHOR", "CI", "RV"} {
		if !strings.Contains(view, want) {
			t.Fatalf("view is missing %q:\n%s", want, view)
		}
	}
	if !strings.Contains(view, "CI: success · review: approved") {
		t.Fatalf("footer should summarise the highlighted PR, got:\n%s", view)
	}
}

func TestPartialStateFailureKeepsResultsAndWarns(t *testing.T) {
	t.Parallel()

	service := &fakeService{
		states:    map[string]githubapi.PullRequestState{"acme/one#1": {Build: githubapi.BuildStatusSuccess}},
		statesErr: errors.New("Resource not accessible"),
	}
	model := newTestModel(service)
	model = updateModel(t, model, model.Init2StateCommand(t))

	if got := model.states["acme/one#1"].Build; got != githubapi.BuildStatusSuccess {
		t.Fatalf("usable state was discarded: %q", got)
	}
	if model.stateWarning == "" {
		t.Fatal("partial failure was not surfaced to the user")
	}
	if !strings.Contains(model.View(), "CI/review data incomplete") {
		t.Fatalf("warning is not rendered:\n%s", model.View())
	}
	if len(model.log) == 0 {
		t.Fatal("state failures should also reach the session log")
	}
}

func TestNarrowTerminalDropsOptionalColumns(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 60, Height: 20})
	view := model.View()
	if strings.Contains(view, "AUTHOR") || strings.Contains(view, "AGE") {
		t.Fatalf("narrow terminal should drop optional columns:\n%s", view)
	}
	if !strings.Contains(view, "REPOSITORY") {
		t.Fatalf("core columns are missing:\n%s", view)
	}
}

func TestHelpOverlay(t *testing.T) {
	t.Parallel()

	// The overlay scrolls, so check the content itself for completeness.
	content := helpContent()
	for _, want := range []string{"Navigation", "approve and merge", "approve only", "Diff viewer", "next or previous file", "Columns"} {
		if !strings.Contains(content, want) {
			t.Fatalf("help content is missing %q", want)
		}
	}

	model := newTestModel(&fakeService{})
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 100, Height: 30})
	model = updateModel(t, model, key("?"))
	if model.mode != modeHelp {
		t.Fatalf("mode = %v, want help", model.mode)
	}
	if !strings.Contains(model.View(), "Navigation") {
		t.Fatalf("help overlay does not render:\n%s", model.View())
	}

	model = updateModel(t, model, key("q"))
	if model.mode != modeList {
		t.Fatalf("q did not close the help overlay: %v", model.mode)
	}
}

// Every documented key must actually exist in the help text, so the overlay
// cannot drift away from the implementation.
func TestHelpCoversEveryListAction(t *testing.T) {
	t.Parallel()

	content := helpContent()
	for _, binding := range []string{"m", "A", "c", "r", "C", "u", "d", "w", "L", "?", "/", "a"} {
		if !strings.Contains(content, "  "+binding+" ") {
			t.Errorf("help does not document the %q key", binding)
		}
	}
}

func TestFormatAge(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		updated time.Time
		want    string
	}{
		{time.Time{}, "-"},
		{now.Add(-30 * time.Second), "now"},
		{now.Add(-5 * time.Minute), "5m"},
		{now.Add(-3 * time.Hour), "3h"},
		{now.Add(-50 * time.Hour), "2d"},
		{now.Add(-20 * 24 * time.Hour), "2w"},
		{now.Add(-800 * 24 * time.Hour), "2y"},
	}
	for _, testCase := range cases {
		if got := formatAge(testCase.updated, now); got != testCase.want {
			t.Errorf("formatAge(%v) = %q, want %q", testCase.updated, got, testCase.want)
		}
	}
}

// Column alignment must be based on display cells, not rune counts: CJK text
// and emoji occupy two cells each.
func TestClampAndPadUseDisplayWidth(t *testing.T) {
	t.Parallel()

	cases := []struct {
		value string
		width int
	}{
		{"日本語のタイトルです", 8},
		{"🎉 celebrate the release", 10},
		{"plain ascii text", 7},
	}
	for _, testCase := range cases {
		got := clamp(testCase.value, testCase.width)
		if width := displayWidth(got); width > testCase.width {
			t.Errorf("clamp(%q, %d) = %q with width %d", testCase.value, testCase.width, got, width)
		}
		if padded := padRight(got, testCase.width); displayWidth(padded) != testCase.width {
			t.Errorf("padRight(%q, %d) has width %d", got, testCase.width, displayWidth(padded))
		}
	}

	if got := clamp("short", 40); got != "short" {
		t.Errorf("clamp must leave short strings untouched, got %q", got)
	}
	if got := clamp("anything", 0); got != "anything" {
		t.Errorf("width 0 means unbounded, got %q", got)
	}
}

// Columns must line up even when a repository name contains wide characters.
func TestRowColumnsAlignWithWideCharacters(t *testing.T) {
	t.Parallel()

	model := newTestModelWith(&fakeService{}, []githubapi.PullRequest{
		{Owner: "acme", Repo: "one", Number: 1, Title: "ascii title", Author: "alice", URL: "u"},
		{Owner: "acme", Repo: "日本語リポジトリ", Number: 2, Title: "日本語のタイトル", Author: "bob", URL: "u"},
		{Owner: "acme", Repo: "emoji", Number: 3, Title: "🎉 release", Author: "carol", URL: "u"},
	})
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 120, Height: 20})

	// The pull request number starts each row's second column; if the
	// repository column is padded by display width it lands at the same cell.
	columnStart := func(index int) int {
		row := model.renderRow(index)
		at := strings.Index(row, "#")
		if at < 0 {
			t.Fatalf("row %d has no number column: %s", index, row)
		}
		return displayWidth(row[:at])
	}

	want := columnStart(0)
	for index := 1; index < 3; index++ {
		if got := columnStart(index); got != want {
			t.Fatalf("row %d starts its number column at cell %d, want %d\n%s\n%s",
				index, got, want, model.renderRow(0), model.renderRow(index))
		}
	}
}

func newTestModel(service GitHubService) Model {
	return newTestModelWith(service, samplePulls())
}

func newTestModelWith(service GitHubService, pulls []githubapi.PullRequest) Model {
	model := New(Options{
		Context: context.Background(),
		Service: service,
		Search:  githubapi.SearchOptions{Owner: "acme", Limit: 100},
	})
	model.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }
	updated, _ := model.Update(pullsLoadedMsg{pulls: pulls, initial: true})
	return updated.(Model)
}

func samplePulls() []githubapi.PullRequest {
	return []githubapi.PullRequest{
		{Owner: "acme", Repo: "one", Number: 1, Title: "First", Author: "alice", URL: "https://example.test/1"},
		{Owner: "acme", Repo: "two", Number: 2, Title: "Second", Author: "bob", URL: "https://example.test/2"},
	}
}

// Init2StateCommand runs the pending state loader and returns its message.
func (m Model) Init2StateCommand(t *testing.T) tea.Msg {
	t.Helper()
	command := m.loadNextStateBatch()
	if command == nil {
		t.Fatal("no state loader command was scheduled")
	}
	return command()
}

func runBatchToCompletion(t *testing.T, model Model, selected action, comment string) (Model, batchFinishedMsg) {
	t.Helper()

	events := make(chan tea.Msg, 64)
	go model.runBatch(context.Background(), events, selected, model.selectedPulls(), comment, model.batchGeneration)

	for message := range events {
		if finished, ok := message.(batchFinishedMsg); ok {
			return model, finished
		}
	}
	t.Fatal("batch never reported completion")
	return model, batchFinishedMsg{}
}

func updateModel(t *testing.T, model Model, message tea.Msg) Model {
	t.Helper()
	updated, _ := model.Update(message)
	return updated.(Model)
}

func typeString(t *testing.T, model Model, value string) Model {
	t.Helper()
	for _, character := range value {
		model = updateModel(t, model, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{character}})
	}
	return model
}

func key(value string) tea.KeyMsg {
	switch value {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+e":
		return tea.KeyMsg{Type: tea.KeyCtrlE}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
	}
}
