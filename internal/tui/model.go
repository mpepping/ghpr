package tui

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/mpepping/ghpr/internal/browser"
	"github.com/mpepping/ghpr/internal/githubapi"
)

// GitHubService is the part of the GitHub API used by the TUI.
type GitHubService interface {
	ListOpenPullRequests(context.Context, githubapi.SearchOptions) ([]githubapi.PullRequest, error)
	PullRequestStates(context.Context, []githubapi.PullRequest) (map[string]githubapi.PullRequestState, error)
	Diff(context.Context, githubapi.PullRequest) (string, error)
	Approve(context.Context, githubapi.PullRequest) error
	ApproveAndMerge(context.Context, githubapi.PullRequest) (githubapi.MergeOutcome, error)
	RequestChanges(context.Context, githubapi.PullRequest, string) error
	Comment(context.Context, githubapi.PullRequest, string) error
	UpdateBranch(context.Context, githubapi.PullRequest) error
	Close(context.Context, githubapi.PullRequest, string) error
}

const stateBatchSize = 25

type mode int

const (
	modeLoading mode = iota
	modeList
	modeFilter
	modeComment
	modeConfirm
	modeRunning
	modeDiffLoading
	modeDiff
	modeHelp
	modeLog
)

type action int

const (
	actionNone action = iota
	actionMerge
	actionApprove
	actionClose
	actionRequestChanges
	actionComment
	actionUpdateBranch
)

// needsBody reports whether the action collects a message before running.
func (a action) needsBody() bool {
	return a == actionClose || a == actionRequestChanges || a == actionComment
}

// requiresBody reports whether that message is mandatory.
func (a action) requiresBody() bool {
	return a == actionRequestChanges || a == actionComment
}

func (a action) verb() string {
	switch a {
	case actionMerge:
		return "approve and merge"
	case actionApprove:
		return "approve"
	case actionClose:
		return "close"
	case actionRequestChanges:
		return "request changes on"
	case actionComment:
		return "comment on"
	case actionUpdateBranch:
		return "update the branch of"
	default:
		return "act on"
	}
}

func (a action) progressLabel() string {
	switch a {
	case actionMerge:
		return "Approving and merging"
	case actionApprove:
		return "Approving"
	case actionClose:
		return "Closing"
	case actionRequestChanges:
		return "Requesting changes"
	case actionComment:
		return "Commenting"
	case actionUpdateBranch:
		return "Updating branches"
	default:
		return "Working"
	}
}

// removesPullRequest reports whether a successful action takes the pull
// request out of the list.
func (a action) removesPullRequest() bool {
	return a == actionMerge || a == actionClose
}

type actionResult struct {
	pr      githubapi.PullRequest
	outcome string
	err     error
}

type batchItemStartedMsg struct {
	generation int
	pr         githubapi.PullRequest
}

type batchItemDoneMsg struct {
	generation int
	result     actionResult
}

