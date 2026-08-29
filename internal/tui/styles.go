package tui

import "github.com/charmbracelet/lipgloss"

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
	warnStyle         = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("214"))
	matchStyle        = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("0")).Background(lipgloss.Color("214"))
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

// DisableColor strips every color from the interface while keeping the bold
// and reverse attributes that carry meaning. It backs both the --no-color flag
// and the NO_COLOR convention.
//
// Every status is also encoded as a distinct glyph, so the UI stays readable
// without color for colorblind users and on monochrome terminals.
func DisableColor() {
	plain := lipgloss.NewStyle()
	bold := lipgloss.NewStyle().Bold(true)
	reverse := lipgloss.NewStyle().Reverse(true)

	titleStyle = bold
	helpStyle = plain
	selectedStyle = plain
	cursorStyle = bold
	errorStyle = bold
	statusStyle = plain
	draftStyle = plain
	filterStyle = bold
	staleStyle = plain
	authorStyle = plain
	warnStyle = bold
	matchStyle = reverse
	diffFileStyle = bold
	diffHeaderStyle = bold
	diffAddStyle = plain
	diffDeleteStyle = plain
	diffMetaStyle = plain
	buildSuccessStyle = plain
	buildPendingStyle = plain
	buildFailureStyle = bold
	buildUnknownStyle = plain
}
