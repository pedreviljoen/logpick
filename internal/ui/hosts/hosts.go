package hosts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pedreviljoen/logpick/internal/config"
	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/picker"
)

// ListFunc loads the host history from state.toml, in whatever order the
// store returns it. Model does not read state.toml itself: New's caller
// injects this, keeping this package free of internal/state exactly as
// browser.PreviewFunc keeps the browser package free of internal/transport
// (DESIGN.md 9.2).
//
// Sorting is not ListFunc's job. DESIGN.md 9.1 requires the picker sorted
// pinned first, then by most recently seen, and internal/state.Store.List's
// own doc comment is explicit that presentation order "belongs to the
// screen that renders them, not to the store". The tea.Cmd Init returns
// calls ListFunc and sorts the result before wrapping it in a
// ui.HostsLoadedMsg, so by the time Update ever sees one, Hosts is already
// in display order (see ui.HostsLoadedMsg's own doc comment).
type ListFunc func() ([]ui.HostSummary, error)

// RemoveFunc deletes host from the history in state.toml. Bound to ctrl+d.
type RemoveFunc func(host string) error

// PinFunc sets host's pinned flag in state.toml. Bound to ctrl+p.
type PinFunc func(host string, pinned bool) error

// ProbeFunc performs the first-run liveness probe: a trivial remote command
// run over exec, the exec template the new profile would use, to prove the
// host is reachable before anything is written to config.toml (mechanic 2:
// probe before save). Model calls it only from inside the tea.Cmd Update
// returns for a first-run commit, never from Update itself (invariant 4).
//
// It is injected into New rather than Model holding a transport.Transport,
// for the same reason browser.PreviewFunc is injected: this package stays
// free of any dependency on internal/transport (DESIGN.md 9.2). Production
// wiring hands profile to transport.New and runs something inexpensive like
// "true"; a test substitutes a func that never touches a process.
//
// It takes the whole config.Profile rather than just the exec argv because
// the argv alone does not determine how to run a command: a profile with
// Persistent true must be probed through the persistent backend, which
// spawns the wrapper once and writes commands to the shell it lands on,
// while a non-persistent one substitutes {cmd} into the argv per call
// (DESIGN.md 7.3). Probing a persistent wrapper as if it were a plain ssh
// would append a command the wrapper cannot accept, which is exactly the
// failure this signature exists to prevent.
type ProbeFunc func(ctx context.Context, host string, profile config.Profile) error

// WriteFunc persists a newly probed host's profile and routing rule to
// config.toml. It is called only after ProbeFunc has already reported
// success for the same host, never before and never at all when the probe
// fails (mechanic 2), which is why it takes no context: by the time it
// runs, there is nothing left to cancel.
//
// Production code binds it to WriteConfigFile pointed at the real
// config.toml path. It is injected, rather than Model holding a path
// itself, so a test can point it at a temp-dir copy of a fixture, or
// substitute a func that fails the test if it is ever called, which is
// exactly how mechanic 2 proves nothing was written.
type WriteFunc func(host, profileName string, profile config.Profile) error

// ThemeSaveFunc persists the user-owned UI palette outside config.toml.
type ThemeSaveFunc func(primary, secondary string) error

// probeResultMsg returns a first-run probe result to the hosts model so it can
// clear the pending state before forwarding a failure to the root error banner.
type probeResultMsg struct {
	host    string
	profile string
	err     error
}

type themeSavedMsg struct {
	primary, secondary string
	err                error
}

// hostItem adapts a ui.HostSummary to picker.Item so the history list can
// be a picker.Model[hostItem]. FilterValue is the host's Label when it has
// one, since DESIGN.md 9.1 shows the label in place of the raw hostname,
// and the bare Name otherwise.
type hostItem struct {
	// Host is the wrapped history entry.
	Host ui.HostSummary
}

// FilterValue returns the item's label, or its hostname when it has none.
func (i hostItem) FilterValue() string {
	if i.Host.Label != "" {
		return i.Host.Label
	}
	return i.Host.Name
}

// Option configures a Model constructed by New.
type Option func(*Model)

// WithRemover overrides the RemoveFunc a Model uses for ctrl+d. New's
// default reports an error for every call, so a Model built without this
// option cannot silently no-op a delete.
func WithRemover(fn RemoveFunc) Option {
	return func(m *Model) {
		m.removeFn = fn
	}
}

// WithPinner overrides the PinFunc a Model uses for ctrl+p. New's default
// reports an error for every call, for the same reason as WithRemover.
func WithPinner(fn PinFunc) Option {
	return func(m *Model) {
		m.pinFn = fn
	}
}

// WithProber overrides the ProbeFunc a Model uses for the first-run probe.
// New's default reports an error for every call, for the same reason as
// WithRemover.
func WithProber(fn ProbeFunc) Option {
	return func(m *Model) {
		m.probeFn = fn
	}
}

// WithWriter overrides the WriteFunc a Model uses to save a newly probed
// profile. New's default reports an error for every call, for the same
// reason as WithRemover.
func WithWriter(fn WriteFunc) Option {
	return func(m *Model) {
		m.writeFn = fn
	}
}

// WithTheme applies the application's configured palette to the host picker
// and connection form.
func WithTheme(theme ui.Theme) Option {
	return func(m *Model) { m.theme = theme }
}

// WithPalette seeds the theme editor with the active hex values.
func WithPalette(primary, secondary string) Option {
	return func(m *Model) {
		m.palettePrimary = primary
		m.paletteSecondary = secondary
	}
}

// WithThemeSaver configures persistence for the interactive theme editor.
func WithThemeSaver(fn ThemeSaveFunc) Option {
	return func(m *Model) { m.themeSaveFn = fn }
}

