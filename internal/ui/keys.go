package ui

import "github.com/charmbracelet/bubbles/key"

// KeyMap is every binding in the application. It is one flat map rather than
// one per screen so the same action never gets two different keys on two
// screens, and so the help overlay can be rendered from a single source.
//
// Screens read the bindings they use and ignore the rest.
type KeyMap struct {
	// Movement, shared by every list and the viewport.
	Up       key.Binding
	Down     key.Binding
	PageUp   key.Binding
	PageDown key.Binding
	Top      key.Binding
	Bottom   key.Binding

	// Commit and cancel.
	Enter key.Binding
	Back  key.Binding
	Quit  key.Binding
	Help  key.Binding

	// Picker.
	Filter key.Binding
	Next   key.Binding
	Prev   key.Binding

	// Hosts screen.
	Delete  key.Binding
	Pin     key.Binding
	NewHost key.Binding

	// Browser screen.
	Fetch  key.Binding
	Follow key.Binding

	// Viewer screen.
	Search    key.Binding
	NextMatch key.Binding
	PrevMatch key.Binding
	ToggleRe  key.Binding
}

// DefaultKeyMap returns the bindings the application ships with. The picker
// movement keys follow Helix: ctrl+n and ctrl+p move the selection.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		PageUp: key.NewBinding(
			key.WithKeys("pgup", "ctrl+b"),
			key.WithHelp("pgup", "page up"),
		),
		PageDown: key.NewBinding(
			key.WithKeys("pgdown", "ctrl+f"),
			key.WithHelp("pgdn", "page down"),
		),
		Top: key.NewBinding(
			key.WithKeys("g"),
			key.WithHelp("g", "top"),
		),
		Bottom: key.NewBinding(
			key.WithKeys("G"),
			key.WithHelp("G", "bottom"),
		),

		Enter: key.NewBinding(
			key.WithKeys("enter"),
			key.WithHelp("enter", "open"),
		),
		Back: key.NewBinding(
			key.WithKeys("esc"),
			key.WithHelp("esc", "back"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),

		Filter: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "filter"),
		),
		Next: key.NewBinding(
			key.WithKeys("ctrl+n"),
			key.WithHelp("ctrl+n", "next"),
		),
		Prev: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "prev"),
		),

		Delete: key.NewBinding(
			key.WithKeys("ctrl+d"),
			key.WithHelp("ctrl+d", "delete"),
		),
		Pin: key.NewBinding(
			key.WithKeys("ctrl+p"),
			key.WithHelp("ctrl+p", "pin"),
		),
		NewHost: key.NewBinding(
			key.WithKeys("ctrl+n"),
			key.WithHelp("ctrl+n", "new host"),
		),

		Fetch: key.NewBinding(
			key.WithKeys("f"),
			key.WithHelp("f", "fetch"),
		),
		Follow: key.NewBinding(
			key.WithKeys("F"),
			key.WithHelp("F", "follow"),
		),

		Search: key.NewBinding(
			key.WithKeys("/"),
			key.WithHelp("/", "search"),
		),
		NextMatch: key.NewBinding(
			key.WithKeys("n"),
			key.WithHelp("n", "next match"),
		),
		PrevMatch: key.NewBinding(
			key.WithKeys("N"),
			key.WithHelp("N", "prev match"),
		),
		ToggleRe: key.NewBinding(
			key.WithKeys("ctrl+r"),
			key.WithHelp("ctrl+r", "toggle regex"),
		),
	}
}

// ShortHelp returns the bindings for the single help line, satisfying the
// help.KeyMap interface.
func (k KeyMap) ShortHelp() []key.Binding {
	return []key.Binding{k.Up, k.Down, k.Enter, k.Back, k.Help, k.Quit}
}

// FullHelp returns the bindings for the help overlay, grouped into columns,
// satisfying the help.KeyMap interface.
func (k KeyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.PageUp, k.PageDown, k.Top, k.Bottom},
		{k.Enter, k.Back, k.Filter, k.Next, k.Prev},
		{k.Delete, k.Pin, k.NewHost},
		{k.Fetch, k.Follow},
		{k.Search, k.NextMatch, k.PrevMatch, k.ToggleRe},
		{k.Help, k.Quit},
	}
}
