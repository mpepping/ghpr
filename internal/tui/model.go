package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/mpepping/ghpr/internal/browser"
	"github.com/mpepping/ghpr/internal/githubapi"
)

// GitHubService is the part of the GitHub API used by the TUI.
type GitHubService interface {
	ListOpenPullRequests(context.Context, string, int) ([]githubapi.PullRequest, error)
	PullRequestStates(context.Context, []githubapi.PullRequest) (map[string]githubapi.PullRequestState, error)
	Diff(context.Context, githubapi.PullRequest) (string, error)
	ApproveAndMerge(context.Context, githubapi.PullRequest) (githubapi.MergeOutcome, error)
	RequestChanges(context.Context, githubapi.PullRequest, string) error
	Close(context.Context, githubapi.PullRequest, string) error
}

const stateBatchSize = 25

type mode int

const (
	modeList mode = iota
	modeFilter
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

// batchItemStartedMsg is emitted just before an action runs for a pull request
// so the UI can name the item currently being processed.
type batchItemStartedMsg struct {
	generation int
	index      int
	pr         githubapi.PullRequest
}

// batchItemDoneMsg reports the outcome of a single pull request, letting the
// list update incrementally instead of waiting for the whole batch.
type batchItemDoneMsg struct {
	generation int
	result     actionResult
}

type batchFinishedMsg struct {
	generation int
	action     action
	results    []actionResult
}

type diffLoadedMsg struct {
	key     string
	content string
	err     error
}

type urlOpenedMsg struct {
	key string
	err error
}

type statesLoadedMsg struct {
	generation int
	keys       []string
	states     map[string]githubapi.PullRequestState
	err        error
}

type refreshedMsg struct {
	pulls []githubapi.PullRequest
	err   error
}

var (
	titleStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("63"))
	helpStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	selectedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	cursorStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	errorStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	statusStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("39"))
	draftStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("214"))
	filterStyle       = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	staleStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
	authorStyle       = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	diffFileStyle     = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	diffHeaderStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("45"))
	diffAddStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	diffDeleteStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("196"))
	diffMetaStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	buildSuccessStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("42"))
	buildPendingStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	buildFailureStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("196"))
	buildUnknownStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("208"))
)

// Model is the ghpr Bubble Tea model.
type Model struct {
	ctx     context.Context
	service GitHubService
	owner   string
	limit   int
	now     func() time.Time

	// pulls holds every loaded pull request; visible holds the subset that
	// passes the active filter. The cursor always indexes into visible.
	pulls    []githubapi.PullRequest
	visible  []githubapi.PullRequest
	selected map[string]bool
	cursor   int
	width    int
	height   int

	mode        mode
	pending     action
	comment     string
	input       textinput.Model
	filterInput textinput.Model
	filter      string
	filterDraft string
	spinner     spinner.Model
	viewport    viewport.Model
	diffPR      githubapi.PullRequest
	openURL     func(string) error

	states       map[string]githubapi.PullRequestState
	stateQueue   []githubapi.PullRequest
	stateNext    int
	stateWarning string

	// generation invalidates in-flight state loads after a refresh so results
	// for a stale pull request list are discarded. Batches carry their own
	// counter: a refresh must never orphan a batch that is already running.
	generation      int
	batchGeneration int
	refreshing      bool

	batchEvents  chan tea.Msg
	batchAction  action
	batchTotal   int
	batchDone    int
	batchFailed  int
	batchCurrent string
	batchResults []actionResult

	cancel context.CancelFunc
	status string
}

// New creates a multi-select pull request model.
func New(ctx context.Context, service GitHubService, owner string, limit int, pulls []githubapi.PullRequest) Model {
	pullList := append([]githubapi.PullRequest(nil), pulls...)
	input := textinput.New()
	input.CharLimit = 500
	input.Width = 70

	filter := textinput.New()
	filter.Prompt = "/"
	filter.CharLimit = 100
	filter.Width = 60

	activity := spinner.New()
	activity.Spinner = spinner.Dot
	activity.Style = statusStyle

	diffViewport := viewport.New(80, 20)
	diffViewport.SetHorizontalStep(8)

	model := Model{
		ctx:         ctx,
		service:     service,
		owner:       owner,
		limit:       limit,
		now:         time.Now,
		pulls:       pullList,
		selected:    make(map[string]bool),
		input:       input,
		filterInput: filter,
		spinner:     activity,
		viewport:    diffViewport,
		openURL:     browser.Open,
		states:      make(map[string]githubapi.PullRequestState),
		stateQueue:  append([]githubapi.PullRequest(nil), pullList...),
	}
	return model.applyFilter()
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return m.loadNextStateBatch()
}