// Model is the hosts screen: the fuzzy list of host history and the
// first-run flow for a host that history and config.toml both know nothing
// about (DESIGN.md 9.1). It implements ui.ScreenModel.
//
// # Committing an existing host
//
// enter on a row already in the picker's match set - a host that has
// state.toml history, and so already carries a known Profile on its
// ui.HostSummary - never probes or writes anything. Update reports
// ui.HostSelectedMsg{Host: summary.Name, Profile: summary.Profile}
// directly from the values already on the row (mechanic 1's simplest
// case).
//
// # The first-run flow
//
// With no host history, the connection form opens automatically; ctrl+n opens
// it later. The form captures host, optional identity file and command template.
// On enter, Update resolves the hostname against cfg the same way config.Resolve
// does: it checks cfg.Hosts for an exact [host.<name>] entry and cfg.Matches, in
// file order, for the first matching rule.
//
//   - If either exists, the host already has a routing rule and Update
//     takes the same no-probe path as an existing history row: it calls
//     config.Resolve(cfg, host, config.Overrides{}) for the resolved
//     profile name and reports ui.HostSelectedMsg immediately (mechanic 1
//     for a host with no history yet).
//   - If neither exists, this is a genuinely new host. Update builds an argv
//     template from the form, remembers the complete profile as pending, and
//     returns a tea.Cmd that calls ProbeFunc. A probe failure reopens the form,
//     reports ui.ErrorMsg and touches config.toml not at all. A probe success
//     advances to the config write.
//
// The private probe result message returns to Model while it is still active.
// Model checks it against the pending host, clears the pending state, and on
// success returns a tea.Cmd
// that calls WriteFunc with the pending host, the host name as profile name,
// and the form-built profile. The identity file path may be stored in Exec,
// but key contents are never read into or written to config. A write failure
// reports ui.ErrorMsg. A write success
// reports ui.HostSelectedMsg{Host, Profile: host} (mechanic 3), which is
// the point the root moves to the browser and starts the real connection.
//
// # Delete and pin
//
// ctrl+d calls RemoveFunc for the row under the cursor and, on success,
// reports ui.HostRemovedMsg; Update then drops that row from Hosts. ctrl+p
// calls PinFunc with the row's flag inverted and, on success, reports
// ui.HostPinnedMsg; Update then flips that row's Pinned flag and re-sorts
// pinned-first-then-by-last-seen, the same order Init's load applies
// (mechanic 5).
//
// # Value semantics
//
// Model follows the value-model style used throughout internal/ui: Init,
// Update, View and Resize never mutate the receiver.
//
// The zero value is not useful. Use New.
type Model struct {
	// theme styles focus, borders, guidance and warnings.
	theme ui.Theme

	// cfg is the parsed config.toml. Resolving a hostname against it is a
	// pure, in-memory operation (config.Resolve does no I/O), so Model
	// holds it directly rather than through an injected func.
	cfg *config.Config

	// listFn, removeFn, pinFn, probeFn, writeFn are the injected
	// collaborators. See their type doc comments.
	listFn      ListFunc
	removeFn    RemoveFunc
	pinFn       PinFunc
	probeFn     ProbeFunc
	writeFn     WriteFunc
	themeSaveFn ThemeSaveFunc

	// hosts is the current history, already in display order: pinned
	// first, then by most recently seen (DESIGN.md 9.1). It backs list and
	// is what Hosts reports.
	hosts []ui.HostSummary

	// list is the fuzzy picker over hosts, adapted through hostItem.
	list picker.Model[hostItem]

	// defining reports whether the new-connection form is open. It opens
	// automatically on first run and via ctrl+n when history exists.
	defining bool

	// The connection form captures enough information to create a usable
	// command profile instead of assuming unauthenticated ssh.
	input         textinput.Model
	identityInput textinput.Model
	commandInput  textinput.Model
	formFocus     int

	// formPersistent is the "persistent session" toggle, the fourth form
	// field. It records whether the command is a wrapper that takes a host
	// and no remote command (ec2-ssh and similar), which must be driven as
	// one long-lived shell rather than one process per command. See
	// firstRunProfile for why this cannot be inferred from the command.
	formPersistent bool

	// Theme editor state. Changes preview live through ui.ThemeChangedMsg and
	// are persisted only when Enter commits them.
	theming                bool
	themeFocus             int
	themePrimaryInput      textinput.Model
	themeSecondaryInput    textinput.Model
	palettePrimary         string
	paletteSecondary       string
	themeOriginalPrimary   string
	themeOriginalSecondary string

	// hasPending, pendingHost and pendingProfileName track a first-run
	// probe issued but not yet answered: the host name that was probed and
	// the profile name WriteFunc will be called with once ui.ConnectedMsg
	// arrives for it. hasPending is false at every other time, including
	// before the first probe and after its write has been reported either
	// way.
	hasPending         bool
	pendingHost        string
	pendingProfileName string
	pendingProfile     config.Profile

	// width and height are the last dimensions Resize was called with.
	width  int
	height int
}

// New returns a Model with no history loaded and nothing highlighted or
// defining, using cfg to resolve hostnames and list to load history on
// Init. Every injected collaborator besides list defaults to a func that
// returns an error identifying itself, so a Model built without the
// matching With* option fails loudly instead of silently no-oping the
// action it backs.
func New(cfg *config.Config, list ListFunc, opts ...Option) Model {
	input, identityInput, commandInput := newConnectionInputs()

	m := Model{
		theme:  ui.DefaultTheme(),
		cfg:    cfg,
		listFn: list,
		removeFn: func(host string) error {
			return fmt.Errorf("hosts: no RemoveFunc configured (use WithRemover); cannot remove host %q", host)
		},
		pinFn: func(host string, pinned bool) error {
			return fmt.Errorf("hosts: no PinFunc configured (use WithPinner); cannot set pinned=%v for host %q", pinned, host)
		},
		probeFn: func(ctx context.Context, host string, profile config.Profile) error {
			return fmt.Errorf("hosts: no ProbeFunc configured (use WithProber); cannot probe host %q", host)
		},
		writeFn: func(host, profileName string, profile config.Profile) error {
			return fmt.Errorf("hosts: no WriteFunc configured (use WithWriter); cannot write profile %q for host %q", profileName, host)
		},
		themeSaveFn: func(primary, secondary string) error {
			return fmt.Errorf("hosts: no ThemeSaveFunc configured; cannot save palette %q/%q", primary, secondary)
		},
		list:          picker.New[hostItem](nil),
		input:         input,
		identityInput: identityInput,
		commandInput:  commandInput,
	}

	for _, opt := range opts {
		opt(&m)
	}
	m.themeOriginalPrimary = m.palettePrimary
	m.themeOriginalSecondary = m.paletteSecondary

	return m
}

