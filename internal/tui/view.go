package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/mpepping/ghpr/internal/githubapi"
)

// View implements tea.Model.
func (m Model) View() string {
	switch m.mode {
	case modeLoading:
		return m.renderLoading()
	case modeDiffLoading:
		return m.renderDiffLoading()
	case modeDiff:
		return m.renderDiff()
	case modeHelp:
		return m.renderOverlay("ghpr help", "esc/q back · ↑/↓ scroll")
	case modeLog:
		return m.renderOverlay("ghpr session log", "esc/q back · ↑/↓ scroll")
	}

	var view strings.Builder
	view.WriteString(m.renderHeader())
	view.WriteString(m.renderCounts())
	view.WriteString("\n\n")

	if len(m.pulls) == 0 {
		view.WriteString("No open pull requests found.\n")
	} else if len(m.visible) == 0 {
		view.WriteString(helpStyle.Render("No pull requests match the filter.") + "\n")
	} else {
		view.WriteString(m.renderListHeader())
		view.WriteByte('\n')
		start, end := m.visibleRange()
		for index := start; index < end; index++ {
			view.WriteString(m.renderRow(index))
			view.WriteByte('\n')
		}
	}

	view.WriteByte('\n')
	switch m.mode {
	case modeFilter:
		view.WriteString("Filter by repository, title or author:\n")
		view.WriteString(m.filterInput.View())
		view.WriteByte('\n')
		view.WriteString(helpStyle.Render("enter apply · esc cancel"))
	case modeComment:
		view.WriteString(m.renderCommentPrompt())
	case modeConfirm:
		view.WriteString(m.renderConfirmation())
	case modeRunning:
		view.WriteString(m.renderProgress())
	default:
		view.WriteString(m.renderFooter())
	}

	return view.String()
}

func (m Model) renderHeader() string {
	title := titleStyle.Render("ghpr")
	context := "owner: " + m.searchLabel()
	if m.dryRun {
		return fmt.Sprintf("%s  %s  %s\n", title, helpStyle.Render(context), warnStyle.Render("DRY RUN"))
	}
	return fmt.Sprintf("%s  %s\n", title, helpStyle.Render(context))
}

func (m Model) searchLabel() string {
	owner := m.search.Owner
	if owner == "" {
		owner = "(any)"
	}
	if m.search.Scope != "" && m.search.Scope != githubapi.ScopeOwned {
		return owner + " · " + string(m.search.Scope)
	}
	return owner
}

func (m Model) renderLoading() string {
	var view strings.Builder
	view.WriteString(m.renderHeader())
	view.WriteByte('\n')
	fmt.Fprintf(&view, "%s Loading open pull requests…\n\n", m.spinner.View())
	view.WriteString(helpStyle.Render("q quit"))
	return view.String()
}

func (m Model) renderCounts() string {
	counts := fmt.Sprintf("%d open pull request(s)", len(m.pulls))
	if m.filter != "" {
		counts = fmt.Sprintf("%d of %d shown", len(m.visible), len(m.pulls))
	}
	counts += fmt.Sprintf(" · %d selected", len(m.selected))
	if m.refreshing {
		counts += " · " + m.spinner.View() + "refreshing"
	}
	if m.filter != "" && m.mode != modeFilter {
		counts += " · " + filterStyle.Render("filter: "+m.filter)
	}
	return counts
}

func (m Model) renderFooter() string {
	var view strings.Builder
	if m.status != "" {
		style := statusStyle
		if strings.Contains(m.status, "failed") || strings.Contains(m.status, "required") || strings.HasPrefix(m.status, "Unable") {
			style = errorStyle
		}
		view.WriteString(style.Render(m.status))
		view.WriteByte('\n')
	}
	if m.stateWarning != "" {
		view.WriteString(errorStyle.Render(clamp("CI/review data incomplete: "+m.stateWarning, m.textWidth())))
		view.WriteByte('\n')
	}
	view.WriteString(helpStyle.Render("↑/↓ navigate · space select · a all · / filter · R refresh · d diff · m merge · c close · ? help · q quit"))
	if pr, ok := m.current(); ok {
		view.WriteByte('\n')
		view.WriteString(helpStyle.Render(clamp(m.stateLabel(pr)+" · "+pr.URL, m.textWidth())))
	}
	return view.String()
}