// Update implements tea.Model.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.Width = max(20, min(80, msg.Width-6))
		m.filterInput.Width = max(20, min(80, msg.Width-6))
		m.viewport.Width = max(1, msg.Width)
		m.viewport.Height = max(1, msg.Height-3)
		return m, nil
	case batchItemStartedMsg:
		if msg.generation != m.batchGeneration {
			return m, nil
		}
		m.batchCurrent = msg.pr.Key()
		return m, waitForBatchEvent(m.batchEvents)
	case batchItemDoneMsg:
		if msg.generation != m.batchGeneration {
			return m, nil
		}
		m.batchDone++
		m.batchResults = append(m.batchResults, msg.result)
		if msg.result.err != nil {
			m.batchFailed++
		}
		return m, waitForBatchEvent(m.batchEvents)
	case batchFinishedMsg:
		if msg.generation != m.batchGeneration {
			return m, nil
		}
		return m.finishBatch(msg), nil
	case diffLoadedMsg:
		return m.finishDiff(msg), nil
	case refreshedMsg:
		return m.finishRefresh(msg)
	case urlOpenedMsg:
		if msg.err != nil {
			m.status = "Unable to open " + msg.key + " in the default browser: " + msg.err.Error()
		} else {
			m.status = "Opened " + msg.key + " in the default browser."
		}
		return m, nil
	case statesLoadedMsg:
		return m.finishStateBatch(msg)
	case spinner.TickMsg:
		// The spinner also runs in list mode while a refresh is in flight.
		if m.refreshing || m.mode == modeRunning || m.mode == modeDiffLoading {
			var cmd tea.Cmd
			m.spinner, cmd = m.spinner.Update(message)
			return m, cmd
		}
		return m, nil
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			if m.cancel != nil {
				m.cancel()
				m.cancel = nil
			}
			return m, tea.Quit
		}
	}

	switch m.mode {
	case modeFilter:
		return m.updateFilter(message)
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

// current returns the highlighted pull request, if the visible list is not empty.
func (m Model) current() (githubapi.PullRequest, bool) {
	if len(m.visible) == 0 || m.cursor < 0 || m.cursor >= len(m.visible) {
		return githubapi.PullRequest{}, false
	}
	return m.visible[m.cursor], true
}

// applyFilter recomputes the visible slice and keeps the cursor on the same
// pull request whenever it survives the filter.
func (m Model) applyFilter() Model {
	previous, hadPrevious := m.current()

	terms := strings.Fields(strings.ToLower(m.filter))
	if len(terms) == 0 {
		// Copy so later in-place edits of pulls cannot corrupt visible.
		m.visible = append([]githubapi.PullRequest(nil), m.pulls...)
	} else {
		visible := make([]githubapi.PullRequest, 0, len(m.pulls))
		for _, pr := range m.pulls {
			if matchesFilter(pr, terms) {
				visible = append(visible, pr)
			}
		}
		m.visible = visible
	}

	m.cursor = 0
	if hadPrevious {
		for index, pr := range m.visible {
			if pr.Key() == previous.Key() {
				m.cursor = index
				break
			}
		}
	}
	if m.cursor >= len(m.visible) {
		m.cursor = max(0, len(m.visible)-1)
	}
	return m
}

// matchesFilter reports whether every term appears in the pull request's
// searchable text. Terms are ANDed so "dependabot go" narrows progressively.
func matchesFilter(pr githubapi.PullRequest, terms []string) bool {
	haystack := strings.ToLower(fmt.Sprintf("%s #%d %s %s", pr.Repository(), pr.Number, pr.Title, pr.Author))
	if pr.Draft {
		haystack += " draft"
	}
	for _, term := range terms {
		if !strings.Contains(haystack, term) {
			return false
		}
	}
	return true
}

func (m Model) loadNextStateBatch() tea.Cmd {
	if m.stateNext >= len(m.stateQueue) {
		return nil
	}
	end := min(len(m.stateQueue), m.stateNext+stateBatchSize)
	batch := append([]githubapi.PullRequest(nil), m.stateQueue[m.stateNext:end]...)
	generation := m.generation
	return func() tea.Msg {
		states, err := m.service.PullRequestStates(m.ctx, batch)
		keys := make([]string, 0, len(batch))
		for _, pr := range batch {
			keys = append(keys, pr.Key())
		}
		return statesLoadedMsg{generation: generation, keys: keys, states: states, err: err}
	}
}

func (m Model) finishStateBatch(message statesLoadedMsg) (tea.Model, tea.Cmd) {
	if message.generation != m.generation {
		return m, nil
	}

	// A partial failure still carries usable data for the other pull requests,
	// so prefer any state that came back and only fall back to unknown.
	for _, key := range message.keys {
		if state, ok := message.states[key]; ok {
			m.states[key] = state
			continue
		}
		m.states[key] = githubapi.PullRequestState{
			Build:     githubapi.BuildStatusUnknown,
			Mergeable: githubapi.MergeableUnknown,
		}
	}
	if message.err != nil {
		m.stateWarning = message.err.Error()
	}
	m.stateNext += len(message.keys)
	return m, m.loadNextStateBatch()
}

func (m Model) updateList(message tea.Msg) (tea.Model, tea.Cmd) {
	msg, ok := message.(tea.KeyMsg)
	if !ok {
		return m, nil
	}

	switch msg.String() {
	case "q":
		return m, tea.Quit
	case "esc":
		// Escape clears the filter instead of quitting, so it is consistent
		// with the cancel meaning it has in every other mode.
		if m.filter != "" {
			m.filter = ""
			m.filterInput.Reset()
			m.status = ""
			return m.applyFilter(), nil
		}
		return m, nil
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.visible)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		if len(m.visible) > 0 {
			m.cursor = len(m.visible) - 1
		}
	case "pgup":
		m.cursor = max(0, m.cursor-m.pageSize())
	case "pgdown":
		m.cursor = min(max(0, len(m.visible)-1), m.cursor+m.pageSize())
	case " ":
		if pr, ok := m.current(); ok {
			key := pr.Key()
			m.selected[key] = !m.selected[key]
			if !m.selected[key] {
				delete(m.selected, key)
			}
		}
	case "a":
		m = m.toggleSelectAll()
	case "/":
		m.mode = modeFilter
		m.filterDraft = m.filter
		m.filterInput.SetValue(m.filter)
		m.filterInput.CursorEnd()
		m.status = ""
		return m, m.filterInput.Focus()
	case "R", "ctrl+r":
		return m.startRefresh()
	case "m":
		return m.prepareAction(actionMerge)
	case "c":
		return m.prepareAction(actionClose)
	case "r":
		return m.prepareAction(actionRequestChanges)
	case "d":
		return m.openDiff()
	case "w":
		return m.openHighlightedURL()
	}
	return m, nil
}

