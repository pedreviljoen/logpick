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
// wiring builds exec's argv into a transport.Command and runs something
// inexpensive like "true"; a test substitutes a func that never touches a
// process.
type ProbeFunc func(ctx context.Context, host string, exec []string) error

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
// ctrl+n opens a text field for a hostname that is not in history. On
// enter, Update resolves that hostname against cfg the same way
// config.Resolve does: it checks cfg.Hosts for an exact [host.<name>]
// entry and cfg.Matches, in file order, for the first [[match]] rule whose
// Host pattern matches (config.Resolve's own path.Match semantics).
//
//   - If either exists, the host already has a routing rule and Update
//     takes the same no-probe path as an existing history row: it calls
//     config.Resolve(cfg, host, config.Overrides{}) for the resolved
//     profile name and reports ui.HostSelectedMsg immediately (mechanic 1
//     for a host with no history yet).
//   - If neither exists, this is a genuinely new host. Update remembers it
//     as pending and returns a tea.Cmd that calls ProbeFunc with
//     config.DefaultProfile.Exec, the only exec template T18's first-run
//     flow offers: there is no form for a custom one. A probe failure
//     reports ui.ErrorMsg and touches config.toml not at all (mechanic 2).
//     A probe success reports ui.ConnectedMsg{Host, Profile: host}, reusing
//     the message exactly as its own doc comment describes: "also the
//     success signal of the first-run probe".
//
// Because ui.ConnectedMsg is only ever routed to Model while Model is still
// the active screen, and the root only ever makes Model inactive in
// response to Model itself sending ui.HostSelectedMsg (which the first-run
// path has not sent yet at this point), every ui.ConnectedMsg Update
// receives belongs to a probe it issued. Update checks it against the
// pending host it recorded, clears the pending state, and returns a
// tea.Cmd that calls WriteFunc with the pending host, the pending host's
// name again as the profile name, and config.DefaultProfile - an exec
// template and a scan spec, nothing else (invariant 3: never write
// credentials). A write failure reports ui.ErrorMsg. A write success
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
	// cfg is the parsed config.toml. Resolving a hostname against it is a
	// pure, in-memory operation (config.Resolve does no I/O), so Model
	// holds it directly rather than through an injected func.
	cfg *config.Config

	// listFn, removeFn, pinFn, probeFn, writeFn are the injected
	// collaborators. See their type doc comments.
	listFn   ListFunc
	removeFn RemoveFunc
	pinFn    PinFunc
	probeFn  ProbeFunc
	writeFn  WriteFunc

	// hosts is the current history, already in display order: pinned
	// first, then by most recently seen (DESIGN.md 9.1). It backs list and
	// is what Hosts reports.
	hosts []ui.HostSummary

	// list is the fuzzy picker over hosts, adapted through hostItem.
	list picker.Model[hostItem]

	// defining reports whether the ctrl+n text field for a new hostname is
	// open. While true, tea.KeyMsg values that are not enter or esc go to
	// input rather than to list.
	defining bool

	// input is the text field for a new hostname, live only while defining
	// is true.
	input textinput.Model

	// hasPending, pendingHost and pendingProfileName track a first-run
	// probe issued but not yet answered: the host name that was probed and
	// the profile name WriteFunc will be called with once ui.ConnectedMsg
	// arrives for it. hasPending is false at every other time, including
	// before the first probe and after its write has been reported either
	// way.
	hasPending         bool
	pendingHost        string
	pendingProfileName string

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
	input := textinput.New()
	input.Placeholder = "hostname"

	m := Model{
		cfg:    cfg,
		listFn: list,
		removeFn: func(host string) error {
			return fmt.Errorf("hosts: no RemoveFunc configured (use WithRemover); cannot remove host %q", host)
		},
		pinFn: func(host string, pinned bool) error {
			return fmt.Errorf("hosts: no PinFunc configured (use WithPinner); cannot set pinned=%v for host %q", pinned, host)
		},
		probeFn: func(ctx context.Context, host string, exec []string) error {
			return fmt.Errorf("hosts: no ProbeFunc configured (use WithProber); cannot probe host %q", host)
		},
		writeFn: func(host, profileName string, profile config.Profile) error {
			return fmt.Errorf("hosts: no WriteFunc configured (use WithWriter); cannot write profile %q for host %q", profileName, host)
		},
		list:  picker.New[hostItem](nil),
		input: input,
	}

	for _, opt := range opts {
		opt(&m)
	}

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

	case ui.ConnectedMsg:
		return m.handleConnected(msg)

	case tea.KeyMsg:
		return m.handleKey(msg)

	default:
		return m, nil
	}
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
	profile := config.DefaultProfile

	next.pendingHost = ""
	next.pendingProfileName = ""

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
	switch msg.Type {
	case tea.KeyCtrlD:
		return m.handleDelete()
	case tea.KeyCtrlP:
		return m.handlePin()
	case tea.KeyCtrlN:
		return m.handleCtrlN()
	case tea.KeyEnter:
		return m.handleEnter()
	case tea.KeyEsc:
		if m.defining {
			next := m
			next.defining = false
			next.input = newHostInput()
			return next, nil
		}
	}

	if m.defining {
		next := m
		var cmd tea.Cmd
		next.input, cmd = next.input.Update(msg)
		return next, cmd
	}

	next := m
	var cmd tea.Cmd
	next.list, cmd = next.list.Update(msg)
	return next, cmd
}