func (m Model) renderCommentPrompt() string {
	var view strings.Builder
	switch m.pending {
	case actionClose:
		view.WriteString("Comment to add before closing (optional):\n")
	case actionComment:
		view.WriteString("Comment to post on the selected pull requests:\n")
	default:
		view.WriteString("Reason for requesting changes:\n")
	}
	view.WriteString(m.input.View())
	view.WriteByte('\n')
	if m.status != "" {
		view.WriteString(errorStyle.Render(m.status))
		view.WriteByte('\n')
	}
	view.WriteString(helpStyle.Render("ctrl+d continue · ctrl+e external editor · enter newline · esc cancel"))
	return view.String()
}

func (m Model) renderProgress() string {
	var view strings.Builder
	fmt.Fprintf(&view, "%s %s · %d/%d", m.spinner.View(), m.batchAction.progressLabel(), m.batchDone, m.batchTotal)
	if m.batchFailed > 0 {
		view.WriteString(errorStyle.Render(fmt.Sprintf(" · %d failed", m.batchFailed)))
	}
	view.WriteByte('\n')
	if m.batchCurrent != "" && m.batchDone < m.batchTotal {
		view.WriteString(helpStyle.Render("current: " + m.batchCurrent))
		view.WriteByte('\n')
	}

	// Show the tail of the results so long batches stay informative.
	const recent = 4
	start := max(0, len(m.batchResults)-recent)
	for _, result := range m.batchResults[start:] {
		if result.err != nil {
			view.WriteString(errorStyle.Render(clamp("✗ "+result.pr.Key()+": "+result.err.Error(), m.textWidth())))
		} else {
			view.WriteString(selectedStyle.Render(clamp("✓ "+result.pr.Key()+" "+result.outcome, m.textWidth())))
		}
		view.WriteByte('\n')
	}
	view.WriteString(helpStyle.Render("ctrl+c cancel"))
	return view.String()
}

// renderConfirmation names the pull requests that are about to change. For a
// destructive bulk action a count alone is not enough to review.
func (m Model) renderConfirmation() string {
	selected := m.selectedPulls()
	verb := m.pending.verb()
	if m.pending == actionMerge {
		verb = "approve and " + m.mergeMethod.Label()
	}

	var view strings.Builder
	prefix := ""
	if m.dryRun {
		prefix = warnStyle.Render("[dry run] ")
	}
	fmt.Fprintf(&view, "%s%s %d pull request(s)?\n", prefix, strings.ToUpper(verb[:1])+verb[1:], len(selected))

	// Keep the list short enough that it never pushes the prompt off screen.
	limit := 6
	if m.height > 0 {
		limit = max(1, min(10, m.height/3))
	}
	for index, pr := range selected {
		if index == limit {
			fmt.Fprintf(&view, "  %s\n", helpStyle.Render(fmt.Sprintf("…and %d more", len(selected)-index)))
			break
		}
		marker := "  "
		if state, ok := m.states[pr.Key()]; ok && state.Blocked() {
			marker = warnStyle.Render("! ")
		}
		fmt.Fprintf(&view, "%s%s\n", marker, clamp(pr.Key()+"  "+pr.Title, max(0, m.textWidth()-2)))
	}

	if m.comment != "" {
		first := strings.SplitN(m.comment, "\n", 2)[0]
		if len(first) < len(m.comment) {
			first += " …"
		}
		fmt.Fprintf(&view, "Message: %s\n", clamp(first, max(0, m.textWidth()-9)))
	}
	if blocked := m.blockedCount(selected); blocked > 0 && m.pending == actionMerge {
		fmt.Fprintf(&view, "%s\n", warnStyle.Render(fmt.Sprintf("! %d selected pull request(s) have failing checks, conflicts or requested changes", blocked)))
	}
	view.WriteString(helpStyle.Render("y/enter confirm · n/esc cancel"))
	return view.String()
}

func (m Model) blockedCount(pulls []githubapi.PullRequest) int {
	blocked := 0
	for _, pr := range pulls {
		if state, ok := m.states[pr.Key()]; ok && state.Blocked() {
			blocked++
		}
	}
	return blocked
}

