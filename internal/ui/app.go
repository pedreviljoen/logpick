package ui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Screen identifies one of the application's four screens.
type Screen int

// The screens, in the order of the flow through them: hosts to browser to
// library to viewer.
const (
	// ScreenHosts is the fuzzy list of host history and the first-run flow.
	ScreenHosts Screen = iota
	// ScreenBrowser is the split pane log browser, the main screen.
	ScreenBrowser
	// ScreenLibrary is the fuzzy list of locally fetched files.
	ScreenLibrary
	// ScreenViewer is the local file view with in-file search.
	ScreenViewer
)

// numScreens is how many screens the root holds. It is the length of the array
// in App, so every Screen value is a valid index into it.
const numScreens = 4

// String returns the screen's lowercase name: hosts, browser, library or
// viewer. An unknown value renders as the word unknown followed by its number.
func (s Screen) String() string {
	switch s {
	case ScreenHosts:
		return "hosts"
	case ScreenBrowser:
		return "browser"
	case ScreenLibrary:
		return "library"
	case ScreenViewer:
		return "viewer"
	default:
		return fmt.Sprintf("unknown %d", int(s))
	}
}

type verticalLayout struct {
	top, content, error, bottom int
}

func calculateVerticalLayout(height int) verticalLayout {
	if height <= 0 {
		return verticalLayout{}
	}
	// The content starts 5% from the top and occupies 75% of the terminal.
	// The error and bottom regions each receive the remaining 10%.
	top := (height + 10) / 20
	content := (height*3 + 2) / 4
	errorHeight := (height + 5) / 10
	if top > height {
		top = height
	}
	if errorHeight > height-top {
		errorHeight = height - top
	}
	if content > height-top-errorHeight {
		content = height - top - errorHeight
	}
	return verticalLayout{
		top:     top,
		content: content,
		error:   errorHeight,
		bottom:  height - top - content - errorHeight,
	}
}

// ContentHeight returns the 75% terminal-height region assigned to screens.
func ContentHeight(height int) int {
	return calculateVerticalLayout(height).content
}

// ScreenModel is what the root needs from a screen. Every screen package
// implements it, and the root holds nothing else about them.
//
// It is deliberately not tea.Model. A tea.Model returns tea.Model from Update,
// which would force the root to type assert the result back to a screen on
// every message and to decide what to do when that assertion fails. Returning
// the interface the root stores makes the routing total instead.
//
// Screens are values. Update and Resize return the updated screen rather than
// mutating the receiver, so the root can hold four of them without one screen's
// update being visible to another.
type ScreenModel interface {
	// Init returns the command to run when the screen is first shown, or nil.
	Init() tea.Cmd

	// Update handles a message and returns the updated screen. It never blocks:
	// work is returned as a tea.Cmd.
	Update(msg tea.Msg) (ScreenModel, tea.Cmd)

	// View renders the screen into the area it was last resized to.
	View() string

	// Resize tells the screen the area it has to render into, which is the
	// terminal size less the root's own chrome, and returns the updated screen.
	Resize(width, height int) ScreenModel
}

// App is the root model. It owns the terminal dimensions, the error banner and
// which screen is active, and routes everything else to that screen.
//
// The zero value is not useful: use New, or in a test a literal with Screens
// filled in.
type App struct {
	// Screens holds one model per screen, indexed by Screen. The screens that
	// are not active keep their state, so leaving a screen and coming back to it
	// restores what the user had.
	Screens [numScreens]ScreenModel

	// Active is the screen that receives routed messages and renders.
	Active Screen

	// Width and Height are the last known terminal dimensions, in cells.
	Width  int
	Height int

	// Err is the error shown in the banner, nil when the banner is clear.
	Err error

	// Status is the transient status line text, empty when there is none.
	Status string

	// ShowHelp reports whether the full help overlay is open.
	ShowHelp bool

	// Keys is the global key map. Screens carry their own bindings.
	Keys KeyMap

	// Theme is the style set the root and screens render with. ThemePrimary and
	// ThemeSecondary retain the palette values used to build it.
	Theme          Theme
	ThemePrimary   string
	ThemeSecondary string
}

// New returns the root model with the four screen models installed and the
// hosts screen active. The screens are injected rather than constructed here so
// this package does not import the screen packages that import it, and they are
// passed as an array indexed by Screen so a caller cannot get the order wrong:
//
//	ui.New([4]ui.ScreenModel{
//		ui.ScreenHosts:   hostsScreen,
//		ui.ScreenBrowser: browserScreen,
//		ui.ScreenLibrary: libraryScreen,
//		ui.ScreenViewer:  viewerScreen,
//	})
//
// Every element must be non-nil.
func New(screens [numScreens]ScreenModel) App {
	return App{
		Screens: screens,
		Active:  ScreenHosts,
		Keys:    DefaultKeyMap(),
		Theme:   DefaultTheme(),
	}
}

// Init returns the command for the initially active screen.
func (a App) Init() tea.Cmd { return a.Screens[a.Active].Init() }