// toggleSelectAll operates on the visible rows only, which makes
// "filter, then select all" a precise bulk-selection workflow.
func (m Model) toggleSelectAll() Model {
	if len(m.visible) == 0 {
		return m
	}
	allSelected := true
	for _, pr := range m.visible {
		if !m.selected[pr.Key()] {
			allSelected = false
			break
		}
	}
	for _, pr := range m.visible {
		if allSelected {
			delete(m.selected, pr.Key())
		} else {
			m.selected[pr.Key()] = true
		}
	}
	return m
}

func (m Model) updateFilter(message tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := message.(tea.KeyMsg); ok {
		switch msg.String() {
		case "esc":
			// Restore the filter that was active before editing started.
			m.filter = m.filterDraft
			m.filterInput.SetValue(m.filter)
			m.filterInput.Blur()
			m.mode = modeList
			return m.applyFilter(), nil
		case "enter":
			m.filter = strings.TrimSpace(m.filterInput.Value())
			m.filterInput.SetValue(m.filter)
			m.filterInput.Blur()
			m.mode = modeList
			return m.applyFilter(), nil
		}
	}

	var cmd tea.Cmd
	m.filterInput, cmd = m.filterInput.Update(message)
	// Filter as the user types so the result list is always in sync.
	m.filter = m.filterInput.Value()
	return m.applyFilter(), cmd
}