// Init returns the tea.Cmd that loads the host history: it calls listFn,
// sorts the result pinned first then by most recently seen (DESIGN.md
// 9.1), and reports it as a ui.HostsLoadedMsg, or a ui.ErrorMsg if listFn
// fails.
func (m Model) Init() tea.Cmd {
	listFn := m.listFn
	return func() tea.Msg {
		loaded, err := listFn()
		if err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("loading host history: %w", err)}
		}
		return ui.HostsLoadedMsg{Hosts: sortHosts(loaded)}
	}
}

// sortHosts returns a copy of hosts sorted pinned first, then by most
// recently seen (DESIGN.md 9.1), stably so hosts with equal keys keep
// their relative order. It never mutates hosts.
func sortHosts(hosts []ui.HostSummary) []ui.HostSummary {
	sorted := append([]ui.HostSummary(nil), hosts...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Pinned != sorted[j].Pinned {
			return sorted[i].Pinned
		}
		return sorted[i].LastSeen.After(sorted[j].LastSeen)
	})
	return sorted
}

// buildList returns a picker over hosts, adapted through hostItem, in the
// same order as hosts, so a fuzzy.Match's Index (for both an empty and a
// non-empty query) can be used directly as an index into hosts.
func buildList(hosts []ui.HostSummary) picker.Model[hostItem] {
	items := make([]hostItem, len(hosts))
	for i, h := range hosts {
		items[i] = hostItem{Host: h}
	}
	return picker.New(items)
}

// Update handles one message and returns the updated Model and, when the
// message requires further work, the tea.Cmd that performs it. It never
// mutates the receiver and it never blocks on I/O.
//
// Messages handled:
//
//   - ui.HostsLoadedMsg replaces Hosts with the (already sorted) payload
//     and rebuilds the picker over it.
//   - ui.HostRemovedMsg drops the named row from Hosts and rebuilds the
//     picker.
//   - ui.HostPinnedMsg updates the named row's Pinned flag, re-sorts, and
//     rebuilds the picker (mechanic 5).
//   - ui.ConnectedMsg drives the second half of the first-run flow. See
//     Model's "The first-run flow" doc section.
//   - tea.KeyMsg is handled as documented on Model: ctrl+d, ctrl+p and
//     ctrl+n are intercepted here rather than forwarded, since the
//     embedded picker already treats ctrl+n and ctrl+p as list movement
//     and this screen needs those two chords for delete-adjacent actions
//     instead (DESIGN.md 9.1's bindings take precedence over picker's on
//     this screen). Every other key goes to the picker when not defining,
//     or to input when defining.
//
// Every other message is passed through unchanged and does not alter the
// model.
func (m Model) Update(msg tea.Msg) (ui.ScreenModel, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.HostsLoadedMsg:
		hosts := append([]ui.HostSummary(nil), msg.Hosts...)
		next := m
		next.hosts = hosts
		next.list = buildList(hosts)
		if len(hosts) == 0 {
			next = next.openConnectionForm()
		}
		return next, nil

	case ui.HostRemovedMsg:
		hosts := make([]ui.HostSummary, 0, len(m.hosts))
		for _, h := range m.hosts {
			if h.Name != msg.Host {
				hosts = append(hosts, h)
			}
		}
		next := m
		next.hosts = hosts
		next.list = buildList(hosts)
		return next, nil

	case ui.HostPinnedMsg:
		hosts := make([]ui.HostSummary, len(m.hosts))
		copy(hosts, m.hosts)
		for i := range hosts {
			if hosts[i].Name == msg.Host {
				hosts[i].Pinned = msg.Pinned
			}
		}
		hosts = sortHosts(hosts)
		next := m
		next.hosts = hosts
		next.list = buildList(hosts)
		return next, nil

	case ui.ThemeChangedMsg:
		next := m
		next.theme = ui.DefaultTheme().WithColors(msg.Primary, msg.Secondary)
		next.palettePrimary = msg.Primary
		next.paletteSecondary = msg.Secondary
		return next, nil

	case themeSavedMsg:
		next := m
		if msg.err != nil {
			return next, func() tea.Msg { return ui.ErrorMsg{Err: fmt.Errorf("saving theme: %w", msg.err)} }
		}
		next.theming = false
		next.palettePrimary = msg.primary
		next.paletteSecondary = msg.secondary
		next.themeOriginalPrimary = msg.primary
		next.themeOriginalSecondary = msg.secondary
		return next, func() tea.Msg { return ui.StatusMsg{Text: "theme saved"} }

	case probeResultMsg:
		return m.handleProbeResult(msg)

	case ui.ConnectedMsg:
		return m.handleConnected(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)

	default:
		return m, nil
	}
}

func (m Model) handleProbeResult(msg probeResultMsg) (ui.ScreenModel, tea.Cmd) {
	if !m.hasPending || msg.host != m.pendingHost {
		return m, nil
	}
	if msg.err == nil {
		return m.handleConnected(ui.ConnectedMsg{Host: msg.host, Profile: msg.profile})
	}

	next := m
	next.hasPending = false
	next.defining = true
	next = next.focusFormField(m.formFocus)
	cmd := func() tea.Msg {
		return ui.ErrorMsg{Err: fmt.Errorf("probing host %q: %w", msg.host, msg.err)}
	}
	return next, cmd
}

// handleConnected drives the second half of the first-run flow: it ignores
// a ui.ConnectedMsg that does not belong to the probe this Model issued,
// and otherwise clears the pending state and returns the tea.Cmd that
// calls WriteFunc for it (see Model's "The first-run flow" doc section).
func (m Model) handleConnected(msg ui.ConnectedMsg) (ui.ScreenModel, tea.Cmd) {
	if !m.hasPending || msg.Host != m.pendingHost {
		return m, nil
	}

	next := m
	next.hasPending = false

	host := m.pendingHost
	profileName := m.pendingProfileName
	writeFn := m.writeFn
	profile := m.pendingProfile

	next.pendingHost = ""
	next.pendingProfileName = ""
	next.pendingProfile = config.Profile{}
	next.defining = false

	cmd := func() tea.Msg {
		if err := writeFn(host, profileName, profile); err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("writing profile for host %q: %w", host, err)}
		}
		return ui.HostSelectedMsg{Host: host, Profile: profileName}
	}
	return next, cmd
}

