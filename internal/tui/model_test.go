package tui

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mpepping/ghpr/internal/githubapi"
)

type fakeService struct {
	merged       []string
	closed       []string
	reviews      []string
	diffRequests []string
	diffs        map[string]string
	fail         map[string]error
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

	message := model.runBatch(context.Background(), actionClose, model.selectedPulls(), "obsolete")().(batchFinishedMsg)
	model = model.finishBatch(message)

	if len(service.closed) != 2 {
		t.Fatalf("Close() calls = %d, want 2", len(service.closed))
	}
	if len(model.pulls) != 1 || model.pulls[0].Key() != "acme/two#2" {
		t.Fatalf("remaining pulls = %#v, want failed pull only", model.pulls)
	}
	if !model.selected["acme/two#2"] {
		t.Fatal("failed pull request should remain selected")
	}
	if !strings.Contains(model.status, "1 closed, 1 failed") || !strings.Contains(model.status, "permission denied") {
		t.Fatalf("status = %q", model.status)
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
	return New(context.Background(), service, "acme", []githubapi.PullRequest{
		{Owner: "acme", Repo: "one", Number: 1, Title: "First", URL: "https://example.test/1"},
		{Owner: "acme", Repo: "two", Number: 2, Title: "Second", URL: "https://example.test/2"},
	})
}

func updateModel(t *testing.T, model Model, message tea.Msg) Model {
	t.Helper()
	updated, _ := model.Update(message)
	return updated.(Model)
}

func key(value string) tea.KeyMsg {
	switch value {
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace}
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgup":
		return tea.KeyMsg{Type: tea.KeyPgUp}
	default:
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(value)}
	}
}
