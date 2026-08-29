package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

const sampleDiff = `diff --git a/alpha.go b/alpha.go
--- a/alpha.go
+++ b/alpha.go
@@ -1,4 +1,4 @@
-old alpha
+new alpha
 context one
 context two
diff --git a/beta.go b/beta.go
--- a/beta.go
+++ b/beta.go
@@ -10,3 +10,3 @@
-old beta
+new beta needle
 trailing context
diff --git a/gamma.go b/gamma.go
--- a/gamma.go
+++ b/gamma.go
@@ -1,2 +1,2 @@
-old gamma
+new gamma
`

func TestDiffViewerLoadsHighlightedPullAndScrolls(t *testing.T) {
	t.Parallel()

	service := &fakeService{diffs: map[string]string{"acme/two#2": longDiff()}}
	model := newTestModel(service)
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 80, Height: 12})
	model = updateModel(t, model, key("down"))

	updated, command := model.Update(key("d"))
	model = updated.(Model)
	if model.mode != modeDiffLoading || model.diff.pr.Key() != "acme/two#2" || command == nil {
		t.Fatalf("diff did not start: mode=%v pr=%s command=%v", model.mode, model.diff.pr.Key(), command)
	}

	model = updateModel(t, model, model.loadDiff(context.Background(), model.diff.pr)())
	if model.mode != modeDiff {
		t.Fatalf("mode = %v, want diff", model.mode)
	}

	model = updateModel(t, model, key(" "))
	pageDown := model.viewport.YOffset
	if pageDown == 0 {
		t.Fatal("space did not page down")
	}
	model = updateModel(t, model, key("pgup"))
	if model.viewport.YOffset >= pageDown {
		t.Fatal("Page Up did not scroll up")
	}
	model = updateModel(t, model, key("q"))
	if model.mode != modeList {
		t.Fatalf("q returned mode %v, want list", model.mode)
	}
}

// A second visit must not hit the API again.
func TestDiffIsCachedBetweenVisits(t *testing.T) {
	t.Parallel()

	service := &fakeService{diffs: map[string]string{"acme/one#1": sampleDiff}}
	model := newTestModel(service)
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 100, Height: 24})

	model = updateModel(t, model, key("d"))
	model = updateModel(t, model, model.loadDiff(context.Background(), model.diff.pr)())
	model = updateModel(t, model, key("esc"))

	updated, command := model.Update(key("d"))
	model = updated.(Model)
	if model.mode != modeDiff {
		t.Fatalf("cached diff should open immediately, mode = %v", model.mode)
	}
	if command != nil {
		t.Fatal("cached diff must not schedule another fetch")
	}
	if len(service.diffRequests) != 1 {
		t.Fatalf("diff requests = %v, want exactly one", service.diffRequests)
	}
}

// A refresh may bring new commits, so cached diffs have to be dropped.
func TestRefreshInvalidatesTheDiffCache(t *testing.T) {
	t.Parallel()

	service := &fakeService{diffs: map[string]string{"acme/one#1": sampleDiff}}
	model := newTestModel(service)
	model = updateModel(t, model, key("d"))
	model = updateModel(t, model, model.loadDiff(context.Background(), model.diff.pr)())
	model = updateModel(t, model, key("esc"))

	model = updateModel(t, model, pullsLoadedMsg{pulls: model.pulls})
	if len(model.diffCache) != 0 {
		t.Fatalf("diff cache survived a refresh: %d entries", len(model.diffCache))
	}
}

func TestDiffSearchFindsAndCyclesMatches(t *testing.T) {
	t.Parallel()

	service := &fakeService{diffs: map[string]string{"acme/one#1": sampleDiff}}
	model := openDiffFor(t, service)

	model = updateModel(t, model, key("/"))
	if !model.diff.searching {
		t.Fatal("/ did not start a search")
	}
	model = typeString(t, model, "context")
	model = updateModel(t, model, key("enter"))

	if len(model.diff.matches) != 3 {
		t.Fatalf("matches = %v, want 3 context lines", model.diff.matches)
	}
	if !strings.Contains(model.View(), "match 1/3") {
		t.Fatalf("footer does not report the match position:\n%s", model.View())
	}

	first := model.diff.matchIndex
	model = updateModel(t, model, key("n"))
	if model.diff.matchIndex == first {
		t.Fatal("n did not advance to the next match")
	}
	model = updateModel(t, model, key("N"))
	if model.diff.matchIndex != first {
		t.Fatal("N did not go back to the previous match")
	}

	// Wrapping around the end must return to the first match.
	for range len(model.diff.matches) {
		model = updateModel(t, model, key("n"))
	}
	if model.diff.matchIndex != first {
		t.Fatalf("matches did not wrap: index = %d", model.diff.matchIndex)
	}
}