// handleKey handles one tea.KeyMsg, intercepting ctrl+d, ctrl+p, ctrl+n and
// enter as documented on Model, and forwarding every other key to input
// while defining is true, or to the picker otherwise.
func (m Model) handleKey(msg tea.KeyMsg) (ui.ScreenModel, tea.Cmd) {
	if m.theming {
		return m.handleThemeKey(msg)
	}
	if m.hasPending {
		return m, nil
	}

	if m.defining {
		switch msg.Type {
		case tea.KeyEsc:
			next := m
			next.defining = false
			return next, nil
		case tea.KeyTab, tea.KeyDown:
			next := m.focusFormField((m.formFocus + 1) % formFieldCount)
			return next, nil
		case tea.KeyShiftTab, tea.KeyUp:
			next := m.focusFormField((m.formFocus + formFieldCount - 1) % formFieldCount)
			return next, nil
		case tea.KeyEnter:
			return m.handleEnterDefining()
		}
		// The persistent toggle is a checkbox, not a text field: space
		// flips it. A space press arrives as KeyRunes whose String() is " "
		// (bubbletea does not use KeySpace for typed input), which is the
		// same check browser.go uses; matching it here keeps the space from
		// falling through into a text field. Every other key, and a space
		// on any other field, goes to the focused input.
		if m.formFocus == formFieldPersistent && msg.String() == " " {
			next := m
			next.formPersistent = !next.formPersistent
			return next, nil
		}
		return m.updateFormInput(msg)
	}

	switch msg.Type {
	case tea.KeyCtrlT:
		next := m.openThemeEditor()
		return next, nil
	case tea.KeyCtrlD:
		return m.handleDelete()
	case tea.KeyCtrlP:
		return m.handlePin()
	case tea.KeyCtrlN:
		return m.handleCtrlN()
	case tea.KeyEnter:
		return m.handleEnter()
	}

	next := m
	var cmd tea.Cmd
	next.list, cmd = next.list.Update(msg)
	return next, cmd
}

// The connection form's fields, in tab order. formFieldPersistent is a
// checkbox toggled with space rather than a textinput; the rest are text
// fields. formFieldCount bounds the tab wrap-around.
const (
	formFieldHost = iota
	formFieldIdentity
	formFieldCommand
	formFieldPersistent
	formFieldCount
)

func newConnectionInputs() (textinput.Model, textinput.Model, textinput.Model) {
	host := textinput.New()
	host.Prompt = ""
	host.Placeholder = "ec2-user@example.com"
	host.Focus()

	identity := textinput.New()
	identity.Prompt = ""
	identity.Placeholder = "optional — leave blank to use SSH agent/config"
	identity.Blur()

	command := textinput.New()
	command.Prompt = ""
	command.Placeholder = "ssh"
	command.Blur()
	return host, identity, command
}

func (m Model) openConnectionForm() Model {
	host, identity, command := newConnectionInputs()
	m.defining = true
	m.input = host
	m.identityInput = identity
	m.commandInput = command
	m.formFocus = formFieldHost
	m.formPersistent = false
	return m
}

func (m Model) focusFormField(field int) Model {
	m.input.Blur()
	m.identityInput.Blur()
	m.commandInput.Blur()
	m.formFocus = field
	switch field {
	case formFieldHost:
		m.input.Focus()
	case formFieldIdentity:
		m.identityInput.Focus()
	case formFieldCommand:
		m.commandInput.Focus()
	}
	// formFieldPersistent is a checkbox: nothing to focus, it renders its
	// own marker and responds to space in handleKey.
	return m
}

func (m Model) updateFormInput(msg tea.Msg) (ui.ScreenModel, tea.Cmd) {
	var cmd tea.Cmd
	switch m.formFocus {
	case formFieldHost:
		m.input, cmd = m.input.Update(msg)
	case formFieldIdentity:
		m.identityInput, cmd = m.identityInput.Update(msg)
	case formFieldCommand:
		m.commandInput, cmd = m.commandInput.Update(msg)
	}
	return m, cmd
}

func newThemeInput(value, placeholder string) textinput.Model {
	input := textinput.New()
	input.Prompt = ""
	input.Placeholder = placeholder
	input.SetValue(value)
	input.CharLimit = 7
	return input
}

func (m Model) openThemeEditor() Model {
	m.theming = true
	m.themeFocus = 0
	m.themeOriginalPrimary = m.palettePrimary
	m.themeOriginalSecondary = m.paletteSecondary
	m.themePrimaryInput = newThemeInput(m.palettePrimary, "#7aa2f7")
	m.themeSecondaryInput = newThemeInput(m.paletteSecondary, "#e0af68")
	m.themePrimaryInput.Focus()
	m.themeSecondaryInput.Blur()
	return m
}

func (m Model) focusThemeField(field int) Model {
	m.themePrimaryInput.Blur()
	m.themeSecondaryInput.Blur()
	m.themeFocus = field
	if field == 0 {
		m.themePrimaryInput.Focus()
	} else {
		m.themeSecondaryInput.Focus()
	}
	return m
}

func validHexColor(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != 7 || value[0] != '#' {
		return false
	}
	_, err := strconv.ParseUint(value[1:], 16, 24)
	return err == nil
}

func (m Model) handleThemeKey(msg tea.KeyMsg) (ui.ScreenModel, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEsc:
		next := m
		next.theming = false
		next.palettePrimary = m.themeOriginalPrimary
		next.paletteSecondary = m.themeOriginalSecondary
		return next, func() tea.Msg {
			return ui.ThemeChangedMsg{Primary: m.themeOriginalPrimary, Secondary: m.themeOriginalSecondary}
		}
	case tea.KeyTab, tea.KeyDown, tea.KeyShiftTab, tea.KeyUp:
		next := m.focusThemeField((m.themeFocus + 1) % 2)
		return next, nil
	case tea.KeyEnter:
		primary := m.themePrimaryInput.Value()
		secondary := m.themeSecondaryInput.Value()
		if !validHexColor(primary) || !validHexColor(secondary) {
			return m, func() tea.Msg {
				return ui.ErrorMsg{Err: errors.New("theme colors must be empty or use #RRGGBB hex values")}
			}
		}
		save := m.themeSaveFn
		return m, func() tea.Msg {
			return themeSavedMsg{primary: primary, secondary: secondary, err: save(primary, secondary)}
		}
	}

	next := m
	var inputCmd tea.Cmd
	if next.themeFocus == 0 {
		next.themePrimaryInput, inputCmd = next.themePrimaryInput.Update(msg)
	} else {
		next.themeSecondaryInput, inputCmd = next.themeSecondaryInput.Update(msg)
	}
	primary := next.themePrimaryInput.Value()
	secondary := next.themeSecondaryInput.Value()
	if !validHexColor(primary) || !validHexColor(secondary) {
		return next, inputCmd
	}
	next.palettePrimary = primary
	next.paletteSecondary = secondary
	next.theme = ui.DefaultTheme().WithColors(primary, secondary)
	return next, func() tea.Msg { return ui.ThemeChangedMsg{Primary: primary, Secondary: secondary} }
}