func (m Model) startRefresh() (tea.Model, tea.Cmd) {
	if m.refreshing {
		return m, nil
	}
	m.refreshing = true
	m.status = "Refreshing pull requests…"
	owner, limit := m.owner, m.limit
	return m, tea.Batch(m.spinner.Tick, func() tea.Msg {
		pulls, err := m.service.ListOpenPullRequests(m.ctx, owner, limit)
		return refreshedMsg{pulls: pulls, err: err}
	})
}

func (m Model) finishRefresh(message refreshedMsg) (tea.Model, tea.Cmd) {
	m.refreshing = false
	if message.err != nil {
		m.status = "Unable to refresh: " + message.err.Error()
		return m, nil
	}

	// Invalidate in-flight state loads for the previous list.
	m.generation++
	m.pulls = append([]githubapi.PullRequest(nil), message.pulls...)

	// Keep selections that still refer to an open pull request.
	live := make(map[string]bool, len(m.pulls))
	for _, pr := range m.pulls {
		live[pr.Key()] = true
	}
	for key := range m.selected {
		if !live[key] {
			delete(m.selected, key)
		}
	}

	// Carry over known states so the columns do not flash back to "loading",
	// then re-query everything to pick up new CI and review results.
	states := make(map[string]githubapi.PullRequestState, len(m.pulls))
	for _, pr := range m.pulls {
		if state, ok := m.states[pr.Key()]; ok {
			states[pr.Key()] = state
		}
	}
	m.states = states
	m.stateQueue = append([]githubapi.PullRequest(nil), m.pulls...)
	m.stateNext = 0
	m.stateWarning = ""

	m = m.applyFilter()
	m.status = fmt.Sprintf("Refreshed: %d open pull request(s).", len(m.pulls))
	return m, m.loadNextStateBatch()
}

func (m Model) openHighlightedURL() (tea.Model, tea.Cmd) {
	pr, ok := m.current()
	if !ok {
		return m, nil
	}

	m.status = "Opening " + pr.Key() + " in the default browser…"
	return m, func() tea.Msg {
		return urlOpenedMsg{key: pr.Key(), err: m.openURL(pr.URL)}
	}
}

func (m Model) openDiff() (tea.Model, tea.Cmd) {
	pr, ok := m.current()
	if !ok {
		return m, nil
	}

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.diffPR = pr
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
		return m.startBatch()
	case "n", "N", "esc":
		m.mode = modeList
		m.pending = actionNone
		m.comment = ""
	}
	return m, nil
}

func (m Model) startBatch() (tea.Model, tea.Cmd) {
	selected := m.selectedPulls()
	if len(selected) == 0 {
		m.mode = modeList
		m.pending = actionNone
		m.status = "Select one or more pull requests first."
		return m, nil
	}

	actionContext, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.mode = modeRunning
	m.status = ""
	m.batchGeneration++
	m.batchAction = m.pending
	m.batchTotal = len(selected)
	m.batchDone = 0
	m.batchFailed = 0
	m.batchCurrent = ""
	m.batchResults = nil

	// Buffer every event so the worker never blocks on a slow UI.
	events := make(chan tea.Msg, 2*len(selected)+1)
	m.batchEvents = events
	go m.runBatch(actionContext, events, m.pending, selected, m.comment, m.batchGeneration)

	return m, tea.Batch(m.spinner.Tick, waitForBatchEvent(events))
}

// waitForBatchEvent turns the next event on the channel into a message. The
// Update loop re-arms it after every event, which streams per-item progress
// into the UI instead of delivering one result at the very end.
func waitForBatchEvent(events chan tea.Msg) tea.Cmd {
	if events == nil {
		return nil
	}
	return func() tea.Msg {
		message, ok := <-events
		if !ok {
			return nil
		}
		return message
	}
}