func (m Model) renderOverlay(title, help string) string {
	var view strings.Builder
	fmt.Fprintf(&view, "%s\n", titleStyle.Render(title))
	view.WriteString(m.viewport.View())
	view.WriteByte('\n')
	view.WriteString(helpStyle.Render(help))
	return view.String()
}

func (m Model) renderDiffLoading() string {
	var view strings.Builder
	fmt.Fprintf(&view, "%s  %s\n", titleStyle.Render("ghpr diff"), m.diff.pr.Key())
	view.WriteString(clamp(m.diff.pr.Title, m.textWidth()))
	view.WriteString("\n\n")
	fmt.Fprintf(&view, "%s Loading diff…\n\n", m.spinner.View())
	view.WriteString(helpStyle.Render("esc/q back · ctrl+c quit"))
	return view.String()
}

func (m Model) renderDiff() string {
	var view strings.Builder
	header := m.diff.pr.Key()
	if m.diff.pr.Title != "" {
		header += " — " + m.diff.pr.Title
	}
	fmt.Fprintf(&view, "%s  %s\n", titleStyle.Render("ghpr diff"), clamp(header, max(0, m.textWidth()-12)))
	view.WriteString(m.viewport.View())
	view.WriteByte('\n')

	if m.diff.searching {
		view.WriteString(helpStyle.Render("search: ") + m.diff.searchDraft + "▏")
		return view.String()
	}

	progress := int(m.viewport.ScrollPercent()*100 + 0.5)
	footer := fmt.Sprintf("%3d%%", progress)
	if m.diff.search != "" {
		if len(m.diff.matches) == 0 {
			footer += " · " + errorStyle.Render("no match: "+m.diff.search)
		} else {
			footer += fmt.Sprintf(" · match %d/%d for %q", m.diff.matchIndex+1, len(m.diff.matches), m.diff.search)
		}
	}
	if m.diff.truncated {
		footer += " · " + warnStyle.Render("truncated")
	}
	footer += " · space/pgdn page · / search · n/N next · ]/[ file · w web · esc back"
	view.WriteString(helpStyle.Render(clamp(footer, m.textWidth())))
	return view.String()
}

// layout describes which optional columns fit in the current terminal width.
type layout struct {
	repository int
	number     int
	age        int
	author     int
}

func (m Model) layout() layout {
	width := m.width
	if width <= 0 {
		width = 100
	}
	columns := layout{repository: 26, number: 7}
	if width < 80 {
		columns.repository = 18
	}
	if width >= 70 {
		columns.age = 4
	}
	switch {
	case width >= 110:
		columns.author = 16
	case width >= 95:
		columns.author = 12
	}
	return columns
}

func (m Model) renderListHeader() string {
	columns := m.layout()
	header := "      " + padRight("REPOSITORY", columns.repository) + " " + padRight("PR", columns.number) + " CI RV"
	if columns.age > 0 {
		header += " " + padRight("AGE", columns.age)
	}
	if columns.author > 0 {
		header += " " + padRight("AUTHOR", columns.author)
	}
	header += " TITLE"
	return helpStyle.Render(header)
}

func (m Model) renderRow(index int) string {
	pr := m.visible[index]
	columns := m.layout()

	cursor := "  "
	if index == m.cursor {
		cursor = cursorStyle.Render("> ")
	}
	checkbox := "[ ]"
	if m.selected[pr.Key()] {
		checkbox = selectedStyle.Render("[x]")
	}

	repository := padRight(clamp(pr.Repository(), columns.repository), columns.repository)
	number := padRight(fmt.Sprintf("#%d", pr.Number), columns.number)
	prefix := fmt.Sprintf("%s%s %s %s %s %s", cursor, checkbox, repository, number, m.renderBuildGlyph(pr), m.renderReviewGlyph(pr))
	if columns.age > 0 {
		prefix += " " + m.renderAge(pr, columns.age)
	}
	if columns.author > 0 {
		prefix += " " + padRight(authorStyle.Render(clamp(pr.Author, columns.author)), columns.author)
	}

	extra := ""
	if pr.Draft {
		extra = " " + draftStyle.Render("DRAFT")
	}
	titleWidth := 80
	if m.width > 0 {
		titleWidth = max(15, m.width-lipgloss.Width(prefix)-lipgloss.Width(extra)-2)
	}
	return fmt.Sprintf("%s %s%s", prefix, clamp(pr.Title, titleWidth), extra)
}