// highlighted returns the host currently under the picker's cursor, and
// true, or the zero value and false when the match set is empty.
func (m Model) highlighted() (ui.HostSummary, bool) {
	matches := m.list.Matches()
	if len(matches) == 0 {
		return ui.HostSummary{}, false
	}
	idx := matches[m.list.Cursor()].Index
	return m.hosts[idx], true
}

// handleDelete returns the tea.Cmd that removes the row under the cursor
// (ctrl+d), or nil when defining or when the match set is empty.
func (m Model) handleDelete() (ui.ScreenModel, tea.Cmd) {
	if m.defining {
		return m, nil
	}
	h, ok := m.highlighted()
	if !ok {
		return m, nil
	}

	host := h.Name
	removeFn := m.removeFn
	cmd := func() tea.Msg {
		if err := removeFn(host); err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("removing host %q: %w", host, err)}
		}
		return ui.HostRemovedMsg{Host: host}
	}
	return m, cmd
}

// handlePin returns the tea.Cmd that flips the pinned flag of the row
// under the cursor (ctrl+p), or nil when defining or when the match set is
// empty.
func (m Model) handlePin() (ui.ScreenModel, tea.Cmd) {
	if m.defining {
		return m, nil
	}
	h, ok := m.highlighted()
	if !ok {
		return m, nil
	}

	host := h.Name
	pinned := !h.Pinned
	pinFn := m.pinFn
	cmd := func() tea.Msg {
		if err := pinFn(host, pinned); err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("pinning host %q: %w", host, err)}
		}
		return ui.HostPinnedMsg{Host: host, Pinned: pinned}
	}
	return m, cmd
}

// handleCtrlN opens the new-connection form. It is a no-op while open.
func (m Model) handleCtrlN() (ui.ScreenModel, tea.Cmd) {
	if m.defining {
		return m, nil
	}
	next := m.openConnectionForm()
	return next, nil
}

// handleEnter commits the current action: on the picker, it reports the
// row under the cursor as ui.HostSelectedMsg (mechanic 1); while defining,
// it resolves the typed hostname and either commits it immediately or
// starts the first-run probe (see Model's "The first-run flow" doc
// section).
func (m Model) handleEnter() (ui.ScreenModel, tea.Cmd) {
	if m.defining {
		return m.handleEnterDefining()
	}

	h, ok := m.highlighted()
	if !ok {
		// The picker input doubles as the first-run hostname field when there
		// are no matching history rows. Enter should commit that visible value,
		// not silently do nothing.
		host := strings.TrimSpace(m.list.Query())
		if host == "" {
			return m, nil
		}
		return m.connectNewHost(host, config.DefaultProfile)
	}

	host := h.Name
	profileName := h.Profile
	cmd := func() tea.Msg {
		return ui.HostSelectedMsg{Host: host, Profile: profileName}
	}
	return m, cmd
}

// handleEnterDefining resolves the hostname typed into input against cfg,
// exactly as config.Resolve looks it up, and either commits immediately
// (mechanic 1, for a host with no history yet) or starts the first-run
// probe (mechanic 2), per Model's "The first-run flow" doc section.
func (m Model) handleEnterDefining() (ui.ScreenModel, tea.Cmd) {
	host := strings.TrimSpace(m.input.Value())
	if host == "" {
		return m, func() tea.Msg {
			return ui.ErrorMsg{Err: errors.New("host is required (use user@hostname when the remote user differs)")}
		}
	}

	profile, err := firstRunProfile(m.commandInput.Value(), m.identityInput.Value(), m.formPersistent)
	if err != nil {
		return m, func() tea.Msg { return ui.ErrorMsg{Err: err} }
	}
	return m.connectNewHost(host, profile)
}

// connectNewHost resolves a hostname that is not an existing picker row and
// either selects it immediately or starts the first-run liveness probe.
func (m Model) connectNewHost(host string, profile config.Profile) (ui.ScreenModel, tea.Cmd) {
	hasRule, err := hostHasRule(m.cfg, host)
	if err != nil {
		cmd := func() tea.Msg {
			return ui.ErrorMsg{Err: fmt.Errorf("resolving host %q: %w", host, err)}
		}
		return m, cmd
	}

	if hasRule {
		resolved, err := config.Resolve(m.cfg, host, config.Overrides{})
		if err != nil {
			cmd := func() tea.Msg {
				return ui.ErrorMsg{Err: fmt.Errorf("resolving host %q: %w", host, err)}
			}
			return m, cmd
		}
		profileName := resolved.ProfileName
		cmd := func() tea.Msg {
			return ui.HostSelectedMsg{Host: host, Profile: profileName}
		}
		return m, cmd
	}

	next := m
	next.hasPending = true
	next.pendingHost = host
	next.pendingProfileName = host
	next.pendingProfile = profile

	probeFn := m.probeFn
	probeProfile := profile
	probeProfile.Exec = append([]string(nil), profile.Exec...)
	cmd := func() tea.Msg {
		return probeResultMsg{
			host:    host,
			profile: host,
			err:     probeFn(context.Background(), host, probeProfile),
		}
	}
	return next, cmd
}

