package ui

import "github.com/charmbracelet/lipgloss"

// Theme is every style the application renders with. Screens take a Theme
// rather than reaching for package level styles, so the styles are injectable
// and there is no package level state.
type Theme struct {
	// Chrome drawn by the root.
	Title  lipgloss.Style
	Status lipgloss.Style
	Error  lipgloss.Style
	Help   lipgloss.Style

	// Panes and their borders. Active is the pane with focus.
	PaneActive   lipgloss.Style
	PaneInactive lipgloss.Style

	// List rows.
	Row      lipgloss.Style
	RowFocus lipgloss.Style
	// Match highlights the characters a fuzzy query matched.
	Match lipgloss.Style
	// Dim is for secondary detail such as sizes and ages.
	Dim lipgloss.Style

	// Log content in the preview and the viewer.
	Line lipgloss.Style
	// LineMatch highlights an in-file search hit.
	LineMatch lipgloss.Style
}

// DefaultTheme returns the styles the application ships with. It uses adaptive
// colours, so the same theme is legible on a light and on a dark terminal.
func DefaultTheme() Theme {
	adaptive := func(light, dark string) lipgloss.AdaptiveColor {
		return lipgloss.AdaptiveColor{Light: light, Dark: dark}
	}

	fg := adaptive("235", "255")
	dim := adaptive("246", "243")
	accent := adaptive("25", "111")
	match := adaptive("94", "220")
	danger := adaptive("124", "203")

	return Theme{
		Title:  lipgloss.NewStyle().Bold(true).Foreground(fg),
		Status: lipgloss.NewStyle().Foreground(dim),
		Error:  lipgloss.NewStyle().Bold(true).Foreground(danger),
		Help:   lipgloss.NewStyle().Foreground(dim),

		PaneActive: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(accent),
		PaneInactive: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(dim),

		Row:      lipgloss.NewStyle().Foreground(fg),
		RowFocus: lipgloss.NewStyle().Bold(true).Foreground(fg),
		Match:    lipgloss.NewStyle().Bold(true).Foreground(match),
		Dim:      lipgloss.NewStyle().Foreground(dim),

		Line:      lipgloss.NewStyle().Foreground(fg),
		LineMatch: lipgloss.NewStyle().Bold(true).Foreground(match),
	}
}

// WithColors applies user-configured palette colors to a base theme. Primary
// drives titles, focused controls and active borders. Secondary drives warning
// and match/highlight states. Empty values preserve the adaptive defaults.
func (t Theme) WithColors(primary, secondary string) Theme {
	if primary != "" {
		color := lipgloss.Color(primary)
		t.Title = t.Title.Foreground(color)
		t.PaneActive = t.PaneActive.BorderForeground(color)
		t.RowFocus = t.RowFocus.Foreground(color)
	}
	if secondary != "" {
		color := lipgloss.Color(secondary)
		t.Error = t.Error.Foreground(color)
		t.Match = t.Match.Foreground(color)
		t.LineMatch = t.LineMatch.Foreground(color)
	}
	return t
}