func (m Model) renderAge(pr githubapi.PullRequest, width int) string {
	age := formatAge(pr.UpdatedAt, m.now())
	style := helpStyle
	// Anything untouched for a month is worth calling out.
	if !pr.UpdatedAt.IsZero() && m.now().Sub(pr.UpdatedAt) > 30*24*time.Hour {
		style = staleStyle
	}
	return padRight(style.Render(clamp(age, width)), width)
}

// formatAge renders a compact relative duration such as "4h" or "3w".
func formatAge(updated, now time.Time) string {
	if updated.IsZero() {
		return "-"
	}
	elapsed := now.Sub(updated)
	switch {
	case elapsed < time.Minute:
		return "now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh", int(elapsed.Hours()))
	case elapsed < 7*24*time.Hour:
		return fmt.Sprintf("%dd", int(elapsed.Hours()/24))
	case elapsed < 365*24*time.Hour:
		return fmt.Sprintf("%dw", int(elapsed.Hours()/(24*7)))
	default:
		return fmt.Sprintf("%dy", int(elapsed.Hours()/(24*365)))
	}
}

func (m Model) renderBuildGlyph(pr githubapi.PullRequest) string {
	state, loaded := m.states[pr.Key()]
	if !loaded {
		return padRight(helpStyle.Render("·"), 2)
	}
	switch state.Build {
	case githubapi.BuildStatusSuccess:
		return padRight(buildSuccessStyle.Render("✓"), 2)
	case githubapi.BuildStatusPending:
		return padRight(buildPendingStyle.Render("…"), 2)
	case githubapi.BuildStatusFailure:
		return padRight(buildFailureStyle.Render("✗"), 2)
	case githubapi.BuildStatusUnknown:
		return padRight(buildUnknownStyle.Render("?"), 2)
	default:
		return padRight(helpStyle.Render("–"), 2)
	}
}

// renderReviewGlyph shows merge readiness. Conflicts win over the review
// decision because they block the merge regardless of approvals.
func (m Model) renderReviewGlyph(pr githubapi.PullRequest) string {
	state, loaded := m.states[pr.Key()]
	if !loaded {
		return padRight(helpStyle.Render("·"), 2)
	}
	if state.Mergeable == githubapi.MergeableConflicting {
		return padRight(buildFailureStyle.Render("⚠"), 2)
	}
	switch state.Review {
	case githubapi.ReviewDecisionApproved:
		return padRight(buildSuccessStyle.Render("✓"), 2)
	case githubapi.ReviewDecisionChangesRequested:
		return padRight(buildFailureStyle.Render("✗"), 2)
	case githubapi.ReviewDecisionReviewRequired:
		return padRight(buildPendingStyle.Render("○"), 2)
	default:
		return padRight(helpStyle.Render("–"), 2)
	}
}

func (m Model) stateLabel(pr githubapi.PullRequest) string {
	state, loaded := m.states[pr.Key()]
	if !loaded {
		return "CI: loading"
	}
	return state.Summary()
}

func (m Model) visibleRange() (int, int) {
	pageSize := m.pageSize()
	start := 0
	if m.cursor >= pageSize {
		start = m.cursor - pageSize + 1
	}
	end := min(len(m.visible), start+pageSize)
	return start, end
}

// textWidth is the width available for free-form text. Before the first
// WindowSizeMsg arrives the width is unknown, so nothing is truncated.
func (m Model) textWidth() int {
	if m.width <= 0 {
		return 0
	}
	return max(20, m.width)
}

// clamp truncates to a display width, counting the cells a terminal actually
// uses. Counting runes would misalign columns for CJK text and emoji, which
// occupy two cells each. Width 0 means "unbounded".
func clamp(value string, width int) string {
	if width <= 0 {
		return value
	}
	if ansi.StringWidth(value) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return ansi.Truncate(value, width, "…")
}

// padRight pads to a display width, ignoring any ANSI styling already applied.
func padRight(value string, width int) string {
	padding := width - ansi.StringWidth(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

// displayWidth reports how many terminal cells a rendered string occupies.
func displayWidth(value string) int {
	return ansi.StringWidth(value)
}