// newHostInput returns a fresh, focused text field for a new hostname.
func newHostInput() textinput.Model {
	input := textinput.New()
	input.Placeholder = "hostname"
	input.Focus()
	return input
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

// handleCtrlN opens the new-hostname text field (ctrl+n). It is a no-op
// while already defining.
func (m Model) handleCtrlN() (ui.ScreenModel, tea.Cmd) {
	if m.defining {
		return m, nil
	}
	next := m
	next.defining = true
	next.input = newHostInput()
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
		return m, nil
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

	next := m
	next.defining = false
	next.input = newHostInput()

	if host == "" {
		return next, nil
	}

	hasRule, err := hostHasRule(m.cfg, host)
	if err != nil {
		cmd := func() tea.Msg {
			return ui.ErrorMsg{Err: fmt.Errorf("resolving host %q: %w", host, err)}
		}
		return next, cmd
	}

	if hasRule {
		resolved, err := config.Resolve(m.cfg, host, config.Overrides{})
		if err != nil {
			cmd := func() tea.Msg {
				return ui.ErrorMsg{Err: fmt.Errorf("resolving host %q: %w", host, err)}
			}
			return next, cmd
		}
		profileName := resolved.ProfileName
		cmd := func() tea.Msg {
			return ui.HostSelectedMsg{Host: host, Profile: profileName}
		}
		return next, cmd
	}

	next.hasPending = true
	next.pendingHost = host
	next.pendingProfileName = host

	probeFn := m.probeFn
	exec := config.DefaultProfile.Exec
	cmd := func() tea.Msg {
		if err := probeFn(context.Background(), host, exec); err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("probing host %q: %w", host, err)}
		}
		return ui.ConnectedMsg{Host: host, Profile: host}
	}
	return next, cmd
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

// View renders the query field, the sorted, filtered history list, and,
// while defining, the new-hostname field in place of the list.
func (m Model) View() string {
	if m.defining {
		return m.input.View() + "\n"
	}
	return m.list.View()
}

// Resize records the area the screen has to render into and returns the
// updated Model.
func (m Model) Resize(width, height int) ui.ScreenModel {
	next := m
	next.width = width
	next.height = height
	next.list = next.list.SetHeight(height)
	return next
}

// Hosts returns the current history in display order: pinned first, then
// by most recently seen (DESIGN.md 9.1). The returned slice is owned by
// Model; a caller must not mutate it.
func (m Model) Hosts() []ui.HostSummary {
	return m.hosts
}

// ApplyFirstRun returns config.toml's bytes for existing with a new
// [profile.<profileName>] block appended, carrying only profile's Exec and
// Scan fields, never Copy, Persistent, ConnectTimeout, AuthHint or
// SudoPrefix (invariant 3: a first-run profile is an exec template and a
// scan spec, nothing else), and a new [[match]] block appended after it
// routing host - matched literally as the pattern, not glob-expanded
// beyond what a plain hostname already is under path.Match - to that
// profile.
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

// writeFileAtomic writes data to path atomically: a temp file created in
// the same directory as path, written, fsynced, then renamed over path,
// exactly as internal/state's own writes do (AGENTS.md section 3 invariant
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
