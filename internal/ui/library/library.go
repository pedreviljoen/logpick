// Package library is the screen listing locally fetched logs.
package library

import (
	"context"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/picker"
)

// fileItem adapts a ui.FetchedFile to picker.Item so the library's list can
// be a picker.Model[fileItem]. FilterValue is the fetched file's Local path
// rather than its Remote path, because Local is what DESIGN.md 10.1's
// layout guarantees unique across every entry the library ever lists — two
// hosts fetching the same remote path, or one host fetching it twice, land
// under distinct timestamp directories — while two entries could otherwise
// share a Remote. That uniqueness is what lets Highlighted and the delete
// path below identify one row by a single string with no ambiguity.
type fileItem struct {
	// File is the wrapped fetch record.
	File ui.FetchedFile
}

// FilterValue returns the fetched file's local path.
func (i fileItem) FilterValue() string {
	return i.File.Local
}

// DeleteFunc deletes a fetched file identified by its local path, from disk
// and from state. Model calls it only from inside the tea.Cmd Update
// returns for a delete key press (mechanic 1), never from Update itself,
// which is what keeps Update from blocking on I/O (AGENTS.md section 3,
// invariant 4).
//
// It is injected into New rather than Model holding a *local.Store and a
// *state.Store directly, which is what keeps this package free of a
// dependency on internal/local and internal/state, the same way
// browser.PreviewFunc keeps the browser package free of a dependency on
// internal/transport. A test supplies its own DeleteFunc rather than
// reaching into Model's unexported state (AGENTS.md section 5).
type DeleteFunc func(ctx context.Context, local string) error

// ContextFunc builds the context.Context a single delete request runs
// under. New's default is context.Background(), the shape production code
// always uses.
//
// WithContextFunc exists so a test can substitute a context it can observe
// or control, the same reason browser.ContextFunc exists, without reaching
// into Model's unexported state.
type ContextFunc func() context.Context

// Option configures a Model constructed by New.
type Option func(*Model)

// WithContextFunc overrides the ContextFunc a Model uses for every delete
// request it issues. New's default is a plain context.Background().
func WithContextFunc(fn ContextFunc) Option {
	return func(m *Model) {
		m.ctxFunc = fn
	}
}

// WithTheme applies the configured interactive palette.
func WithTheme(theme ui.Theme) Option {
	return func(m *Model) { m.theme = theme }
}

// Model is the library screen: a fuzzy list over locally fetched files, with
// delete (DESIGN.md 9.1). It implements ui.ScreenModel.
//
// # Loading
//
// Model does not load the fetch history itself: it only reacts to
// ui.LibraryLoadedMsg as it is routed to Update, the same scope decision
// browser.Model documents for starting a scan (see that type's "Scope
// note"). Reading state.toml requires a *state.Store, and injecting one
// here would tie this package to internal/state the same way an injected
// DeleteFunc keeps it free of internal/local and internal/state directly;
// unlike delete, nothing in this task's Contract names loading as this
// unit's responsibility, so Init returns nil and whatever composes the
// running application is responsible for reading state and sending
// ui.LibraryLoadedMsg once, on entry to this screen.
//
// # Delete
//
// ctrl+d deletes the entry currently highlighted by the embedded picker,
// the same target Highlighted reports: it cancels no other row and does not
// touch the query text. Update returns the tea.Cmd that calls the injected
// DeleteFunc under a context from ctxFunc; the row is not removed from
// Files until the resulting ui.LibraryDeletedMsg (on success) is routed
// back to Update, or the failure is reported as ui.ErrorMsg and the row is
// left in place (mechanic 1). ctrl+d on an empty match set is a no-op: nil
// Cmd, no state change.
//
// # Value semantics
//
// Model follows the value-model style used throughout internal/ui: Init,
// Update, View and Resize never mutate the receiver, so a caller such as
// ui.App can hold a Model as a plain field with no aliasing surprises.
//
// The zero value is not useful. Use New.
type Model struct {
	// theme styles the panel, title and controls.
	theme ui.Theme

	// deleter deletes one fetched file. See DeleteFunc.
	deleter DeleteFunc

	// ctxFunc builds the context each delete request runs under. See
	// ContextFunc.
	ctxFunc ContextFunc

	// width and height are the last dimensions Resize was called with.
	width  int
	height int

	// files is every fetched file this Model currently lists, in the order
	// the most recent ui.LibraryLoadedMsg supplied (state.toml's own order,
	// most recent fetch first). It backs list, and is what Files reports.
	files []ui.FetchedFile

	// list is the picker over files, adapted through fileItem.
	list picker.Model[fileItem]
}

// New returns a Model with no files listed, using deleter to perform every
// delete this Model issues, and context.Background() for every delete
// request unless overridden by WithContextFunc.
func New(deleter DeleteFunc, opts ...Option) Model {
	m := Model{
		theme:   ui.DefaultTheme(),
		deleter: deleter,
		ctxFunc: func() context.Context { return context.Background() },
		list:    picker.New[fileItem](nil),
	}
	for _, opt := range opts {
		opt(&m)
	}
	return m
}

// Init returns nil: Model has nothing to do until a ui.LibraryLoadedMsg or
// ui.LibraryDeletedMsg is routed to it. Loading the fetch history is the
// responsibility of whatever composes the application (see Model's
// "Loading" section).
func (m Model) Init() tea.Cmd {
	return nil
}

// buildList returns a picker over files, adapted through fileItem, in the
// same order as files, so a fuzzy.Match's Index (for both an empty and a
// non-empty query) can be used directly as an index into files.
func buildList(files []ui.FetchedFile) picker.Model[fileItem] {
	items := make([]fileItem, len(files))
	for i, f := range files {
		items[i] = fileItem{File: f}
	}
	return picker.New(items)
}

