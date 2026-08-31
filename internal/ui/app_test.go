package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Test plan for T12, the UI shell. One test per mechanic.
//
//  1. A tea.WindowSizeMsg updates the model's dimensions and propagates to the
//     active screen.
//  2. A screen transition message switches the active screen and preserves the
//     inactive ones.
//  3. An error message sets the error banner without changing screen.
//  4. SafeGo converts a panicking goroutine into an error on the error channel
//     rather than crashing the process.
//
// Mechanics 1 to 3 are in this file. Mechanic 4 is in safego_test.go.
//
// Every test feeds a tea.Msg to Update and asserts on the returned model and on
// the command it emitted. Nothing here asserts on rendered output: a golden
// frame tests lipgloss rather than this application.

// stubScreen is a recording ScreenModel. It makes propagation to a screen and
// preservation of a screen observable as behaviour, which the real screen
// packages cannot do yet because they do not exist.
//
// It is a pointer so a recording survives the App value being copied through
// Update, which is what lets a test see what an inactive screen was left with.
type stubScreen struct {
	id      string
	msgs    []tea.Msg
	width   int
	height  int
	resizes int
	inits   int
}

func (s *stubScreen) Init() tea.Cmd {
	s.inits++
	return nil
}

func (s *stubScreen) Update(msg tea.Msg) (ScreenModel, tea.Cmd) {
	s.msgs = append(s.msgs, msg)
	return s, nil
}

func (s *stubScreen) View() string { return s.id }

func (s *stubScreen) Resize(width, height int) ScreenModel {
	s.width = width
	s.height = height
	s.resizes++
	return s
}

// dims is a comparable view of a width and a height, so sizes are diffed as one
// value rather than field by field.
type dims struct {
	Width  int
	Height int
}

// newTestApp returns a root model holding four recording screens, with active
// selected, and the stubs so a test can observe them. New is not used: it is a
// Phase A stub.
func newTestApp(t *testing.T, active Screen) (App, [numScreens]*stubScreen) {
	t.Helper()

	var stubs [numScreens]*stubScreen
	var screens [numScreens]ScreenModel
	for i := range stubs {
		stubs[i] = &stubScreen{id: string(rune('a' + i))}
		screens[i] = stubs[i]
	}
	return App{Screens: screens, Active: active}, stubs
}

// update feeds msg to a and returns the root model it produced.
func update(t *testing.T, a App, msg tea.Msg) (App, tea.Cmd) {
	t.Helper()

	m, cmd := a.Update(msg)
	next, ok := m.(App)
	if !ok {
		t.Fatalf("Update returned %T, want ui.App", m)
	}
	return next, cmd
}

// Mechanic 1.
func TestAppWindowSize(t *testing.T) {
	tests := []struct {
		name   string
		active Screen
		msg    tea.WindowSizeMsg
	}{
		{
			name:   "a window size updates the dimensions and resizes the active screen",
			active: ScreenBrowser,
			msg:    tea.WindowSizeMsg{Width: 100, Height: 40},
		},
		{
			name:   "a window size reaches whichever screen is active",
			active: ScreenViewer,
			msg:    tea.WindowSizeMsg{Width: 80, Height: 24},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, stubs := newTestApp(t, tt.active)

			got, cmd := update(t, app, tt.msg)

			want := dims{Width: tt.msg.Width, Height: tt.msg.Height}
			if diff := cmp.Diff(want, dims{Width: got.Width, Height: got.Height}); diff != "" {
				t.Errorf("root dimensions mismatch (-want +got):\n%s", diff)
			}

			active := stubs[tt.active]
			wantScreen := dims{Width: tt.msg.Width, Height: ContentHeight(tt.msg.Height)}
			if diff := cmp.Diff(wantScreen, dims{Width: active.width, Height: active.height}); diff != "" {
				t.Errorf("active screen size mismatch (-want +got):\n%s", diff)
			}
			if active.resizes != 1 {
				t.Errorf("active screen resized %d times, want 1", active.resizes)
			}
			if cmd != nil {
				t.Error("resizing emitted a command, want none")
			}
		})
	}
}

// Mechanic 2.
func TestAppScreenTransition(t *testing.T) {
	first := LinesMsg{Lines: []string{"one"}}
	second := LinesMsg{Lines: []string{"two"}}

	tests := []struct {
		name       string
		active     Screen
		msgs       []tea.Msg
		wantActive Screen
		wantRouted map[Screen][]tea.Msg
	}{
		{
			name:       "a transition switches the active screen",
			active:     ScreenHosts,
			msgs:       []tea.Msg{ScreenTransitionMsg{To: ScreenBrowser}},
			wantActive: ScreenBrowser,
		},
		{
			name:   "a round trip preserves what each screen received",
			active: ScreenHosts,
			msgs: []tea.Msg{
				first,
				ScreenTransitionMsg{To: ScreenBrowser},
				second,
				ScreenTransitionMsg{To: ScreenHosts},
			},
			wantActive: ScreenHosts,
			wantRouted: map[Screen][]tea.Msg{
				ScreenHosts:   {first},
				ScreenBrowser: {second},
			},
		},
		{
			name:       "back leaves the browser for the hosts screen",
			active:     ScreenBrowser,
			msgs:       []tea.Msg{BackMsg{}},
			wantActive: ScreenHosts,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, stubs := newTestApp(t, tt.active)

			var cmd tea.Cmd
			for _, msg := range tt.msgs {
				app, cmd = update(t, app, msg)
			}

			if app.Active != tt.wantActive {
				t.Errorf("active screen = %d, want %d", app.Active, tt.wantActive)
			}
			if cmd != nil {
				t.Error("the transition emitted a command, want none")
			}

			for screen, stub := range stubs {
				if diff := cmp.Diff(tt.wantRouted[Screen(screen)], stub.msgs, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("screen %d received the wrong messages (-want +got):\n%s", screen, diff)
				}
			}
		})
	}
}