func TestDiffSearchWithoutMatchIsReported(t *testing.T) {
	t.Parallel()

	model := openDiffFor(t, &fakeService{diffs: map[string]string{"acme/one#1": sampleDiff}})
	model = updateModel(t, model, key("/"))
	model = typeString(t, model, "nonexistent")
	model = updateModel(t, model, key("enter"))

	if len(model.diff.matches) != 0 {
		t.Fatalf("matches = %v, want none", model.diff.matches)
	}
	if !strings.Contains(model.View(), "no match") {
		t.Fatalf("view does not report the empty result:\n%s", model.View())
	}
}

// Escape must abandon the query without applying it.
func TestDiffSearchCanBeCancelled(t *testing.T) {
	t.Parallel()

	model := openDiffFor(t, &fakeService{diffs: map[string]string{"acme/one#1": sampleDiff}})
	model = updateModel(t, model, key("/"))
	model = typeString(t, model, "context")
	model = updateModel(t, model, key("esc"))

	if model.diff.searching || model.diff.search != "" {
		t.Fatalf("search was not cancelled: searching=%t query=%q", model.diff.searching, model.diff.search)
	}
	if model.mode != modeDiff {
		t.Fatalf("esc during a search must stay in the diff, got %v", model.mode)
	}
}

func TestDiffFileNavigation(t *testing.T) {
	t.Parallel()

	model := openDiffFor(t, &fakeService{diffs: map[string]string{"acme/one#1": sampleDiff}})
	if len(model.diff.fileLines) != 3 {
		t.Fatalf("file offsets = %v, want 3 files", model.diff.fileLines)
	}

	model = updateModel(t, model, key("]"))
	if model.viewport.YOffset != model.diff.fileLines[1] {
		t.Fatalf("] jumped to %d, want %d", model.viewport.YOffset, model.diff.fileLines[1])
	}
	model = updateModel(t, model, key("]"))
	if model.viewport.YOffset != model.diff.fileLines[2] {
		t.Fatalf("] jumped to %d, want %d", model.viewport.YOffset, model.diff.fileLines[2])
	}
	model = updateModel(t, model, key("["))
	if model.viewport.YOffset != model.diff.fileLines[1] {
		t.Fatalf("[ jumped to %d, want %d", model.viewport.YOffset, model.diff.fileLines[1])
	}
}

// Huge diffs must be capped so colorizing them cannot exhaust memory.
func TestOversizedDiffIsTruncated(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("+a line of a very large generated diff\n", (maxDiffBytes/38)+2000)
	if len(huge) <= maxDiffBytes {
		t.Fatalf("test fixture is only %d bytes", len(huge))
	}

	model := openDiffFor(t, &fakeService{diffs: map[string]string{"acme/one#1": huge}})
	if !model.diff.truncated {
		t.Fatal("oversized diff was not truncated")
	}
	joined := strings.Join(model.diff.plain, "\n")
	if len(joined) > maxDiffBytes+500 {
		t.Fatalf("truncated diff is still %d bytes", len(joined))
	}
	if !strings.Contains(model.View(), "truncated") {
		t.Fatalf("view does not warn about truncation:\n%s", lastLines(model.View(), 3))
	}
}

func TestDiffLoadFailureReturnsToList(t *testing.T) {
	t.Parallel()

	service := &fakeService{fail: map[string]error{"acme/one#1": errors.New("server error")}}
	model := newTestModel(service)
	model = updateModel(t, model, key("d"))
	model = updateModel(t, model, model.loadDiff(context.Background(), model.diff.pr)())

	if model.mode != modeList {
		t.Fatalf("mode = %v, want list after a failed diff", model.mode)
	}
	if !strings.Contains(model.status, "server error") {
		t.Fatalf("status = %q", model.status)
	}
}

func TestDiffViewerOpensBrowser(t *testing.T) {
	t.Parallel()

	model := openDiffFor(t, &fakeService{diffs: map[string]string{"acme/one#1": sampleDiff}})
	var opened string
	model.openURL = func(target string) error {
		opened = target
		return nil
	}

	_, command := model.Update(key("w"))
	if command == nil {
		t.Fatal("w did not schedule a browser command")
	}
	command()
	if opened != "https://example.test/1" {
		t.Fatalf("opened %q", opened)
	}
}

func openDiffFor(t *testing.T, service *fakeService) Model {
	t.Helper()
	model := newTestModelWith(service, samplePulls())
	// Keep the viewport shorter than the fixture so scroll offsets are not
	// clamped to the end of the content.
	model = updateModel(t, model, tea.WindowSizeMsg{Width: 100, Height: 10})
	model = updateModel(t, model, key("d"))
	model = updateModel(t, model, model.loadDiff(context.Background(), model.diff.pr)())
	if model.mode != modeDiff {
		t.Fatalf("diff did not open: mode = %v", model.mode)
	}
	return model
}

func longDiff() string {
	var builder strings.Builder
	builder.WriteString("diff --git a/file.go b/file.go\n--- a/file.go\n+++ b/file.go\n@@ -1,40 +1,40 @@\n")
	for index := range 60 {
		fmt.Fprintf(&builder, " context line %d\n", index)
	}
	return builder.String()
}

func lastLines(value string, count int) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	if len(lines) <= count {
		return value
	}
	return strings.Join(lines[len(lines)-count:], "\n")
}