// Update handles the messages the root owns and routes every other message to
// the active screen, replacing the stored screen with the one it returns.
//
// The root owns four things, and none of them are forwarded to a screen:
//
//   - tea.WindowSizeMsg records the new dimensions and resizes the active
//     screen to the 75% content region. It emits no command.
//   - ScreenTransitionMsg and BackMsg change the active screen. The screens that
//     are not active are left exactly as they were. Neither emits a command.
//   - ErrorMsg sets the error banner and ClearErrorMsg clears it. Neither
//     changes the active screen and neither emits a command.
//   - A tea.KeyMsg matching Keys.Quit or Keys.Help is handled here. Every other
//     key goes to the active screen.
//
// Update never does I/O. Anything that touches the network or the disk is
// returned as a tea.Cmd.
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.Width = msg.Width
		a.Height = msg.Height
		a.Screens[a.Active] = a.Screens[a.Active].Resize(msg.Width, ContentHeight(msg.Height))
		return a, nil

	case ScreenTransitionMsg:
		a.Active = msg.To
		return a, nil

	case BackMsg:
		a.Active = backTarget(a.Active)
		return a, nil

	case ErrorMsg:
		a.Err = msg.Err
		return a, nil

	case ClearErrorMsg:
		a.Err = nil
		return a, nil

	case StatusMsg:
		a.Status = msg.Text
		return a, nil

	case ThemeChangedMsg:
		a.ThemePrimary = msg.Primary
		a.ThemeSecondary = msg.Secondary
		a.Theme = DefaultTheme().WithColors(msg.Primary, msg.Secondary)
		for i, screen := range a.Screens {
			updated, _ := screen.Update(msg)
			a.Screens[i] = updated
		}
		return a, nil

	case tea.KeyMsg:
		if a.Err != nil && msg.Type == tea.KeyEsc {
			a.Err = nil
			return a, nil
		}
		switch {
		case key.Matches(msg, a.Keys.Quit):
			return a, tea.Quit
		case key.Matches(msg, a.Keys.Help):
			a.ShowHelp = !a.ShowHelp
			return a, nil
		}
	}

	screen, cmd := a.Screens[a.Active].Update(msg)
	a.Screens[a.Active] = screen
	return a, cmd
}

// backTarget returns the screen the back edge out of active leads to. Screens
// with no back edge in the screen graph (hosts, library) return themselves,
// which is a no-op transition.
func backTarget(active Screen) Screen {
	switch active {
	case ScreenBrowser:
		return ScreenHosts
	case ScreenViewer:
		return ScreenLibrary
	default:
		return active
	}
}

// View renders the chrome around the active screen: the title line, the fixed
// screen area, and either a bounded error box or the reserved status area.
func (a App) View() string {
	height := a.Height
	if height <= 0 {
		height = 30
	}
	layout := calculateVerticalLayout(height)
	title := a.Theme.Title.Render("logpick  /  " + a.Active.String())
	top := lipgloss.NewStyle().Height(layout.top).AlignVertical(lipgloss.Bottom).Render(title)
	screen := lipgloss.NewStyle().Height(layout.content).Render(a.Screens[a.Active].View())

	footer := lipgloss.NewStyle().Height(layout.error).Render(a.Theme.Status.Render(a.Status))
	if a.Err != nil {
		width := a.Width
		if width <= 0 {
			width = 80
		}
		if layout.error >= 3 {
			contentWidth := max(8, width-4)
			contentHeight := layout.error - 2
			message := wrapError("Error: "+a.Err.Error()+"  (esc dismiss)", contentWidth, contentHeight)
			footer = lipgloss.NewStyle().
				Width(contentWidth).
				Height(contentHeight).
				Padding(0, 1).
				Border(lipgloss.RoundedBorder()).
				Render(a.Theme.Error.Render(message))
		} else {
			message := wrapError("Error: "+a.Err.Error(), max(1, width), max(1, layout.error))
			footer = lipgloss.NewStyle().Height(layout.error).Render(a.Theme.Error.Render(message))
		}
	}
	bottom := lipgloss.NewStyle().Height(layout.bottom).Render("")

	return lipgloss.JoinVertical(lipgloss.Left, top, screen, footer, bottom)
}

func wrapError(message string, width, maxLines int) string {
	words := strings.Fields(message)
	if len(words) == 0 || width <= 0 || maxLines <= 0 {
		return ""
	}
	lines := make([]string, 0, maxLines)
	var current strings.Builder
	for _, word := range words {
		if current.Len() > 0 && current.Len()+1+len(word) > width {
			lines = append(lines, current.String())
			current.Reset()
			if len(lines) == maxLines {
				break
			}
		}
		if current.Len() > 0 {
			current.WriteByte(' ')
		}
		wordRunes := []rune(word)
		if len(wordRunes) > width {
			word = string(wordRunes[:max(1, width-1)]) + "…"
		}
		current.WriteString(word)
	}
	if len(lines) < maxLines && current.Len() > 0 {
		lines = append(lines, current.String())
	}
	if len(lines) == maxLines && len(strings.Join(lines, " ")) < len(strings.Join(words, " ")) {
		last := []rune(lines[len(lines)-1])
		if len(last) >= width {
			last = last[:max(1, width-1)]
		}
		lines[len(lines)-1] = strings.TrimSpace(string(last)) + "…"
	}
	return strings.Join(lines, "\n")
}

// WaitForLines returns a command that takes one batch off ch and reports it as
// a LinesMsg, or reports StreamClosedMsg when ch is closed. A command returns
// exactly one message, so the handler of LinesMsg reissues this command to keep
// draining the stream.
func WaitForLines(ch <-chan []string) tea.Cmd {
	return func() tea.Msg {
		lines, ok := <-ch
		if !ok {
			return StreamClosedMsg{}
		}
		return LinesMsg{Lines: lines}
	}
}