// firstRunProfile builds the profile the first-run form describes: the
// command template to reach the host, an optional identity file, and
// whether the command is a wrapper that must be driven as a persistent
// session rather than handed a command per call.
//
// # Why persistent is a form field and not an inference
//
// A wrapper divides into one of two kinds, and nothing about its name or
// argv reveals which:
//
//   - It forwards a trailing remote command, the way ssh does. Such a
//     template needs a {cmd} placeholder for the command to land in, and
//     runs one process per command.
//   - It takes a host and nothing else, dropping the caller into an
//     interactive shell. Amazon's ec2-ssh is this kind: its CLI is
//     `ec2-ssh [options] <host>`, so an argument appended after the host is
//     parsed as a second host address and rejected outright
//     (HostInfoUndefinedHosttypeException). The only way to run commands
//     through it is to spawn it once and write to the shell's stdin, which
//     is what a persistent profile does (DESIGN.md 7.3).
//
// This function used to assume every command was the first kind and append
// `-- {cmd}` unconditionally, which is precisely why the form could not
// produce a working profile for anything but ssh. persistent now selects
// between the two shapes, and only the non-persistent branch appends {cmd}.
//
// {host} is appended when absent in either case: without it the profile
// cannot name the host it is connecting to.
func firstRunProfile(command, identity string, persistent bool) (config.Profile, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		command = "ssh"
	}
	argv, err := splitCommandLine(command)
	if err != nil {
		return config.Profile{}, fmt.Errorf("invalid command: %w", err)
	}
	if len(argv) == 0 {
		return config.Profile{}, errors.New("command is required")
	}

	identity = strings.TrimSpace(identity)
	if identity != "" {
		identity, err = expandIdentityPath(identity)
		if err != nil {
			return config.Profile{}, err
		}
		insertAt := len(argv)
		for i, arg := range argv {
			if strings.Contains(arg, "{host}") {
				insertAt = i
				break
			}
		}
		withIdentity := make([]string, 0, len(argv)+2)
		withIdentity = append(withIdentity, argv[:insertAt]...)
		withIdentity = append(withIdentity, "-i", identity)
		withIdentity = append(withIdentity, argv[insertAt:]...)
		argv = withIdentity
	}

	if !containsTemplate(argv, "{host}") {
		argv = append(argv, "{host}")
	}
	if !persistent && !containsTemplate(argv, "{cmd}") {
		argv = append(argv, "--", "{cmd}")
	}

	profile := config.DefaultProfile
	profile.Exec = argv
	profile.Persistent = persistent
	return profile, nil
}

func containsTemplate(argv []string, placeholder string) bool {
	for _, arg := range argv {
		if strings.Contains(arg, placeholder) {
			return true
		}
	}
	return false
}

func expandIdentityPath(path string) (string, error) {
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expanding identity file %q: %w", path, err)
		}
		path = filepath.Join(home, strings.TrimPrefix(path, "~/"))
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving identity file %q: %w", path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("identity file %q: %w", absolute, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("identity file %q is not a regular file", absolute)
	}
	if permissions := info.Mode().Perm(); permissions&0o077 != 0 {
		return "", fmt.Errorf(
			"identity file %q has permissions %04o; SSH requires a private key mode such as 0600 (run: chmod 600 %s)",
			absolute,
			permissions,
			strconv.Quote(absolute),
		)
	}
	return absolute, nil
}

// splitCommandLine parses a command template into argv without invoking a
// shell. It supports whitespace, single/double quotes and backslash escapes.
func splitCommandLine(input string) ([]string, error) {
	var (
		argv    []string
		word    strings.Builder
		quote   rune
		escaped bool
		started bool
	)
	flush := func() {
		if started {
			argv = append(argv, word.String())
			word.Reset()
			started = false
		}
	}

	for _, r := range input {
		if escaped {
			word.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		if r == '\\' && quote != '\'' {
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			} else {
				word.WriteRune(r)
			}
			started = true
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
			started = true
		case ' ', '\t', '\n':
			flush()
		default:
			word.WriteRune(r)
			started = true
		}
	}
	if escaped {
		return nil, errors.New("trailing escape")
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote")
	}
	flush()
	return argv, nil
}

// hostHasRule reports whether cfg already routes host: an exact
// [host.<name>] entry, or the first [[match]] rule whose Host pattern
// matches, in file order, using the same path.Match semantics
// config.Resolve uses. A malformed pattern is reported as an error rather
// than silently skipped, the same as Resolve does.
func hostHasRule(cfg *config.Config, host string) (bool, error) {
	if cfg == nil {
		return false, nil
	}
	if _, ok := cfg.Hosts[host]; ok {
		return true, nil
	}
	for _, r := range cfg.Matches {
		matched, err := path.Match(r.Host, host)
		if err != nil {
			return false, fmt.Errorf("match pattern %q: %w", r.Host, err)
		}
		if matched {
			return true, nil
		}
	}
	return false, nil
}

// View renders either the connection form or the saved-host picker as a
// bordered panel with its controls visible.
func (m Model) View() string {
	if m.theming {
		return m.themeEditorView()
	}
	if m.defining {
		return m.connectionFormView()
	}

	body := m.list.View()
	if len(m.hosts) == 0 {
		body += m.theme.Dim.Render("No saved hosts. Press ctrl+n to configure a connection.") + "\n"
	} else {
		body += m.theme.Dim.Render("enter connect  •  ctrl+n new  •  ctrl+l library  •  ctrl+t theme  •  f1 keys") + "\n"
	}
	return hostPanelStyle(m.width, m.theme).Render(body)
}

func (m Model) themeEditorView() string {
	var b strings.Builder
	b.WriteString(m.theme.Title.Render("◆ Theme"))
	b.WriteString("\n")
	b.WriteString(m.theme.Dim.Render("Enter hex colors and watch this preview update live."))
	b.WriteString("\n\n")
	b.WriteString(renderFormField("Primary — titles, active borders, focus", m.themePrimaryInput, m.themeFocus == 0, m.theme))
	b.WriteString(renderFormField("Secondary — warnings, matches, highlights", m.themeSecondaryInput, m.themeFocus == 1, m.theme))

	previewWidth := m.width - 16
	if previewWidth > 56 {
		previewWidth = 56
	}
	if previewWidth < 24 {
		previewWidth = 24
	}
	preview := m.theme.PaneActive.
		Width(previewWidth).
		Padding(0, 1).
		Render(
			m.theme.Title.Render("Live preview") + "\n" +
				m.theme.Row.Render("normal log line") + "\n" +
				m.theme.Match.Render("highlighted match") + "\n" +
				m.theme.Error.Render("warning state"),
		)
	b.WriteString(preview)
	b.WriteString("\n\n")
	if !validHexColor(m.themePrimaryInput.Value()) || !validHexColor(m.themeSecondaryInput.Value()) {
		b.WriteString(m.theme.Error.Render("Use #RRGGBB values (or leave blank for defaults)."))
		b.WriteByte('\n')
	}
	b.WriteString(m.theme.Dim.Render("Tab move  •  Enter save  •  Esc cancel"))
	b.WriteByte('\n')
	return hostPanelStyle(m.width, m.theme).Render(b.String())
}