// runBatch applies the action to each pull request in turn. Work stays
// sequential to be gentle on GitHub's write rate limits, but every step is
// reported as it happens.
func (m Model) runBatch(ctx context.Context, events chan<- tea.Msg, selectedAction action, pulls []githubapi.PullRequest, comment string, generation int) {
	defer close(events)

	results := make([]actionResult, 0, len(pulls))
	for index, pr := range pulls {
		if ctx.Err() != nil {
			break
		}
		events <- batchItemStartedMsg{generation: generation, index: index, pr: pr}

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
		events <- batchItemDoneMsg{generation: generation, result: result}
	}
	events <- batchFinishedMsg{generation: generation, action: selectedAction, results: results}
}

func (m Model) finishBatch(message batchFinishedMsg) Model {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.batchEvents = nil

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
		remaining := make([]githubapi.PullRequest, 0, len(m.pulls))
		for _, pr := range m.pulls {
			if !succeeded[pr.Key()] {
				remaining = append(remaining, pr)
			}
		}
		m.pulls = remaining
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
	if len(summary) == 0 {
		summary = append(summary, "nothing to do")
	}
	status := "Completed: " + strings.Join(summary, ", ")
	for index, failure := range failures {
		if index == 3 {
			status += fmt.Sprintf("\n…and %d more error(s)", len(failures)-index)
			break
		}
		status += "\n" + failure
	}

	m.mode = modeList
	m.pending = actionNone
	m.comment = ""
	m = m.applyFilter()
	m.status = status
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
	view.WriteString(m.renderCounts())
	view.WriteString("\n\n")

	if len(m.pulls) == 0 {
		view.WriteString("No open pull requests remain.\n")
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
		view.WriteString(m.renderProgress())
	default:
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
		view.WriteString(helpStyle.Render("↑/↓ navigate · space select · a all · / filter · R refresh · d diff · w web · m merge · c close · r request changes · q quit"))
		if pr, ok := m.current(); ok {
			view.WriteByte('\n')
			view.WriteString(helpStyle.Render(clamp(m.stateLabel(pr)+" · "+pr.URL, m.textWidth())))
		}
	}

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

func (m Model) renderProgress() string {
	var view strings.Builder
	verb := actionLabel(m.batchAction)
	fmt.Fprintf(&view, "%s %s · %d/%d", m.spinner.View(), verb, m.batchDone, m.batchTotal)
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

func actionLabel(current action) string {
	switch current {
	case actionMerge:
		return "Approving and merging"
	case actionClose:
		return "Closing"
	case actionRequestChanges:
		return "Requesting changes"
	default:
		return "Working"
	}
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

	repository := padRight(truncate(pr.Repository(), columns.repository), columns.repository)
	number := padRight(fmt.Sprintf("#%d", pr.Number), columns.number)
	prefix := fmt.Sprintf("%s%s %s %s %s %s", cursor, checkbox, repository, number, m.renderBuildGlyph(pr), m.renderReviewGlyph(pr))
	if columns.age > 0 {
		prefix += " " + m.renderAge(pr, columns.age)
	}
	if columns.author > 0 {
		prefix += " " + padRight(authorStyle.Render(truncate(pr.Author, columns.author)), columns.author)
	}

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

func (m Model) renderAge(pr githubapi.PullRequest, width int) string {
	age := formatAge(pr.UpdatedAt, m.now())
	style := helpStyle
	// Anything untouched for a month is worth calling out.
	if !pr.UpdatedAt.IsZero() && m.now().Sub(pr.UpdatedAt) > 30*24*time.Hour {
		style = staleStyle
	}
	return padRight(style.Render(truncate(age, width)), width)
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
	end := min(len(m.visible), start+pageSize)
	return start, end
}

func (m Model) pageSize() int {
	if m.height <= 0 {
		return 15
	}
	return max(3, m.height-10)
}

// textWidth is the width available for free-form text. Before the first
// WindowSizeMsg arrives the width is unknown, so nothing is truncated.
func (m Model) textWidth() int {
	if m.width <= 0 {
		return 0
	}
	return max(20, m.width)
}

// clamp truncates only when a width is known; width 0 means "unbounded".
func clamp(value string, width int) string {
	if width <= 0 {
		return value
	}
	return truncate(value, width)
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
