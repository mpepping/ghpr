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
	closed       []string
	reviews      []string
	diffRequests []string
	diffs        map[string]string
	states       map[string]githubapi.PullRequestState
	statesErr    error
	fail         map[string]error

	listPulls []githubapi.PullRequest
	listErr   error
	listCalls int
}

func (f *fakeService) ListOpenPullRequests(_ context.Context, _ string, _ int) ([]githubapi.PullRequest, error) {
	f.listCalls++
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

func (f *fakeService) Close(_ context.Context, pr githubapi.PullRequest, _ string) error {
	f.closed = append(f.closed, pr.Key())
	return f.fail[pr.Key()]
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
	if !strings.Contains(model.status, "1 closed, 1 failed") || !strings.Contains(model.status, "permission denied") {
		t.Fatalf("status = %q", model.status)
	}
}

// The batch must report every item as it finishes rather than staying silent
// until the whole run is done.
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
	if model.mode != modeRunning || model.batchTotal != 2 {
		t.Fatalf("batch did not start: mode=%v total=%d", model.mode, model.batchTotal)
	}
	if command == nil {
		t.Fatal("no command returned to await batch events")
	}

	// Drain the event stream one message at a time, checking the progress the
	// user would actually see along the way.
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
	if len(model.pulls) != 1 || model.pulls[0].Key() != "acme/two#2" {
		t.Fatalf("remaining pulls = %#v", model.pulls)
	}
}

// A refresh landing while a batch is running must not orphan the batch: its
// events still have to be applied, otherwise the UI would hang in modeRunning.
func TestRefreshDuringBatchDoesNotOrphanIt(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model.selected["acme/one#1"] = true
	model = updateModel(t, model, key("m"))
	model = updateModel(t, model, key("y"))
	if model.mode != modeRunning {
		t.Fatalf("mode = %v, want running", model.mode)
	}

	// An in-flight refresh completes while the batch is still working.
	model = updateModel(t, model, refreshedMsg{pulls: model.pulls})

	for step := 0; step < 10 && model.mode == modeRunning; step++ {
		model = updateModel(t, model, <-model.batchEvents)
	}
	if model.mode != modeRunning && !strings.Contains(model.status, "1 merged") {
		t.Fatalf("status = %q, want the batch result", model.status)
	}
	if model.mode == modeRunning {
		t.Fatal("batch events were discarded, UI stayed in running mode")
	}
}

