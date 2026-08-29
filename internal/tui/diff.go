package tui

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/mpepping/ghpr/internal/githubapi"
)

// maxDiffBytes caps how much of a diff is rendered. Colorizing a multi-megabyte
// diff costs a lot of memory and makes scrolling sluggish, and nobody reviews
// that much in a pager anyway.
const maxDiffBytes = 2 << 20

type diffLoadedMsg struct {
	key     string
	content string
	err     error
}

// diffState holds everything about the diff currently open.
type diffState struct {
	pr        githubapi.PullRequest
	lines     []string // rendered, colorized lines
	plain     []string // same lines without styling, used for searching
	fileLines []int    // indexes of "diff --git" lines, for ] and [
	truncated bool

	search      string
	searching   bool
	searchDraft string
	matches     []int
	matchIndex  int
}

func (m Model) openDiff() (tea.Model, tea.Cmd) {
	pr, ok := m.current()
	if !ok {
		return m, nil
	}

	m.diff = diffState{pr: pr}
	m.status = ""
	m.viewport.SetContent("")
	m.viewport.GotoTop()

	// Diffs do not change while the pull request list is loaded, so a cached
	// copy can be shown immediately.
	if cached, hit := m.diffCache[pr.Key()]; hit {
		m.mode = modeDiff
		return m.showDiff(cached), nil
	}

	ctx, cancel := context.WithCancel(m.ctx)
	m.cancel = cancel
	m.mode = modeDiffLoading
	return m, tea.Batch(m.spinner.Tick, m.loadDiff(ctx, pr))
}

func (m Model) loadDiff(ctx context.Context, pr githubapi.PullRequest) tea.Cmd {
	return func() tea.Msg {
		content, err := m.service.Diff(ctx, pr)
		return diffLoadedMsg{key: pr.Key(), content: content, err: err}
	}
}

func (m Model) finishDiff(message diffLoadedMsg) Model {
	if m.mode != modeDiffLoading || message.key != m.diff.pr.Key() {
		return m
	}
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	if message.err != nil {
		m.status = "Unable to load diff: " + message.err.Error()
		m.mode = modeList
		m.diff = diffState{}
		return m
	}

	m.diffCache[message.key] = message.content
	m.mode = modeDiff
	return m.showDiff(message.content)
}

// showDiff prepares the viewport content for an already fetched diff.
func (m Model) showDiff(content string) Model {
	if strings.TrimSpace(content) == "" {
		content = "No changes in this pull request."
	}
	if len(content) > maxDiffBytes {
		content = content[:maxDiffBytes]
		// Drop the partial final line so highlighting stays sane.
		if cut := strings.LastIndexByte(content, '\n'); cut > 0 {
			content = content[:cut]
		}
		m.diff.truncated = true
	}

	content = strings.ReplaceAll(content, "\r\n", "\n")
	plain := strings.Split(content, "\n")
	rendered := make([]string, len(plain))
	files := make([]int, 0, 16)
	for index, line := range plain {
		if strings.HasPrefix(line, "diff --git ") {
			files = append(files, index)
		}
		rendered[index] = styleDiffLine(line)
	}
	if m.diff.truncated {
		notice := fmt.Sprintf("… diff truncated at %d MB; open it on GitHub with w to see the rest", maxDiffBytes>>20)
		plain = append(plain, "", notice)
		rendered = append(rendered, "", warnStyle.Render(notice))
	}

	m.diff.plain = plain
	m.diff.lines = rendered
	m.diff.fileLines = files
	m.diff.matches = nil
	m.diff.matchIndex = 0
	m.viewport.SetContent(strings.Join(rendered, "\n"))
	m.viewport.GotoTop()
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
	msg, ok := message.(tea.KeyMsg)
	if !ok {
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(message)
		return m, cmd
	}

	// While typing a search query the viewport must not consume the keys.
	if m.diff.searching {
		return m.updateDiffSearch(msg)
	}

	switch msg.String() {
	case "esc", "q":
		return m.closeDiff(), nil
	case "/":
		m.diff.searching = true
		m.diff.searchDraft = ""
		return m, nil
	case "n":
		return m.jumpToMatch(1), nil
	case "N":
		return m.jumpToMatch(-1), nil
	case "]", "}":
		return m.jumpToFile(1), nil
	case "[", "{":
		return m.jumpToFile(-1), nil
	case "w":
		pr := m.diff.pr
		return m, func() tea.Msg {
			return urlOpenedMsg{key: pr.Key(), err: m.openURL(pr.URL)}
		}
	}

	var cmd tea.Cmd
	m.viewport, cmd = m.viewport.Update(message)
	return m, cmd
}