func (m Model) sizedList(files []ui.FetchedFile) picker.Model[fileItem] {
	list := buildList(files)
	if m.height > 0 {
		list = list.SetHeight(max(1, m.height-8)).SetWidth(max(1, m.width-8))
	}
	return list
}

// Update handles one message and returns the updated Model and, when the
// message requires further work, the tea.Cmd that performs it. It never
// mutates the receiver and it never blocks on I/O.
//
// Messages handled:
//
//   - ui.LibraryLoadedMsg replaces Files with the loaded fetch history and
//     rebuilds the picker over it.
//   - ui.LibraryDeletedMsg drops the entry whose Local matches from Files
//     and rebuilds the picker.
//   - A tea.KeyMsg matching enter reports the highlighted file as
//     ui.FileSelectedMsg. esc reports ui.BackMsg. ctrl+d issues the delete
//     command documented on Model's "Delete" section (mechanic 1). Every
//     other tea.KeyMsg is forwarded to the embedded picker.
//
// Every other message is passed through unchanged and does not alter the
// model.
func (m Model) Update(msg tea.Msg) (ui.ScreenModel, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.LibraryLoadedMsg:
		files := append([]ui.FetchedFile(nil), msg.Files...)
		next := m
		next.files = files
		next.list = next.sizedList(files)
		return next, nil

	case ui.LibraryDeletedMsg:
		files := make([]ui.FetchedFile, 0, len(m.files))
		for _, f := range m.files {
			if f.Local != msg.Local {
				files = append(files, f)
			}
		}
		next := m
		next.files = files
		next.list = next.sizedList(files)
		return next, nil

	case ui.ThemeChangedMsg:
		next := m
		next.theme = ui.DefaultTheme().WithColors(msg.Primary, msg.Secondary)
		return next, nil

	case tea.KeyMsg:
		if msg.Type == tea.KeyEsc {
			return m, func() tea.Msg { return ui.BackMsg{} }
		}
		if msg.Type == tea.KeyEnter {
			file, ok := m.Highlighted()
			if !ok {
				return m, nil
			}
			return m, func() tea.Msg { return ui.FileSelectedMsg{File: file} }
		}
		if msg.Type == tea.KeyCtrlD {
			return m, m.deleteHighlightedCmd()
		}
		next := m
		var cmd tea.Cmd
		next.list, cmd = next.list.Update(msg)
		return next, cmd

	default:
		return m, nil
	}
}

// deleteHighlightedCmd returns the tea.Cmd that deletes the currently
// highlighted entry (mechanic 1), or nil when the match set is empty.
func (m Model) deleteHighlightedCmd() tea.Cmd {
	file, ok := m.Highlighted()
	if !ok {
		return nil
	}

	deleter := m.deleter
	ctxFunc := m.ctxFunc
	local := file.Local

	return func() tea.Msg {
		ctx := ctxFunc()
		if err := deleter(ctx, local); err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("delete %s: %w", local, err)}
		}
		return ui.LibraryDeletedMsg{Local: local}
	}
}

// View renders the query line followed by the match rows, sized to the area
// Resize was last called with.
func (m Model) View() string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	height := m.height
	if height <= 0 {
		height = 24
	}
	innerWidth := max(1, width-4)
	innerHeight := max(1, height-2)
	textWidth := max(1, innerWidth-2)

	var b strings.Builder
	b.WriteString(m.theme.Title.Render(fitLine("Library — fetched logs", textWidth)))
	b.WriteByte('\n')
	if len(m.files) == 0 {
		b.WriteString(m.theme.Dim.Render(fitLine("No fetched logs yet. Space on a file in the browser saves one here.", textWidth)))
	} else {
		b.WriteString(m.theme.Dim.Render(fmt.Sprintf("%d files", len(m.files))))
	}
	b.WriteString("\n\n")
	b.WriteString(m.list.View())
	b.WriteString(m.theme.Dim.Render(fitLine("enter open  •  ctrl+d delete  •  type to filter  •  esc back", textWidth)))

	body := clipLines(b.String(), innerHeight)
	return m.theme.PaneActive.
		Width(innerWidth).
		Height(innerHeight).
		Padding(0, 1).
		Render(body)
}

// Resize records the area the screen has to render into and returns the
// updated Model, propagating the height to the embedded picker via
// picker.Model.SetHeight.
func (m Model) Resize(width, height int) ui.ScreenModel {
	next := m
	next.width = width
	next.height = height
	listHeight := max(1, height-8)
	next.list = next.list.SetHeight(listHeight).SetWidth(max(1, width-8))
	return next
}

func fitLine(value string, width int) string {
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

func clipLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) <= n {
		return s
	}
	return strings.Join(lines[:n], "\n")
}

// Files returns every fetched file this Model currently lists, in the order
// the most recent ui.LibraryLoadedMsg supplied, less any row a completed
// delete has since removed. The returned slice is owned by Model; a caller
// must not mutate it.
func (m Model) Files() []ui.FetchedFile {
	return m.files
}

// Highlighted returns the fetched file currently under the picker's cursor,
// and true. It returns the zero value and false when the match set is
// empty, which is also when ctrl+d is a no-op.
func (m Model) Highlighted() (ui.FetchedFile, bool) {
	matches := m.list.Matches()
	if len(matches) == 0 {
		return ui.FetchedFile{}, false
	}
	idx := matches[m.list.Cursor()].Index
	return m.files[idx], true
}