func TestFilterNarrowsListAndSelectAllRespectsIt(t *testing.T) {
	t.Parallel()

	model := New(context.Background(), &fakeService{}, "acme", 100, []githubapi.PullRequest{
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

	// Select all must apply to the filtered rows only.
	model = updateModel(t, model, key("a"))
	if len(model.selected) != 2 {
		t.Fatalf("selected = %d, want only the 2 visible rows", len(model.selected))
	}
	if model.selected["acme/two#2"] {
		t.Error("a filtered-out pull request was selected")
	}

	// Escape clears the filter but keeps the selection.
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

	// Escape restores the filter that was active before editing.
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

func TestRefreshReloadsAndKeepsValidSelection(t *testing.T) {
	t.Parallel()

	service := &fakeService{listPulls: []githubapi.PullRequest{
		{Owner: "acme", Repo: "two", Number: 2, Title: "Second", URL: "https://example.test/2"},
		{Owner: "acme", Repo: "new", Number: 9, Title: "Fresh", URL: "https://example.test/9"},
	}}
	model := newTestModel(service)
	model.selected["acme/one#1"] = true // disappears after the refresh
	model.selected["acme/two#2"] = true // survives

	updated, command := model.Update(key("R"))
	model = updated.(Model)
	if !model.refreshing || command == nil {
		t.Fatalf("refresh did not start: refreshing=%t command=%v", model.refreshing, command)
	}

	model = updateModel(t, model, refreshedMsg{pulls: service.listPulls})
	if model.refreshing {
		t.Error("refresh flag was not cleared")
	}
	if service.listCalls != 0 {
		// The command itself performs the call; here we injected the result.
		t.Logf("list calls = %d", service.listCalls)
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

// Results from a load that started before a refresh must not overwrite the new
// list's state.
func TestStaleStateResultsAreDiscardedAfterRefresh(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	stale := statesLoadedMsg{
		generation: model.generation,
		keys:       []string{"acme/one#1"},
		states:     map[string]githubapi.PullRequestState{"acme/one#1": {Build: githubapi.BuildStatusSuccess}},
	}

	model = updateModel(t, model, refreshedMsg{pulls: model.pulls})
	model = updateModel(t, model, stale)

	if _, ok := model.states["acme/one#1"]; ok {
		t.Fatal("stale state result was applied after a refresh")
	}
}

func TestRefreshFailureIsReported(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model = updateModel(t, model, refreshedMsg{err: errors.New("rate limited")})
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

func TestDiffViewerLoadsHighlightedPullAndScrolls(t *testing.T) {
	t.Parallel()

	service := &fakeService{diffs: map[string]string{
		"acme/two#2": strings.Join([]string{
			"diff --git a/file.go b/file.go",
			"--- a/file.go",
			"+++ b/file.go",
			"@@ -1,8 +1,8 @@",
			"-old 1", "+new 1", " context 2", " context 3", " context 4",
			" context 5", " context 6", " context 7", " context 8", " context 9",
		}, "\n"),
	}}
	model := newTestModel(service)
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 80, Height: 8})
	model = updateModel(t, model, key("down"))

	updated, command := model.Update(key("d"))
	model = updated.(Model)
	if model.mode != modeDiffLoading || model.diffPR.Key() != "acme/two#2" || command == nil {
		t.Fatalf("diff did not start for highlighted PR: mode=%v pr=%s command=%v", model.mode, model.diffPR.Key(), command)
	}

	message := model.loadDiff(context.Background(), model.diffPR)()
	model = updateModel(t, model, message)
	if model.mode != modeDiff {
		t.Fatalf("mode = %v, want diff", model.mode)
	}
	if len(service.diffRequests) != 1 || service.diffRequests[0] != "acme/two#2" {
		t.Fatalf("diff requests = %v", service.diffRequests)
	}

	model = updateModel(t, model, key(" "))
	pageDownOffset := model.viewport.YOffset
	if pageDownOffset == 0 {
		t.Fatal("space did not page down in diff viewer")
	}
	model = updateModel(t, model, key("pgup"))
	if model.viewport.YOffset >= pageDownOffset {
		t.Fatal("Page Up did not scroll up in diff viewer")
	}
	model = updateModel(t, model, key("q"))
	if model.mode != modeList {
		t.Fatalf("q returned mode %v, want list", model.mode)
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

	command := model.Init()
	if command == nil {
		t.Fatal("Init() returned nil, want state loader")
	}
	model = updateModel(t, model, command())

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

// Partial GraphQL failures must keep the data that did arrive and warn about
// the rest.
func TestPartialStateFailureKeepsResultsAndWarns(t *testing.T) {
	t.Parallel()

	service := &fakeService{
		states:    map[string]githubapi.PullRequestState{"acme/one#1": {Build: githubapi.BuildStatusSuccess}},
		statesErr: errors.New("Resource not accessible"),
	}
	model := newTestModel(service)
	model = updateModel(t, model, model.Init()())

	if got := model.states["acme/one#1"].Build; got != githubapi.BuildStatusSuccess {
		t.Fatalf("usable state was discarded: %q", got)
	}
	if model.stateWarning == "" {
		t.Fatal("partial failure was not surfaced to the user")
	}
	if !strings.Contains(model.View(), "CI/review data incomplete") {
		t.Fatalf("warning is not rendered:\n%s", model.View())
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

func TestRequestChangesRequiresReason(t *testing.T) {
	t.Parallel()

	model := newTestModel(&fakeService{})
	model.selected[model.pulls[0].Key()] = true
	model = updateModel(t, model, key("r"))
	if model.mode != modeComment {
		t.Fatalf("mode = %v, want comment", model.mode)
	}
	model = updateModel(t, model, key("enter"))
	if model.mode != modeComment || !strings.Contains(model.status, "required") {
		t.Fatalf("empty reason should remain in comment mode; mode=%v status=%q", model.mode, model.status)
	}
}

func newTestModel(service GitHubService) Model {
	model := New(context.Background(), service, "acme", 100, []githubapi.PullRequest{
		{Owner: "acme", Repo: "one", Number: 1, Title: "First", Author: "alice", URL: "https://example.test/1"},
		{Owner: "acme", Repo: "two", Number: 2, Title: "Second", Author: "bob", URL: "https://example.test/2"},
	})
	model.now = func() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }
	return model
}

// runBatchToCompletion starts a batch and drains its event stream, returning
// the final message.
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
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
	}
}