func (m Model) connectionFormView() string {
	var b strings.Builder
	b.WriteString(m.theme.Title.Render("◆ New connection"))
	b.WriteByte('\n')
	b.WriteString(m.theme.Dim.Render("Configure the command used to reach the remote host."))
	b.WriteByte('\n')
	b.WriteString(m.theme.Dim.Render("For EC2, include the AMI user (often ubuntu@host or ec2-user@host)."))
	b.WriteString("\n\n")
	b.WriteString(renderFormField("Host (user@hostname)", m.input, m.formFocus == formFieldHost, m.theme))
	b.WriteString(renderFormField("Identity file (-i) — optional; leave blank for SSH agent/config", m.identityInput, m.formFocus == formFieldIdentity, m.theme))
	b.WriteString(renderFormField("Command", m.commandInput, m.formFocus == formFieldCommand, m.theme))
	b.WriteString(renderFormCheckbox(
		"Persistent session — for wrappers that take no remote command (e.g. ec2-ssh)",
		m.formPersistent, m.formFocus == formFieldPersistent, m.theme,
	))
	if m.hasPending {
		b.WriteByte('\n')
		b.WriteString(m.theme.Match.Render("● Connecting to " + m.pendingHost + "…"))
		b.WriteByte('\n')
	} else {
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim.Render("Tab/Shift+Tab move  •  Space toggle  •  Enter connect  •  Esc cancel"))
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim.Render("Command defaults to ssh. Include {host} and {cmd} for custom templates."))
		b.WriteByte('\n')
		b.WriteString(m.theme.Dim.Render("Turn on Persistent for a wrapper like ec2-ssh that accepts only a hostname."))
		b.WriteByte('\n')
	}
	return hostPanelStyle(m.width, m.theme).Render(b.String())
}

func renderFormField(label string, input textinput.Model, active bool, theme ui.Theme) string {
	marker := "  "
	style := theme.PaneInactive
	labelStyle := theme.Row
	if active {
		marker = "› "
		style = theme.PaneActive
		labelStyle = theme.RowFocus
	}
	width := input.Width
	if width <= 0 {
		width = 60
	}
	field := style.
		Width(width).
		Padding(0, 1).
		Border(lipgloss.NormalBorder()).
		Render(input.View())
	return fmt.Sprintf("%s%s\n%s\n\n", marker, labelStyle.Render(label), field)
}

// renderFormCheckbox renders a boolean form field in the same visual idiom
// renderFormField uses for text fields - the same focus marker and label
// styling - so the persistent toggle reads as one more field in the form
// rather than a different kind of control bolted on.
func renderFormCheckbox(label string, checked, active bool, theme ui.Theme) string {
	marker := "  "
	labelStyle := theme.Row
	if active {
		marker = "› "
		labelStyle = theme.RowFocus
	}
	box := "[ ]"
	if checked {
		box = "[x]"
	}
	return fmt.Sprintf("%s%s %s\n\n", marker, box, labelStyle.Render(label))
}

func hostPanelStyle(width int, theme ui.Theme) lipgloss.Style {
	if width <= 0 {
		width = 78
	}
	width -= 4
	if width > 88 {
		width = 88
	}
	if width < 32 {
		width = 32
	}
	return theme.PaneActive.
		Width(width).
		Padding(1, 2).
		Border(lipgloss.RoundedBorder())
}

// Resize records the area the screen has to render into and returns the
// updated Model.
func (m Model) Resize(width, height int) ui.ScreenModel {
	next := m
	next.width = width
	next.height = height
	listHeight := height - 5
	if listHeight < 1 {
		listHeight = 1
	}
	next.list = next.list.SetHeight(listHeight).SetWidth(width - 8)
	inputWidth := width - 12
	if inputWidth > 76 {
		inputWidth = 76
	}
	if inputWidth < 12 {
		inputWidth = 12
	}
	next.input.Width = inputWidth
	next.identityInput.Width = inputWidth
	next.commandInput.Width = inputWidth
	next.themePrimaryInput.Width = inputWidth
	next.themeSecondaryInput.Width = inputWidth
	return next
}

// Hosts returns the current history in display order: pinned first, then
// by most recently seen (DESIGN.md 9.1). The returned slice is owned by
// Model; a caller must not mutate it.
func (m Model) Hosts() []ui.HostSummary {
	return m.hosts
}

// Theming reports whether the live theme editor is open.
func (m Model) Theming() bool {
	return m.theming
}

// Palette returns the active primary and secondary color values.
func (m Model) Palette() (string, string) {
	return m.palettePrimary, m.paletteSecondary
}