type batchFinishedMsg struct {
	generation int
	action     action
	results    []actionResult
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

type pullsLoadedMsg struct {
	pulls   []githubapi.PullRequest
	err     error
	initial bool
}

type editorFinishedMsg struct {
	body string
	err  error
}

// logEntry is one line in the session log.
type logEntry struct {
	at      time.Time
	message string
	failed  bool
}

// Options configures a Model.
type Options struct {
	Context     context.Context
	Service     GitHubService
	Search      githubapi.SearchOptions
	MergeMethod githubapi.MergeMethod
	DryRun      bool
	Filter      string
	Editor      string
}

// Model is the ghpr Bubble Tea model.
type Model struct {
	ctx         context.Context
	service     GitHubService
	search      githubapi.SearchOptions
	mergeMethod githubapi.MergeMethod
	dryRun      bool
	editor      string
	now         func() time.Time

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
	input       textarea.Model
	filterInput textinput.Model
	filter      string
	filterDraft string
	spinner     spinner.Model
	viewport    viewport.Model
	openURL     func(string) error

	diff      diffState
	diffCache map[string]string

	states       map[string]githubapi.PullRequestState
	stateQueue   []githubapi.PullRequest
	stateNext    int
	stateWarning string

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

	log    []logEntry
	cancel context.CancelFunc
	status string
}

// New creates a pull request model. The pull request list is loaded by Init so
// the user sees a spinner instead of a frozen terminal.
func New(options Options) Model {
	body := textarea.New()
	body.CharLimit = 4000
	body.SetHeight(4)
	body.ShowLineNumbers = false

	filter := textinput.New()
	filter.Prompt = "/"
	filter.CharLimit = 100
	filter.Width = 60

	activity := spinner.New()
	activity.Spinner = spinner.Dot
	activity.Style = statusStyle

	diffViewport := viewport.New(80, 20)
	diffViewport.SetHorizontalStep(8)

	mergeMethod := options.MergeMethod
	if mergeMethod == "" {
		mergeMethod = githubapi.MergeMethodSquash
	}

	model := Model{
		ctx:         options.Context,
		service:     options.Service,
		search:      options.Search,
		mergeMethod: mergeMethod,
		dryRun:      options.DryRun,
		editor:      options.Editor,
		now:         time.Now,
		selected:    make(map[string]bool),
		input:       body,
		filterInput: filter,
		filter:      strings.TrimSpace(options.Filter),
		spinner:     activity,
		viewport:    diffViewport,
		openURL:     browser.Open,
		diffCache:   make(map[string]string),
		states:      make(map[string]githubapi.PullRequestState),
		mode:        modeLoading,
	}
	if model.filter != "" {
		model.filterInput.SetValue(model.filter)
	}
	return model
}

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.spinner.Tick, m.loadPulls(true))
}

func (m Model) loadPulls(initial bool) tea.Cmd {
	search := m.search
	return func() tea.Msg {
		pulls, err := m.service.ListOpenPullRequests(m.ctx, search)
		return pullsLoadedMsg{pulls: pulls, err: err, initial: initial}
	}
}

// Update implements tea.Model.
func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.input.SetWidth(max(20, min(100, msg.Width-6)))
		m.filterInput.Width = max(20, min(80, msg.Width-6))
		m.viewport.Width = max(1, msg.Width)
		m.viewport.Height = max(1, msg.Height-4)
		return m, nil
	case pullsLoadedMsg:
		return m.finishLoad(msg)
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
		m = m.recordResult(msg.result)
		return m, waitForBatchEvent(m.batchEvents)
	case batchFinishedMsg:
		if msg.generation != m.batchGeneration {
			return m, nil
		}
		return m.finishBatch(msg), nil
	case diffLoadedMsg:
		return m.finishDiff(msg), nil
	case editorFinishedMsg:
		return m.finishEditor(msg)
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
		if m.refreshing || m.mode == modeRunning || m.mode == modeDiffLoading || m.mode == modeLoading {
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
	case modeLoading:
		if msg, ok := message.(tea.KeyMsg); ok && msg.String() == "q" {
			return m, tea.Quit
		}
		var cmd tea.Cmd
		m.spinner, cmd = m.spinner.Update(message)
		return m, cmd
	case modeHelp, modeLog:
		return m.updateOverlay(message)
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

func (m Model) finishLoad(message pullsLoadedMsg) (tea.Model, tea.Cmd) {
	m.refreshing = false
	if message.err != nil {
		if message.initial {
			m.mode = modeList
			m.status = "Unable to load pull requests: " + message.err.Error()
			return m, nil
		}
		m.status = "Unable to refresh: " + message.err.Error()
		return m, nil
	}

	// Invalidate in-flight state loads for the previous list.
	m.generation++
	m.pulls = append([]githubapi.PullRequest(nil), message.pulls...)
	if m.mode == modeLoading {
		m.mode = modeList
	}

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
	// Diffs may have changed since they were cached.
	m.diffCache = make(map[string]string)

	m = m.applyFilter()
	if message.initial {
		m.status = ""
	} else {
		m.status = fmt.Sprintf("Refreshed: %d open pull request(s).", len(m.pulls))
	}
	return m, m.loadNextStateBatch()
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
		m.log = append(m.log, logEntry{at: m.now(), message: "state query: " + message.err.Error(), failed: true})
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
		if m.filter != "" {
			m.filter = ""
			m.filterInput.Reset()
			m.status = ""
			return m.applyFilter(), nil
		}
		return m, nil
	case "?":
		m.mode = modeHelp
		m.viewport.SetContent(helpContent())
		m.viewport.GotoTop()
		return m, nil
	case "L":
		m.mode = modeLog
		m.viewport.SetContent(m.logContent())
		m.viewport.GotoBottom()
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
	case "A":
		return m.prepareAction(actionApprove)
	case "c":
		return m.prepareAction(actionClose)
	case "r":
		return m.prepareAction(actionRequestChanges)
	case "C":
		return m.prepareAction(actionComment)
	case "u":
		return m.prepareAction(actionUpdateBranch)
	case "d":
		return m.openDiff()
	case "w":
		return m.openHighlightedURL()
	}
	return m, nil
}

