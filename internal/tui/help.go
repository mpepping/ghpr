package tui

import (
	"fmt"
	"strings"
)

type helpSection struct {
	title string
	keys  [][2]string
}

var helpSections = []helpSection{
	{"Navigation", [][2]string{
		{"↑/↓, j/k", "move the cursor"},
		{"g / G", "jump to the first or last pull request"},
		{"pgup / pgdn", "move one page"},
		{"q", "quit"},
	}},
	{"Selection", [][2]string{
		{"space", "select or deselect the pull request"},
		{"a", "select or deselect every visible pull request"},
		{"/", "filter by repository, number, title, author or \"draft\""},
		{"esc", "clear the active filter"},
	}},
	{"Inspect", [][2]string{
		{"d", "open the diff"},
		{"w", "open the pull request in a browser"},
		{"R, ctrl+r", "reload pull requests and statuses"},
		{"L", "show the session log"},
		{"?", "show this help"},
	}},
	{"Actions (apply to the selection)", [][2]string{
		{"m", "approve and merge"},
		{"A", "approve only"},
		{"c", "close, with an optional comment"},
		{"r", "request changes, with a required reason"},
		{"C", "post a comment"},
		{"u", "update the branch from its base"},
	}},
	{"Prompts", [][2]string{
		{"ctrl+d", "accept the message and continue"},
		{"ctrl+e", "compose the message in $EDITOR"},
		{"enter", "insert a newline in a message"},
		{"y / enter", "confirm an action"},
		{"n / esc", "cancel"},
	}},
	{"Diff viewer", [][2]string{
		{"space, pgdn", "page down"},
		{"pgup", "page up"},
		{"/", "search the diff"},
		{"n / N", "next or previous match"},
		{"] / [", "next or previous file"},
		{"w", "open the pull request in a browser"},
		{"esc, q", "back to the list"},
	}},
}

func helpContent() string {
	var content strings.Builder
	for index, section := range helpSections {
		if index > 0 {
			content.WriteByte('\n')
		}
		content.WriteString(titleStyle.Render(section.title))
		content.WriteByte('\n')
		for _, entry := range section.keys {
			fmt.Fprintf(&content, "  %s  %s\n", padRight(entry[0], 12), helpStyle.Render(entry[1]))
		}
	}

	content.WriteByte('\n')
	content.WriteString(titleStyle.Render("Columns"))
	content.WriteByte('\n')
	for _, line := range [][2]string{
		{"CI", "checks rollup: ✓ passed · … running · ✗ failed · ? unreadable · – none"},
		{"RV", "merge readiness: ⚠ conflicts · ✓ approved · ✗ changes requested · ○ review required"},
		{"AGE", "time since the pull request was last updated"},
	} {
		fmt.Fprintf(&content, "  %s  %s\n", padRight(line[0], 12), helpStyle.Render(line[1]))
	}
	return content.String()
}

// logContent renders the session log, newest last.
func (m Model) logContent() string {
	if len(m.log) == 0 {
		return helpStyle.Render("Nothing has happened yet in this session.")
	}
	var content strings.Builder
	for _, entry := range m.log {
		stamp := entry.at.Format("15:04:05")
		if entry.failed {
			fmt.Fprintf(&content, "%s %s\n", helpStyle.Render(stamp), errorStyle.Render("✗ "+entry.message))
			continue
		}
		fmt.Fprintf(&content, "%s %s\n", helpStyle.Render(stamp), selectedStyle.Render("✓ "+entry.message))
	}
	return content.String()
}