// ApplyFirstRun returns config.toml's bytes for existing with a new
// [profile.<profileName>] block appended, carrying profile's Exec, Scan and
// Persistent fields, never Copy, ConnectTimeout, AuthHint or SudoPrefix,
// and a new [[match]] block appended after it routing host - matched
// literally as the pattern, not glob-expanded beyond what a plain hostname
// already is under path.Match - to that profile.
//
// Persistent is written, as `persistent = true`, only when it is set;
// false is the TOML default and writing it adds noise. It is included at
// all - widening invariant 3's original "an exec template and a scan spec,
// nothing else" - because it is not an optional embellishment the way the
// other four fields are: it selects which transport backend runs the
// profile (transport.New). Dropping it would write a profile that probed
// successfully and is then unusable, since the exec template of a
// persistent wrapper has no {cmd} for a per-call command to land in. The
// four fields still excluded remain excluded: none of them changes whether
// the profile can run a command at all.
//
// It is a textual edit, never a call to toml.Marshal. AGENTS.md section 9
// names re-marshalling config.toml as the trap that destroys every
// hand-written comment, because go-toml/v2 has no comment-preserving round
// trip. ApplyFirstRun instead appends the new blocks after existing's last
// byte, adding a leading newline first if existing does not already end in
// one, so every byte already in existing - including any hand-written
// comment, anywhere in the file - is carried through unchanged (mechanic
// 4). A nil or empty existing is valid input and produces just the two new
// blocks.
//
// profileName is used as a TOML quoted key, in both the [profile.*] and
// [[match]] blocks, rather than a bare dotted path: a hostname such as
// "web-01.prod.internal" is a single profile name, not three nested
// tables, and only a quoted key round-trips through go-toml/v2 back into a
// single Config.Profiles entry keyed by profileName verbatim.
//
// profileName must not already name a profile in existing; ApplyFirstRun
// does not check for a collision, since New's first-run flow only ever
// calls it after confirming no rule already routes host (see Model's
// "first-run flow" doc section), and a caller that races that precondition
// has a bug ApplyFirstRun cannot detect from the bytes alone.
func ApplyFirstRun(existing []byte, host, profileName string, profile config.Profile) ([]byte, error) {
	var b bytes.Buffer
	b.Write(existing)

	if len(existing) > 0 {
		if !bytes.HasSuffix(existing, []byte("\n")) {
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}

	quotedProfile := strconv.Quote(profileName)

	fmt.Fprintf(&b, "[profile.%s]\n", quotedProfile)
	fmt.Fprintf(&b, "exec = %s\n", formatTOMLStringArray(profile.Exec))
	if profile.Persistent {
		fmt.Fprintf(&b, "persistent = true\n")
	}
	writeScanBlock(&b, quotedProfile, profile.Scan)

	fmt.Fprintf(&b, "\n[[match]]\n")
	fmt.Fprintf(&b, "host = %s\n", strconv.Quote(host))
	fmt.Fprintf(&b, "profile = %s\n", strconv.Quote(profileName))

	return b.Bytes(), nil
}

// writeScanBlock writes a [profile.<quotedProfile>.scan] block to b for
// every non-zero field of scan, or nothing at all when scan is the zero
// value.
func writeScanBlock(b *bytes.Buffer, quotedProfile string, scan config.ScanSpec) {
	if len(scan.Paths) == 0 && scan.MaxDepth == 0 && len(scan.Include) == 0 && len(scan.Exclude) == 0 {
		return
	}

	fmt.Fprintf(b, "\n[profile.%s.scan]\n", quotedProfile)
	if len(scan.Paths) > 0 {
		fmt.Fprintf(b, "paths = %s\n", formatTOMLStringArray(scan.Paths))
	}
	if scan.MaxDepth != 0 {
		fmt.Fprintf(b, "max_depth = %d\n", scan.MaxDepth)
	}
	if len(scan.Include) > 0 {
		fmt.Fprintf(b, "include = %s\n", formatTOMLStringArray(scan.Include))
	}
	if len(scan.Exclude) > 0 {
		fmt.Fprintf(b, "exclude = %s\n", formatTOMLStringArray(scan.Exclude))
	}
}

// formatTOMLStringArray renders items as a TOML inline array of quoted
// strings.
func formatTOMLStringArray(items []string) string {
	if len(items) == 0 {
		return "[]"
	}
	parts := make([]string, len(items))
	for i, s := range items {
		parts[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// WriteConfigFile returns the production WriteFunc bound to path: reading
// path's current bytes, running them through ApplyFirstRun, and replacing
// path atomically - a temp file created in the same directory, Sync, then
// Rename - exactly as internal/state.Store's own writes do (AGENTS.md
// section 3 invariant 5, DESIGN.md section 11). A path that does not yet
// exist is treated as an empty existing config, the same as ApplyFirstRun
// itself does.
//
// A caller usually binds this to WithWriter with the real XDG config.toml
// path. A test binds it to a path inside t.TempDir(), seeded from a
// testdata fixture, so mechanics 3 and 4 exercise the real read-edit-write
// cycle without ever touching the user's actual config.toml.
func WriteConfigFile(path string) WriteFunc {
	return func(host, profileName string, profile config.Profile) error {
		existing, err := readExistingConfig(path)
		if err != nil {
			return err
		}

		updated, err := ApplyFirstRun(existing, host, profileName, profile)
		if err != nil {
			return fmt.Errorf("applying first-run profile for host %q: %w", host, err)
		}

		if err := writeFileAtomic(path, updated, 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}

		return nil
	}
}

// readExistingConfig reads path's current bytes, treating a missing file
// as an empty config the same way ApplyFirstRun treats a nil existing.
func readExistingConfig(path string) ([]byte, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the caller-chosen config.toml location (XDG default) or a test temp dir, not untrusted input.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	return data, nil
}

// writeFileAtomic creates path's parent directory with user-only permissions,
// then writes data atomically: a temp file created in the same directory as
// path, written, fsynced, then renamed over path, exactly as internal/state's
// own writes do (AGENTS.md section 3 invariant
// 5). Creating the temp file in path's own directory rather than, say,
// os.TempDir matters: Rename is only atomic within one filesystem, and a
// temp file on a different mount could not be renamed atomically at all on
// many systems.
//
// A crash or a full disk during the write therefore leaves either the old
// contents at path or the fully-written new ones, never a half-written
// file. If any step fails, the temp file is removed rather than left
// behind.
func writeFileAtomic(path string, data []byte, perm os.FileMode) (err error) {
	dir := filepath.Dir(path)
	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return fmt.Errorf("creating config directory %s: %w", dir, mkdirErr)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}
	tmpPath := tmp.Name()
	closed := false

	defer func() {
		if !closed {
			_ = tmp.Close()
		}
		if err != nil {
			_ = os.Remove(tmpPath)
		}
	}()

	if chmodErr := os.Chmod(tmpPath, perm); chmodErr != nil { //nolint:gosec // tmpPath is the name CreateTemp just returned, not caller input.
		return fmt.Errorf("setting permissions on temp file: %w", chmodErr)
	}

	if _, writeErr := tmp.Write(data); writeErr != nil {
		return fmt.Errorf("writing temp file: %w", writeErr)
	}

	if syncErr := tmp.Sync(); syncErr != nil {
		return fmt.Errorf("syncing temp file: %w", syncErr)
	}

	if closeErr := tmp.Close(); closeErr != nil {
		closed = true
		return fmt.Errorf("closing temp file: %w", closeErr)
	}
	closed = true

	if renameErr := os.Rename(tmpPath, path); renameErr != nil { //nolint:gosec // tmpPath is the name CreateTemp just returned; path is the caller-chosen config.toml location (XDG default) or a test temp dir, not untrusted input.
		return fmt.Errorf("renaming temp file to %s: %w", path, renameErr)
	}

	return nil
}
