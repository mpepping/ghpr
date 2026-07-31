package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mpepping/ghpr/internal/githubapi"
)

// GitHubService is the part of the GitHub API used by the TUI.
type GitHubService interface {
	Diff(context.Context, githubapi.PullRequest) (string, error)
	ApproveAndMerge(context.Context, githubapi.PullRequest) (githubapi.MergeOutcome, error)
	RequestChanges(context.Context, githubapi.PullRequest, string) error
	Close(context.Context, githubapi.PullRequest, string) error
}

type mode int

const (
	modeList mode = iota
	modeComment
	modeConfirm
	modeRunning
	modeDiffLoading
	modeDiff
)

type action int

const (
	actionNone action = iota
	actionMerge
	actionClose
	actionRequestChanges
)

type actionResult struct {
	pr      githubapi.PullRequest
	outcome string
	err     error
}

type batchFinishedMsg struct {
	action  action
	results []actionResult
}

type diffLoadedMsg struct {
	key     string
	content string
	err     error
}

var (
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	helpStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	selectedStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cursorStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	errorStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	statusStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	draftStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	diffFileStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	diffHeaderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	diffAddStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	diffDeleteStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	diffMetaStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
)

// Model is the ghpr Bubble Tea model.
type Model struct {
	ctx      context.Context
	service  GitHubService
	owner    string
	pulls    []githubapi.PullRequest
	selected map[string]bool
	cursor   int
	width    int
	height   int

	mode     mode
	pending  action
	comment  string
	input    textinput.Model
	spinner  spinner.Model
	viewport viewport.Model
	diffPR   githubapi.PullRequest
	cancel   context.CancelFunc
	status   string
}

// New creates a multi-select pull request model.
func New(ctx context.Context, service GitHubService, owner string, pulls []githubapi.PullRequest) Model {
	input := textinput.New()
	input.CharLimit = 500
	input.Width = 70

	activity := spinner.New()
	activity.Spinner = spinner.Dot
	activity.Style = statusStyle

	diffViewport := viewport.New(80, 20)
	diffViewport.SetHorizontalStep(8)

	return Model{
		ctx:      ctx,
		service:  service,
		owner:    owner,
		pulls:    append([]githubapi.PullRequest(nil), pulls...),
		selected: make(map[string]bool),
		input:    input,
		spinner:  activity,
		viewport: diffViewport,
	}
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return nil
}

// Update implements tea.Model.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = max(20, min(80, msg.Width-6))
		m.viewport.Width = max(1, msg.Width)
		m.viewport.Height = max(1, msg.Height-3)
		return m, nil
	case batchFinishedMsg:
		return m.finishBatch(msg), nil
	case diffLoadedMsg:
		return m.finishDiff(msg), nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
			}
			return m, tea.Quit
		}
	}

	switch m.mode {
	case modeComment:
		return m.updateComment(message)
	case modeConfirm:
		return m.updateConfirm(message)
	case modeRunning:
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(message)
		return m, cmd
	case modeDiffLoading:
		return m.updateDiffLoading(message)
	case modeDiff:
		return m.updateDiff(message)
	default:
		return m.updateList(message)
	}
}

func (m Model) updateList(message tea.Msg) (tea.Model, tea.Cmd) {
	msg, ok := message.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch msg.String() {
	case "q", "esc":
		return m, tea.Quit
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.pulls)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		if len(m.pulls) > 0 {
			m.cursor = len(m.pulls) - 1
		}
	case "pgup":
		m.cursor = max(0, m.cursor-m.pageSize())
	case "pgdown":
		m.cursor = min(max(0, len(m.pulls)-1), m.cursor+m.pageSize())
	case " ":
		if len(m.pulls) > 0 {
			key := m.pulls[m.cursor].Key()
			m.selected[key] = !m.selected[key]
			if !m.selected[key] {
				delete(m.selected, key)
			}
		}
	case "a":
		if len(m.pulls) > 0 && len(m.selected) == len(m.pulls) {
			m.selected = make(map[string]bool)
		} else {
			for _, pr := range m.pulls {
				m.selected[pr.Key()] = true
			}
		}
	case "m":
		return m.prepareAction(actionMerge)
	case "c":
		return m.prepareAction(actionClose)
	case "r":
		return m.prepareAction(actionRequestChanges)
	case "d":
		return m.openDiff()
	}
	return m, nil
}