// updateOverlay drives the help and log viewers.
func (m Model) updateOverlay(message tea.Msg) (tea.Model, tea.Cmd) {
	if msg, ok := message.(tea.KeyMsg); ok {
		switch msg.String() {
		case "esc", "q", "?", "L":
			m.mode = modeList
			m.viewport.SetContent("")
			return m, nil
		}
	}
	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(message)
	return m, cmd
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
	m.filter = m.filterInput.Value()
	return m.applyFilter(), cmd
}

func (m Model) startRefresh() (tea.Model, tea.Cmd) {
	if m.refreshing {
		return m, nil
	}
	m.refreshing = true
	m.status = "Refreshing pull requests…"
	return m, tea.Batch(m.spinner.Tick, m.loadPulls(false))
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

func (m Model) prepareAction(next action) (tea.Model, tea.Cmd) {
	if len(m.selected) == 0 {
		m.status = "Select one or more pull requests first."
		return m, nil
	}

	m.pending = next
	m.comment = ""
	m.status = ""
	if !next.needsBody() {
		m.mode = modeConfirm
		return m, nil
	}

	m.mode = modeComment
	m.input.Reset()
	switch next {
	case actionClose:
		m.input.Placeholder = "Optional comment before closing"
	case actionComment:
		m.input.Placeholder = "Comment to post (required)"
	default:
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
		case "ctrl+e":
			return m.openEditor()
		case "ctrl+d":
			m.comment = strings.TrimSpace(m.input.Value())
			if m.pending.requiresBody() && m.comment == "" {
				m.status = "A message is required for this action."
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

// openEditor hands the review body to $EDITOR so long, multi-line and
// markdown-heavy messages can be written comfortably.
func (m Model) openEditor() (tea.Model, tea.Cmd) {
	editor := m.editorCommand()
	if editor == "" {
		m.status = "Set $EDITOR (or editor: in the config file) to compose in an external editor."
		return m, nil
	}

	file, err := os.CreateTemp("", "ghpr-*.md")
	if err != nil {
		m.status = "Unable to create a temporary file: " + err.Error()
		return m, nil
	}
	name := file.Name()
	if _, err := file.WriteString(m.input.Value()); err != nil {
		file.Close()
		os.Remove(name)
		m.status = "Unable to write the temporary file: " + err.Error()
		return m, nil
	}
	file.Close()

	parts := strings.Fields(editor)
	command := exec.Command(parts[0], append(parts[1:], name)...) // #nosec G204 -- the editor comes from the user's own configuration
	return m, tea.ExecProcess(command, func(err error) tea.Msg {
		defer os.Remove(name)
		if err != nil {
			return editorFinishedMsg{err: err}
		}
		contents, readErr := os.ReadFile(name) // #nosec G304 -- path created by this process
		if readErr != nil {
			return editorFinishedMsg{err: readErr}
		}
		return editorFinishedMsg{body: string(contents)}
	})
}

func (m Model) editorCommand() string {
	if m.editor != "" {
		return m.editor
	}
	for _, name := range []string{"GHPR_EDITOR", "VISUAL", "EDITOR"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			return value
		}
	}
	// A sensible last resort that exists nearly everywhere.
	if path, err := exec.LookPath("vi"); err == nil {
		return filepath.Base(path)
	}
	return ""
}

func (m Model) finishEditor(message editorFinishedMsg) (tea.Model, tea.Cmd) {
	if message.err != nil {
		m.status = "Editor failed: " + message.err.Error()
		return m, nil
	}
	m.input.SetValue(strings.TrimRight(message.body, "\n"))
	m.input.CursorEnd()
	return m, m.input.Focus()
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
	for _, pr := range pulls {
		if ctx.Err() != nil {
			break
		}
		events <- batchItemStartedMsg{generation: generation, pr: pr}

		result := actionResult{pr: pr}
		switch selectedAction {
		case actionMerge:
			outcome, err := m.service.ApproveAndMerge(ctx, pr)
			result.outcome = string(outcome)
			result.err = err
		case actionApprove:
			result.outcome = "approved"
			result.err = m.service.Approve(ctx, pr)
		case actionClose:
			result.outcome = "closed"
			result.err = m.service.Close(ctx, pr, comment)
		case actionRequestChanges:
			result.outcome = "changes requested"
			result.err = m.service.RequestChanges(ctx, pr, comment)
		case actionComment:
			result.outcome = "commented"
			result.err = m.service.Comment(ctx, pr, comment)
		case actionUpdateBranch:
			result.outcome = "branch updated"
			result.err = m.service.UpdateBranch(ctx, pr)
		}
		results = append(results, result)
		events <- batchItemDoneMsg{generation: generation, result: result}
	}
	events <- batchFinishedMsg{generation: generation, action: selectedAction, results: results}
}

// recordResult appends an entry to the session log so nothing is lost when the
// status line is overwritten.
func (m Model) recordResult(result actionResult) Model {
	entry := logEntry{at: m.now()}
	if result.err != nil {
		entry.failed = true
		entry.message = result.pr.Key() + ": " + result.err.Error()
	} else {
		entry.message = result.pr.Key() + " " + result.outcome
	}
	m.log = append(m.log, entry)
	return m
}

func (m Model) finishBatch(message batchFinishedMsg) Model {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.batchEvents = nil

	succeeded := make(map[string]bool)
	failures := 0
	outcomes := make(map[string]int)

	for _, result := range message.results {
		if result.err != nil {
			failures++
			continue
		}
		succeeded[result.pr.Key()] = true
		outcomes[result.outcome]++
		delete(m.selected, result.pr.Key())
	}

	if message.action.removesPullRequest() && !m.dryRun {
		remaining := make([]githubapi.PullRequest, 0, len(m.pulls))
		for _, pr := range m.pulls {
			if !succeeded[pr.Key()] {
				remaining = append(remaining, pr)
			}
		}
		m.pulls = remaining
		m.stateQueue = pruneQueue(m.stateQueue, succeeded, &m.stateNext)
	}

	summary := make([]string, 0, len(outcomes)+1)
	for _, name := range []string{"merged", "auto-merge enabled", "dry run", "approved", "closed", "changes requested", "commented", "branch updated"} {
		if count := outcomes[name]; count > 0 {
			summary = append(summary, fmt.Sprintf("%d %s", count, name))
		}
	}
	if failures > 0 {
		summary = append(summary, fmt.Sprintf("%d failed", failures))
	}
	if len(summary) == 0 {
		summary = append(summary, "nothing to do")
	}
	status := "Completed: " + strings.Join(summary, ", ")
	if failures > 0 {
		status += "  (press L for the full log)"
	}

	m.mode = modeList
	m.pending = actionNone
	m.comment = ""
	m = m.applyFilter()
	m.status = status
	return m
}

// pruneQueue drops finished pull requests from the pending state queue so no
// requests are made for pull requests that already left the list.
func pruneQueue(queue []githubapi.PullRequest, done map[string]bool, next *int) []githubapi.PullRequest {
	if len(done) == 0 {
		return queue
	}
	pruned := make([]githubapi.PullRequest, 0, len(queue))
	removedBeforeCursor := 0
	for index, pr := range queue {
		if done[pr.Key()] {
			if index < *next {
				removedBeforeCursor++
			}
			continue
		}
		pruned = append(pruned, pr)
	}
	*next = max(0, *next-removedBeforeCursor)
	return pruned
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

func (m Model) pageSize() int {
	if m.height <= 0 {
		return 15
	}
	return max(3, m.height-10)
}