func TestThemeChangePropagatesToEveryScreen(t *testing.T) {
	app, stubs := newTestApp(t, ScreenHosts)
	msg := ThemeChangedMsg{Primary: "#112233", Secondary: "#ffaa00"}
	got, cmd := update(t, app, msg)
	if cmd != nil {
		t.Fatal("theme change emitted a command")
	}
	if got.ThemePrimary != msg.Primary || got.ThemeSecondary != msg.Secondary {
		t.Fatalf("root palette = %q/%q", got.ThemePrimary, got.ThemeSecondary)
	}
	for i, stub := range stubs {
		if len(stub.msgs) != 1 {
			t.Fatalf("screen %d received %d theme messages, want 1", i, len(stub.msgs))
		}
		if _, ok := stub.msgs[0].(ThemeChangedMsg); !ok {
			t.Fatalf("screen %d received %T, want ThemeChangedMsg", i, stub.msgs[0])
		}
	}
}

func TestVerticalLayoutUsesViewportPercentages(t *testing.T) {
	layout := calculateVerticalLayout(40)
	want := verticalLayout{top: 2, content: 30, error: 4, bottom: 4}
	if layout != want {
		t.Fatalf("vertical layout = %+v, want %+v", layout, want)
	}
	app, _ := newTestApp(t, ScreenBrowser)
	app.Width, app.Height = 100, 40
	if got := lipgloss.Height(app.View()); got != 40 {
		t.Fatalf("App.View height = %d, want exactly 40", got)
	}
}

func TestWrapErrorIsBounded(t *testing.T) {
	got := wrapError("permission denied "+strings.Repeat("very-long-diagnostic ", 20), 24, 2)
	lines := strings.Split(got, "\n")
	if len(lines) > 2 {
		t.Fatalf("wrapError produced %d lines, want at most 2:\n%s", len(lines), got)
	}
	for _, line := range lines {
		if len([]rune(line)) > 25 { // width plus a possible ellipsis
			t.Fatalf("wrapError line overruns its bound: %q", line)
		}
	}
}

// Mechanic 3.
func TestAppErrorBanner(t *testing.T) {
	errScan := errors.New("scanning jenkins-01: connection lost")

	tests := []struct {
		name         string
		startErr     error
		msg          tea.Msg
		wantErr      error
		wantContains string
	}{
		{
			name:         "an error message sets the banner without changing screen",
			msg:          ErrorMsg{Err: errScan},
			wantErr:      errScan,
			wantContains: "jenkins-01",
		},
		{
			name:     "clearing the banner leaves the screen alone",
			startErr: errScan,
			msg:      ClearErrorMsg{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, stubs := newTestApp(t, ScreenBrowser)
			app.Err = tt.startErr

			got, cmd := update(t, app, tt.msg)

			switch {
			case tt.wantErr == nil && got.Err != nil:
				t.Errorf("banner error = %v, want none", got.Err)
			case tt.wantErr != nil && !errors.Is(got.Err, tt.wantErr):
				t.Errorf("banner error = %v, want it to match %v", got.Err, tt.wantErr)
			case tt.wantContains != "" && !strings.Contains(got.Err.Error(), tt.wantContains):
				t.Errorf("banner error %q does not mention %q", got.Err, tt.wantContains)
			}

			if got.Active != ScreenBrowser {
				t.Errorf("active screen = %d, want %d", got.Active, ScreenBrowser)
			}
			if n := len(stubs[ScreenBrowser].msgs); n != 0 {
				t.Errorf("the active screen received %d messages, want none: the root owns the banner", n)
			}
			if cmd != nil {
				t.Error("the banner emitted a command, want none")
			}
		})
	}
}

func TestAppHelpOverlaySwallowsKeys(t *testing.T) {
	app, stubs := newTestApp(t, ScreenHosts)
	app.Keys = DefaultKeyMap()

	got, cmd := update(t, app, tea.KeyMsg{Type: tea.KeyF1})
	if !got.ShowHelp {
		t.Fatal("F1 did not open the help overlay")
	}
	if cmd != nil {
		t.Fatal("opening help emitted a command")
	}

	got, cmd = update(t, got, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	if !got.ShowHelp {
		t.Fatal("a key while help is open closed the overlay")
	}
	if cmd != nil {
		t.Fatal("a swallowed help key emitted a command")
	}
	if n := len(stubs[ScreenHosts].msgs); n != 0 {
		t.Fatalf("hosts received %d messages while help was open, want none", n)
	}

	got, _ = update(t, got, tea.KeyMsg{Type: tea.KeyEsc})
	if got.ShowHelp {
		t.Fatal("esc did not close the help overlay")
	}
}

func TestAppBackFromLibraryReturnsToLibraryFrom(t *testing.T) {
	app, _ := newTestApp(t, ScreenBrowser)
	app.LibraryFrom = ScreenBrowser

	got, cmd := update(t, app, ScreenTransitionMsg{To: ScreenLibrary})
	if got.Active != ScreenLibrary {
		t.Fatalf("active = %s, want library", got.Active)
	}
	if got.LibraryFrom != ScreenBrowser {
		t.Fatalf("LibraryFrom = %s, want browser", got.LibraryFrom)
	}
	if cmd != nil {
		t.Fatal("transition emitted a command")
	}

	got, _ = update(t, got, BackMsg{})
	if got.Active != ScreenBrowser {
		t.Fatalf("back from library active = %s, want browser", got.Active)
	}
}