func (m Model) openDiff() (tea.Model, tea.Cmd) {
	if len(m.pulls) == 0 {
		return m, nil
	}

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.diffPR = m.pulls[m.cursor]
	m.mode = modeDiffLoading
	m.status = ""
	m.viewport.SetContent("")
	m.viewport.GotoTop()
	return m, tea.Batch(m.spinner.Tick, m.loadDiff(ctx, m.diffPR))
}

func (m Model) loadDiff(ctx context.Context, pr githubapi.PullRequest) tea.Cmd {
	return func() tea.Msg {
		content, err := m.service.Diff(ctx, pr)
		return diffLoadedMsg{key: pr.Key(), content: content, err: err}
	}
}

func (m Model) finishDiff(message diffLoadedMsg) Model {
	if m.mode != modeDiffLoading || message.key != m.diffPR.Key() {
		return m
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if message.err != nil {
		m.status = "Unable to load diff: " + message.err.Error()
		m.mode = modeList
		m.diffPR = githubapi.PullRequest{}
		return m
	}

	content := message.content
	if strings.TrimSpace(content) == "" {
		content = "No changes in this pull request."
	}
	m.viewport.SetContent(highlightDiff(content))
	m.viewport.GotoTop()
	m.mode = modeDiff
	return m
}

func (m Model) updateDiffLoading(message tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := message.(tea.KeyMsg); ok {
		if msg.String() == "esc" || msg.String() == "q" {
			return m.closeDiff(), nil
		}
	}
	var cmd tea.Cmd
	m.spinner, cmd = m.spinner.Update(message)
	return m, cmd
}

func (m Model) updateDiff(message tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := message.(tea.KeyMsg); ok {
		if msg.String() == "esc" || msg.String() == "q" {
			return m.closeDiff(), nil
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(message)
	return m, cmd
}

func (m Model) closeDiff() Model {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mode = modeList
	m.diffPR = githubapi.PullRequest{}
	m.viewport.SetContent("")
	m.status = ""
	return m
}

func (m Model) prepareAction(next action) (tea.Model, tea.Cmd) {
	if len(m.selected) == 0 {
		m.status = "Select one or more pull requests first."
		return m, nil
	}

	m.pending = next
	m.comment = ""
	m.status = ""
	if next == actionMerge {
		m.mode = modeConfirm
		return m, nil
	}

	m.mode = modeComment
	m.input.Reset()
	if next == actionClose {
		m.input.Placeholder = "Optional comment before closing"
	} else {
		m.input.Placeholder = "Reason for requesting changes (required)"
	}
	return m, m.input.Focus()
}

func (m Model) updateComment(message tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := message.(tea.KeyMsg); ok {
		switch msg.String() {
		case "esc":
			m.mode = modeList
			m.pending = actionNone
			m.input.Blur()
			return m, nil
		case "enter":
			m.comment = strings.TrimSpace(m.input.Value())
			if m.pending == actionRequestChanges && m.comment == "" {
				m.status = "A reason is required when requesting changes."
				return m, nil
			}
			m.status = ""
			m.mode = modeConfirm
			m.input.Blur()
			return m, nil
		}
	}

	var cmd tea.Cmd
	m.input, cmd = m.input.Update(message)
	return m, cmd
}

func (m Model) updateConfirm(message tea.Msg) (tea.Model, tea.Cmd) {
	msg, ok := message.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch msg.String() {
	case "y", "Y", "enter":
		selected := m.selectedPulls()
		actionContext, cancel := context.WithCancel(m.ctx)
		m.cancel = cancel
		m.mode = modeRunning
		m.status = ""
		return m, tea.Batch(m.spinner.Tick, m.runBatch(actionContext, m.pending, selected, m.comment))
	case "n", "N", "esc":
		m.mode = modeList
		m.pending = actionNone
		m.comment = ""
	}
	return m, nil
}

func (m Model) runBatch(ctx context.Context, selectedAction action, pulls []githubapi.PullRequest, comment string) tea.Cmd {
	return func() tea.Msg {
		results := make([]actionResult, 0, len(pulls))
		for _, pr := range pulls {
			if ctx.Err() != nil {
				break
			}
			result := actionResult{pr: pr}
			switch selectedAction {
			case actionMerge:
				outcome, err := m.service.ApproveAndMerge(ctx, pr)
				result.outcome = string(outcome)
				result.err = err
			case actionClose:
				result.outcome = "closed"
				result.err = m.service.Close(ctx, pr, comment)
			case actionRequestChanges:
				result.outcome = "changes requested"
				result.err = m.service.RequestChanges(ctx, pr, comment)
			}
			results = append(results, result)
		}
		return batchFinishedMsg{action: selectedAction, results: results}
	}
}

func (m Model) finishBatch(message batchFinishedMsg) Model {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	succeeded := make(map[string]bool)
	failures := make([]string, 0)
	outcomes := make(map[string]int)

	for _, result := range message.results {
		if result.err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", result.pr.Key(), result.err))
			continue
		}
		succeeded[result.pr.Key()] = true
		outcomes[result.outcome]++
		delete(m.selected, result.pr.Key())
	}

	if message.action == actionMerge || message.action == actionClose {
		remaining := m.pulls[:0]
		for _, pr := range m.pulls {
			if !succeeded[pr.Key()] {
				remaining = append(remaining, pr)
			}
		}
		m.pulls = remaining
	}
	if len(m.pulls) == 0 {
		m.cursor = 0
	} else if m.cursor >= len(m.pulls) {
		m.cursor = len(m.pulls) - 1
	}

	summary := make([]string, 0, len(outcomes)+1)
	for _, name := range []string{"merged", "auto-merge enabled", "closed", "changes requested"} {
		if count := outcomes[name]; count > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", count, name))
		}
	}
	if len(failures) > 0 {
		summary = append(summary, fmt.Sprintf("%d failed", len(failures)))
	}
	m.status = "Completed: " + strings.Join(summary, ", ")
	for index, failure := range failures {
		if index == 3 {
			m.status += fmt.Sprintf("\n…and %d more error(s)", len(failures)-index)
			break
		}
		m.status += "\n" + failure
	}

	m.mode = modeList
	m.pending = actionNone
	m.comment = ""
	return m
}

func (m Model) selectedPulls() []githubapi.PullRequest {
	pulls := make([]githubapi.PullRequest, 0, len(m.selected))
	for _, pr := range m.pulls {
		if m.selected[pr.Key()] {
			pulls = append(pulls, pr)
		}
	}
	return pulls
}

// View implements tea.Model.
func (m Model) View() string {
	if m.mode == modeDiffLoading {
		return m.renderDiffLoading()
	}
	if m.mode == modeDiff {
		return m.renderDiff()
	}

	var view strings.Builder
	fmt.Fprintf(&view, "%s  %s\n", titleStyle.Render("ghpr"), helpStyle.Render("owner: "+m.owner))
	fmt.Fprintf(&view, "%d open pull request(s) · %d selected\n\n", len(m.pulls), len(m.selected))

	if len(m.pulls) == 0 {
		view.WriteString("No open pull requests remain.\n")
	} else {
		start, end := m.visibleRange()
		for index := start; index < end; index++ {
			view.WriteString(m.renderRow(index))
			view.WriteByte('\n')
		}
	}

	view.WriteByte('\n')
	switch m.mode {
	case modeComment:
		if m.pending == actionClose {
			view.WriteString("Comment to add before closing (optional):\n")
		} else {
			view.WriteString("Reason for requesting changes:\n")
		}
		view.WriteString(m.input.View())
		view.WriteByte('\n')
		if m.status != "" {
			view.WriteString(errorStyle.Render(m.status))
			view.WriteByte('\n')
		}
		view.WriteString(helpStyle.Render("enter continue · esc cancel"))
	case modeConfirm:
		view.WriteString(m.renderConfirmation())
	case modeRunning:
		fmt.Fprintf(&view, "%s Applying action to %d pull request(s)…\n", m.spinner.View(), len(m.selected))
		view.WriteString(helpStyle.Render("ctrl+c cancel"))
	default:
		if m.status != "" {
			style := statusStyle
			if strings.Contains(m.status, "failed") || strings.Contains(m.status, "required") || strings.HasPrefix(m.status, "Unable") {
				style = errorStyle
			}
			view.WriteString(style.Render(m.status))
			view.WriteByte('\n')
		}
		view.WriteString(helpStyle.Render("↑/↓ navigate · space select · d diff · a all · m approve+merge · c close · r request changes · q quit"))
		if len(m.pulls) > 0 {
			view.WriteByte('\n')
			view.WriteString(helpStyle.Render(m.pulls[m.cursor].URL))
		}
	}

	return view.String()
}

func (m Model) renderDiffLoading() string {
	var view strings.Builder
	fmt.Fprintf(&view, "%s  %s\n", titleStyle.Render("ghpr diff"), m.diffPR.Key())
	view.WriteString(truncate(m.diffPR.Title, max(20, m.width)))
	view.WriteString("\n\n")
	fmt.Fprintf(&view, "%s Loading diff…\n\n", m.spinner.View())
	view.WriteString(helpStyle.Render("esc/q back · ctrl+c quit"))
	return view.String()
}

func (m Model) renderDiff() string {
	var view strings.Builder
	fmt.Fprintf(&view, "%s  %s — %s\n", titleStyle.Render("ghpr diff"), m.diffPR.Key(), truncate(m.diffPR.Title, max(20, m.width-len(m.diffPR.Key())-15)))
	view.WriteString(m.viewport.View())
	view.WriteByte('\n')
	progress := int(m.viewport.ScrollPercent()*100 + 0.5)
	fmt.Fprintf(&view, "%s", helpStyle.Render(fmt.Sprintf("%3d%% · space/pgdn page down · pgup page up · ↑/↓ scroll · esc/q back", progress)))
	return view.String()
}

func (m Model) renderRow(index int) string {
	pr := m.pulls[index]
	cursor := "  "
	if index == m.cursor {
		cursor = cursorStyle.Render("> ")
	}
	checkbox := "[ ]"
	if m.selected[pr.Key()] {
		checkbox = selectedStyle.Render("[x]")
	}

	repositoryWidth := 26
	if m.width > 0 && m.width < 80 {
		repositoryWidth = 18
	}
	repository := padRight(truncate(pr.Repository(), repositoryWidth), repositoryWidth)
	prefix := fmt.Sprintf("%s%s %s #%d", cursor, checkbox, repository, pr.Number)
	extra := ""
	if pr.Draft {
		extra = " " + draftStyle.Render("DRAFT")
	}
	titleWidth := 80
	if m.width > 0 {
		titleWidth = max(15, m.width-lipgloss.Width(prefix)-lipgloss.Width(extra)-2)
	}
	return fmt.Sprintf("%s %s%s", prefix, truncate(pr.Title, titleWidth), extra)
}

func (m Model) renderConfirmation() string {
	verb := "approve and squash-merge"
	if m.pending == actionClose {
		verb = "close"
	} else if m.pending == actionRequestChanges {
		verb = "request changes on"
	}

	var view strings.Builder
	fmt.Fprintf(&view, "%s %d pull request(s)?\n", strings.ToUpper(verb[:1])+verb[1:], len(m.selected))
	if m.comment != "" {
		fmt.Fprintf(&view, "Comment: %s\n", truncate(m.comment, max(20, m.width-10)))
	}
	view.WriteString(helpStyle.Render("y/enter confirm · n/esc cancel"))
	return view.String()
}

func (m Model) visibleRange() (int, int) {
	pageSize := m.pageSize()
	start := 0
	if m.cursor >= pageSize {
		start = m.cursor - pageSize + 1
	}
	end := min(len(m.pulls), start+pageSize)
	return start, end
}

func (m Model) pageSize() int {
	if m.height <= 0 {
		return 15
	}
	return max(3, m.height-9)
}

func truncate(value string, width int) string {
	if width <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= width {
		return value
	}
	if width == 1 {
		return "…"
	}
	return string(runes[:width-1]) + "…"
}

func padRight(value string, width int) string {
	padding := width - lipgloss.Width(value)
	if padding <= 0 {
		return value
	}
	return value + strings.Repeat(" ", padding)
}

func highlightDiff(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			lines[index] = diffFileStyle.Render(line)
		case strings.HasPrefix(line, "@@"):
			lines[index] = diffHeaderStyle.Render(line)
		case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
			lines[index] = diffHeaderStyle.Render(line)
		case strings.HasPrefix(line, "+"):
			lines[index] = diffAddStyle.Render(line)
		case strings.HasPrefix(line, "-"):
			lines[index] = diffDeleteStyle.Render(line)
		case strings.HasPrefix(line, "index "),
			strings.HasPrefix(line, "new file mode "),
			strings.HasPrefix(line, "deleted file mode "),
			strings.HasPrefix(line, "similarity index "),
			strings.HasPrefix(line, "rename from "),
			strings.HasPrefix(line, "rename to "),
			strings.HasPrefix(line, "Binary files "),
			strings.HasPrefix(line, "\\ No newline at end of file"):
			lines[index] = diffMetaStyle.Render(line)
		}
	}
	return strings.Join(lines, "\n")
}