func (m Model) updateDiffSearch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.diff.searching = false
		m.diff.searchDraft = ""
		return m, nil
	case "enter":
		m.diff.searching = false
		m.diff.search = m.diff.searchDraft
		m.diff.searchDraft = ""
		return m.runSearch(), nil
	case "backspace":
		if m.diff.searchDraft != "" {
			runes := []rune(m.diff.searchDraft)
			m.diff.searchDraft = string(runes[:len(runes)-1])
		}
		return m, nil
	}
	if msg.Type == tea.KeyRunes || msg.Type == tea.KeySpace {
		m.diff.searchDraft += string(msg.Runes)
		if msg.Type == tea.KeySpace {
			m.diff.searchDraft += " "
		}
	}
	return m, nil
}

// runSearch collects the matching line numbers and jumps to the first one at
// or after the current position.
func (m Model) runSearch() Model {
	m.diff.matches = nil
	m.diff.matchIndex = 0
	needle := strings.ToLower(strings.TrimSpace(m.diff.search))
	if needle == "" {
		m.viewport.SetContent(strings.Join(m.diff.lines, "\n"))
		return m
	}

	for index, line := range m.diff.plain {
		if strings.Contains(strings.ToLower(line), needle) {
			m.diff.matches = append(m.diff.matches, index)
		}
	}
	if len(m.diff.matches) == 0 {
		m.status = "No match for " + m.diff.search
		return m
	}

	// Highlight every match so they stand out while scrolling.
	highlighted := make([]string, len(m.diff.lines))
	copy(highlighted, m.diff.lines)
	for _, index := range m.diff.matches {
		highlighted[index] = matchStyle.Render(m.diff.plain[index])
	}
	m.viewport.SetContent(strings.Join(highlighted, "\n"))

	// Start from the first match below the current viewport position.
	m.diff.matchIndex = 0
	for position, line := range m.diff.matches {
		if line >= m.viewport.YOffset {
			m.diff.matchIndex = position
			break
		}
	}
	m.viewport.SetYOffset(max(0, m.diff.matches[m.diff.matchIndex]-2))
	m.status = ""
	return m
}

func (m Model) jumpToMatch(direction int) Model {
	if len(m.diff.matches) == 0 {
		return m
	}
	m.diff.matchIndex = (m.diff.matchIndex + direction + len(m.diff.matches)) % len(m.diff.matches)
	m.viewport.SetYOffset(max(0, m.diff.matches[m.diff.matchIndex]-2))
	return m
}

func (m Model) jumpToFile(direction int) Model {
	if len(m.diff.fileLines) == 0 {
		return m
	}
	current := m.viewport.YOffset
	if direction > 0 {
		for _, line := range m.diff.fileLines {
			if line > current {
				m.viewport.SetYOffset(line)
				return m
			}
		}
		m.viewport.SetYOffset(m.diff.fileLines[len(m.diff.fileLines)-1])
		return m
	}
	for index := len(m.diff.fileLines) - 1; index >= 0; index-- {
		if m.diff.fileLines[index] < current {
			m.viewport.SetYOffset(m.diff.fileLines[index])
			return m
		}
	}
	m.viewport.GotoTop()
	return m
}

func (m Model) closeDiff() Model {
	if m.cancel != nil {
		m.cancel()
		m.cancel = nil
	}
	m.mode = modeList
	m.diff = diffState{}
	m.viewport.SetContent("")
	m.status = ""
	return m
}

func styleDiffLine(line string) string {
	switch {
	case strings.HasPrefix(line, "diff --git "):
		return diffFileStyle.Render(line)
	case strings.HasPrefix(line, "@@"):
		return diffHeaderStyle.Render(line)
	case strings.HasPrefix(line, "--- "), strings.HasPrefix(line, "+++ "):
		return diffHeaderStyle.Render(line)
	case strings.HasPrefix(line, "+"):
		return diffAddStyle.Render(line)
	case strings.HasPrefix(line, "-"):
		return diffDeleteStyle.Render(line)
	case strings.HasPrefix(line, "index "),
		strings.HasPrefix(line, "new file mode "),
		strings.HasPrefix(line, "deleted file mode "),
		strings.HasPrefix(line, "similarity index "),
		strings.HasPrefix(line, "rename from "),
		strings.HasPrefix(line, "rename to "),
		strings.HasPrefix(line, "Binary files "),
		strings.HasPrefix(line, "\\ No newline at end of file"):
		return diffMetaStyle.Render(line)
	}
	return line
}
