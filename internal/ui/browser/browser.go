package browser

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/pedreviljoen/logpick/internal/ui"
	"github.com/pedreviljoen/logpick/internal/ui/picker"
)

// PreviewCacheSize is how many previews Model keeps cached at once
// (DESIGN.md 9.5).
//
// Eviction policy: least-recently-used. Every cache lookup that hits, and
// every reply that is written into the cache, moves that entry to the front
// of Model's internal ordering. Inserting an entry that would grow the
// cache past PreviewCacheSize evicts the current back of that ordering, the
// one least recently touched by either path.
const PreviewCacheSize = 20

// DefaultDebounce is the delay Update waits after a selection settles
// before issuing a preview request (DESIGN.md 9.5). New uses it unless
// overridden by WithDebounce.
const DefaultDebounce = 120 * time.Millisecond

// entryItem adapts a ui.ScanEntry to picker.Item so the browser's file list
// can be a picker.Model[entryItem]. FilterValue is the entry's full remote
// path, which is what the picker's fuzzy match runs against and, since
// picker renders the value it matched, also what the row displays.
type entryItem struct {
	// Entry is the wrapped scan result.
	Entry ui.ScanEntry
}

// FilterValue returns the entry's list value. See listValue.
func (i entryItem) FilterValue() string { return listValue(i.Entry) }

// listValue is the string the file list matches and renders for an entry:
// its remote path, with a trailing "/" when the entry is a directory.
//
// picker.Model has no separate display hook - it renders the same string it
// filtered on - so the trailing slash is the only place a directory can be
// marked as one. That makes the list value differ from ui.ScanEntry.Path
// for directories, and everything inside Model that identifies an entry by
// what the list has highlighted (indexOfPath, Highlighted,
// currentListPath) goes through this function rather than reading Path
// directly, so the two can never be compared against each other by
// accident. Anything that leaves this package for the remote - a preview
// request, a fetch, a directory to descend into - uses Entry.Path, which
// never carries the slash.
func listValue(e ui.ScanEntry) string {
	if e.IsDir {
		return e.Path + "/"
	}
	return e.Path
}

// PreviewFunc fetches the head of a remote file for the preview pane: the
// tail -n 100 that backs a PreviewMsg. Model calls it only from inside the
// tea.Cmd that Update returns for a settled, uncached selection, never from
// Update itself, which is what keeps Update from blocking on I/O
// (AGENTS.md section 3, invariant 4).
//
// It is injected into New rather than Model holding a transport.Transport,
// which is what keeps this package free of any dependency on
// internal/transport (DESIGN.md 9.2): a screen returns tea.Cmd values for
// the root to execute, it does not hold the means to perform I/O itself.
type PreviewFunc func(ctx context.Context, host, path string) ([]string, error)

// SelectFunc fetches a remote log to a local snapshot and returns its local
// path and full contents. Space invokes it; search always targets this local
// snapshot rather than repeatedly streaming the remote file.
type SelectFunc func(ctx context.Context, host string, entry ui.ScanEntry) (local string, lines []string, err error)

// SearchFunc searches a selected local snapshot.
type SearchFunc func(ctx context.Context, path, query string, regex bool) ([]ui.SearchMatch, error)

// ContextFunc builds the context a single in-flight preview request runs
// under, and the CancelFunc that stops it early when the selection moves
// on. New's default is a plain context.WithCancel(context.Background()),
// the shape production code always uses.
//
// WithContextFunc exists so a test can substitute a CancelFunc it can
// observe being called, for example one that closes a channel or sets a
// flag, without reaching into Model's unexported cancel field (AGENTS.md
// section 5: "assert on behaviour, never on unexported state that exists
// only for the test").
type ContextFunc func() (context.Context, context.CancelFunc)

// Option configures a Model constructed by New.
type Option func(*Model)

// WithContextFunc overrides the ContextFunc a Model uses for every preview
// request it issues. See ContextFunc's doc comment for why this exists.
func WithContextFunc(fn ContextFunc) Option {
	return func(m *Model) {
		m.ctxFunc = fn
	}
}

// WithDebounce overrides the delay Update waits after a selection settles
// before issuing a preview request. New's default is DefaultDebounce.
func WithDebounce(d time.Duration) Option {
	return func(m *Model) {
		m.debounce = d
	}
}

// WithSelector configures the local snapshot fetch used by Space.
func WithSelector(fn SelectFunc) Option {
	return func(m *Model) { m.selectLog = fn }
}

// WithSearch configures search over a selected local snapshot.
func WithSearch(fn SearchFunc) Option {
	return func(m *Model) { m.search = fn }
}

// WithTheme applies the configured interactive palette.
func WithTheme(theme ui.Theme) Option {
	return func(m *Model) { m.theme = theme }
}

type logSelectedMsg struct {
	host, remote, local string
	lines               []string
	err                 error
}

type browserSearchResultsMsg struct {
	local   string
	query   string
	gen     uint64
	matches []ui.SearchMatch
	err     error
}

// cacheEntry is one memoized preview, keyed by the host and path it was
// fetched for.
type cacheEntry struct {
	host  string
	path  string
	lines []string
}

// Model is the browser screen: the split pane file list and tail preview
// (DESIGN.md 9.1). It implements ui.ScreenModel.
//
// # Selection and preview flow
//
// Model tracks which entry is highlighted, the one under the embedded
// picker's cursor. Whenever handling a message changes which path is
// highlighted, including the transition from no selection to the first
// entry when the first batch of scan results arrives, Model:
//
//  1. cancels any preview request currently in flight, by calling the
//     context.CancelFunc held from that request's ContextFunc, if one is
//     set (mechanic 5);
//  2. increments the generation counter Gen, invalidating any debounce
//     tick or reply already in flight for the previous selection; and
//  3. returns a tea.Tick command that reports a ui.PreviewDebounceMsg
//     carrying the new generation and the newly highlighted (host, path)
//     after the configured debounce delay.
//
// When a ui.PreviewDebounceMsg arrives, Update compares its Gen against
// the model's current generation. A mismatch means the selection moved
// again before the tick fired, so the message is dropped: no request, no
// state change, nil command (mechanic 3). A match means the selection has
// settled, and Update checks the preview cache for that (host, path):
//
//   - a hit renders the cached lines immediately and issues no request, so
//     PreviewRequestCount does not advance (mechanic 4);
//   - a miss calls the model's ContextFunc to obtain a context and a
//     CancelFunc, records the CancelFunc, increments PreviewRequestCount,
//     and returns a tea.Cmd that calls PreviewFunc under that context and
//     reports the result as a ui.PreviewMsg (or a ui.ErrorMsg on failure),
//     both carrying the generation the request was issued for.
//
// A burst of selection changes therefore produces one debounce tick per
// change, but only the last one issued still matches the current
// generation by the time it fires, so exactly one request reaches
// PreviewFunc (mechanic 2), and the entries arriving progressively via
// ui.ScanEntriesMsg populate the file list as they come in rather than
// waiting for ui.ScanDoneMsg (mechanic 1).
//
// A later ui.PreviewMsg whose Gen no longer matches the current generation
// is dropped in the same way: it still writes into the cache, since the
// content it carries is valid for its (host, path) regardless of whether
// the user has since moved on, but it does not change what PreviewLines
// renders and it clears the in-flight CancelFunc if it was the one that
// reply belongs to.
//
// # Scope note
//
// Model does not own the channel a live scan streams from. It only reacts
// to ui.CachedScanMsg, ui.ScanEntriesMsg and ui.ScanDoneMsg as they are
// routed to Update; whatever composes the running application is
// responsible for draining that channel and feeding these messages in,
// the same way it is responsible for starting the scan in the first place.
// This keeps Model itself free of anything transport-shaped (DESIGN.md
// 9.2), and is a deliberate scope decision for this task: the Contract
// section names the preview cache, debounce and cancellation as what this
// unit owns, not scan orchestration.
//
// # Value semantics
//
// Model follows the value-model style used throughout internal/ui: Init,
// Update, View and Resize never mutate the receiver, so a caller such as
// ui.App can hold a Model as a plain field with no aliasing surprises.
//
// The zero value is not useful. Use New.
type Model struct {
	// theme styles pane focus, titles, controls and search matches.
	theme ui.Theme

	// host is the hostname every entry, preview request and cache entry in
	// this Model belongs to.
	host string

	// preview fetches a preview's lines. See PreviewFunc.
	preview PreviewFunc

	// selectLog fetches the committed log to a local snapshot; search operates
	// only on that snapshot.
	selectLog SelectFunc
	search    SearchFunc

	// ctxFunc builds the context/CancelFunc pair for each preview request.
	// See ContextFunc.
	ctxFunc ContextFunc

	// debounce is the delay between a selection settling and Update
	// issuing the tea.Tick that becomes a ui.PreviewDebounceMsg.
	debounce time.Duration

	// width and height are the last dimensions Resize was called with.
	width  int
	height int

	// entries is every scan result received so far, in arrival order. It
	// backs list, and is what Entries reports.
	entries []ui.ScanEntry

	// scanDone records whether the last ui.ScanDoneMsg routed to this
	// Model reported completion, for View's indicator.
	scanDone bool

	// list is the picker over entries, adapted through entryItem.
	list picker.Model[entryItem]

	// highlightedHost and highlightedPath identify the entry currently
	// under list's cursor, the target of the preview pane. hasHighlight is
	// false only before any entry has ever been highlighted, which is the
	// state before the first ui.ScanEntriesMsg or ui.CachedScanMsg arrives.
	highlightedHost string
	highlightedPath string
	hasHighlight    bool

	// gen is the generation counter (DESIGN.md 9.5), incremented every time
	// the highlighted entry changes.
	gen uint64

	// cancel is the CancelFunc for the preview request currently in
	// flight, obtained from ctxFunc when that request was issued. It is nil
	// whenever no request is in flight: before the first one, after a
	// reply has been received, or after it has already been cancelled by a
	// later selection change. AGENTS.md section 4.1 is explicit that this
	// field holds a CancelFunc and never the context itself.
	cancel context.CancelFunc

	// previewLines is what PreviewLines reports: the rendered content of
	// the highlighted entry's preview, from a cache hit or a completed
	// request whose Gen still matches gen at the time it was applied.
	previewLines []string

	// previewViewport owns independent scrolling for the fixed-size preview
	// pane. previewFocused determines whether navigation keys move it or the
	// file picker.
	previewViewport viewport.Model
	previewFocused  bool

	selecting     bool
	selectedPath  string
	selectedLocal string
	selectedLines []string
	selectCancel  context.CancelFunc

	pathSearching  bool
	pathInput      textinput.Model
	activeScanPath string

	searching           bool
	searchInput         textinput.Model
	searchQuery         string
	searchGen           uint64
	searchCancel        context.CancelFunc
	matches             []ui.SearchMatch
	matchIdx            int
	filteredLineNumbers []int

	// following reports whether the preview pane is a live tail. followPath
	// is the remote file being tailed; followLines is the ring of streamed
	// lines, capped at followCap (DESIGN.md 9.5).
	following   bool
	followPath  string
	followLines []string

	// requestCount is what PreviewRequestCount reports: how many times
	// Update has issued a preview request (a cache miss on a settled,
	// current-generation debounce tick).
	requestCount int

	// cache is the preview cache, ordered most-recently-used first, capped
	// at PreviewCacheSize entries. See PreviewCacheSize's doc comment for
	// the eviction policy.
	cache []cacheEntry
}

// New returns a Model for host with no entries and nothing highlighted,
// using preview to fetch previews and DefaultDebounce as the debounce
// delay unless overridden by an Option.
func New(host string, preview PreviewFunc, opts ...Option) Model {
	previewViewport := viewport.New(60, 20)
	previewViewport.SetContent("Select a log to load its preview.")
	searchInput := textinput.New()
	searchInput.Prompt = "/ "
	pathInput := textinput.New()
	pathInput.Prompt = "path › "
	pathInput.Placeholder = "/opt/app/logs or /srv/*/logs"
	m := Model{
		theme:   ui.DefaultTheme(),
		host:    host,
		preview: preview,
		selectLog: func(context.Context, string, ui.ScanEntry) (string, []string, error) {
			return "", nil, errors.New("browser: no SelectFunc configured")
		},
		search: func(context.Context, string, string, bool) ([]ui.SearchMatch, error) {
			return nil, errors.New("browser: no SearchFunc configured")
		},
		previewViewport: previewViewport,
		searchInput:     searchInput,
		pathInput:       pathInput,
		matchIdx:        -1,
		ctxFunc: func() (context.Context, context.CancelFunc) {
			return context.WithCancel(context.Background())
		},
		debounce: DefaultDebounce,
		list:     picker.New[entryItem](nil),
	}

	for _, opt := range opts {
		opt(&m)
	}

	return m
}

// Init returns nil: Model has nothing to do until a message routes an
// entry, a cached scan or a scan result to it. Starting the scan itself,
// and loading any cached listing, is the responsibility of whatever
// composes the application (see Model's "Scope note").
func (m Model) Init() tea.Cmd {
	return nil
}

// Update handles one message and returns the updated Model and, when the
// message requires further work, the tea.Cmd that performs it. It never
// mutates the receiver and it never blocks on I/O.
//
// Messages handled:
//
//   - ui.CachedScanMsg replaces entries with the cached listing.
//   - ui.ScanEntriesMsg appends its batch to entries, so the list populates
//     progressively as batches arrive (mechanic 1).
//   - ui.ScanDoneMsg records that the scan finished; it does not change
//     entries.
//   - tea.KeyMsg is forwarded to the embedded picker.
//   - ui.PreviewDebounceMsg and ui.PreviewMsg drive the preview flow
//     documented on Model itself (mechanics 2, 3, 4, 5).
//
// Any message that changes which entry is highlighted, regardless of which
// case above produced it, triggers the cancel/generation/debounce sequence
// documented on Model.
//
// Every other message is passed through unchanged and does not alter the
// model.
func (m Model) Update(msg tea.Msg) (ui.ScreenModel, tea.Cmd) {
	switch msg := msg.(type) {
	case ui.CachedScanMsg:
		if msg.Host != m.host {
			return m, nil
		}
		entries := append([]ui.ScanEntry(nil), msg.Entries...)
		next := m.withEntries(entries)
		// A cached listing is the configured scan for this host, which
		// leaves browse mode if a one-off listing had been showing.
		next.activeScanPath = ""
		result, cmd := next.applyHighlightChange()
		return result, cmd

	case ui.ScanEntriesMsg:
		if msg.Host != m.host {
			return m, nil
		}
		combined := make([]ui.ScanEntry, 0, len(m.entries)+len(msg.Entries))
		combined = append(combined, m.entries...)
		combined = append(combined, msg.Entries...)
		next := m.withEntries(combined)
		result, cmd := next.applyHighlightChange()
		return result, cmd

	case ui.PathScanStartedMsg:
		if msg.Host != m.host {
			return m, nil
		}
		next := m.withEntries(nil)
		next.scanDone = false
		next.activeScanPath = msg.Path
		next.hasHighlight = false
		next.highlightedPath = ""
		next.selectedPath = ""
		next.selectedLocal = ""
		next.selectedLines = nil
		next.matches = nil
		next.matchIdx = -1
		next.previewViewport.SetContent("Scanning remote path…")
		return next, nil

	case ui.ThemeChangedMsg:
		next := m
		next.theme = ui.DefaultTheme().WithColors(msg.Primary, msg.Secondary)
		if next.selectedLocal != "" {
			next.refreshSelectedContent()
		}
		return next, nil

	case ui.ScanStartedMsg:
		if msg.Host != m.host {
			return m, nil
		}
		next := m
		next.scanDone = false
		return next, nil

	case ui.ScanDoneMsg:
		if msg.Host != m.host {
			return m, nil
		}
		next := m
		next.scanDone = true
		return next, nil

	case ui.FollowStartedMsg:
		return m.handleFollowStarted(msg)

	case ui.LinesMsg:
		return m.handleFollowLines(msg)

	case ui.StreamClosedMsg:
		return m.handleFollowClosed()
	case logSelectedMsg:
		if msg.host != m.host {
			return m, nil
		}
		next := m
		next.selecting = false
		next.selectCancel = nil
		if msg.err != nil {
			next.previewViewport.SetContent("Selection failed. Press Space to retry.")
			return next, func() tea.Msg {
				return ui.ErrorMsg{Err: fmt.Errorf("selecting %s:%s: %w", msg.host, msg.remote, msg.err)}
			}
		}
		next.selectedPath = msg.remote
		next.selectedLocal = msg.local
		next.selectedLines = append([]string(nil), msg.lines...)
		next.previewLines = append([]string(nil), msg.lines...)
		next.previewViewport.SetContent(strings.Join(msg.lines, "\n"))
		next.previewViewport.GotoTop()
		next.previewFocused = true
		if next.searchCancel != nil {
			next.searchCancel()
		}
		next.searchCancel = nil
		next.searchGen++
		next.searching = false
		next.searchInput.Blur()
		next.searchInput.SetValue("")
		next.searchQuery = ""
		next.matches = nil
		next.matchIdx = -1
		next.filteredLineNumbers = nil
		return next, func() tea.Msg { return ui.ClearErrorMsg{} }

	case browserSearchResultsMsg:
		if msg.local != m.selectedLocal || msg.gen != m.searchGen {
			return m, nil
		}
		next := m
		next.searchCancel = nil
		if msg.err != nil {
			return next, func() tea.Msg {
				return ui.ErrorMsg{Err: fmt.Errorf("searching %s: %w", msg.local, msg.err)}
			}
		}
		next.searchQuery = msg.query
		next.matches = append([]ui.SearchMatch(nil), msg.matches...)
		next.matchIdx = -1
		if len(next.matches) > 0 {
			next.matchIdx = 0
		}
		next.refreshSelectedContent()
		next.previewViewport.GotoTop()
		return next, func() tea.Msg { return ui.ClearErrorMsg{} }

	case tea.KeyMsg:
		return m.handleKey(msg)

	case ui.PreviewDebounceMsg:
		return m.handleDebounce(msg)

	case ui.PreviewMsg:
		return m.handlePreviewMsg(msg)

	default:
		return m, nil
	}
}

func (m Model) handleKey(msg tea.KeyMsg) (ui.ScreenModel, tea.Cmd) {
	if m.pathSearching {
		switch msg.Type {
		case tea.KeyEsc:
			next := m
			next.pathSearching = false
			next.pathInput.Blur()
			return next, nil
		case tea.KeyEnter:
			path := strings.TrimSpace(m.pathInput.Value())
			if path == "" {
				return m, nil
			}
			next := m
			next.pathSearching = false
			next.pathInput.Blur()
			return next, pathScanCmd(m.host, path)
		default:
			next := m
			var cmd tea.Cmd
			next.pathInput, cmd = next.pathInput.Update(msg)
			return next, cmd
		}
	}
	if m.searching {
		switch msg.Type {
		case tea.KeyEsc:
			next := m
			if next.searchCancel != nil {
				next.searchCancel()
			}
			next.searchCancel = nil
			next.searchGen++
			next.searching = false
			next.searchInput.Blur()
			next.searchInput.SetValue("")
			next.searchQuery = ""
			next.matches = nil
			next.matchIdx = -1
			next.filteredLineNumbers = nil
			next.previewViewport.SetContent(strings.Join(next.selectedLines, "\n"))
			next.previewViewport.GotoTop()
			return next, nil
		case tea.KeyEnter:
			next := m
			next.searching = false
			next.searchInput.Blur()
			return next, nil
		default:
			next := m
			oldQuery := next.searchInput.Value()
			var inputCmd tea.Cmd
			next.searchInput, inputCmd = next.searchInput.Update(msg)
			if next.searchInput.Value() == oldQuery {
				return next, inputCmd
			}
			filtered, searchCmd := next.startLiveSearch()
			return filtered, searchCmd
		}
	}

	if msg.Type == tea.KeyEsc {
		if m.following {
			path := m.followPath
			return m, func() tea.Msg {
				return ui.FollowRequestedMsg{Host: m.host, Path: path}
			}
		}
		return m, func() tea.Msg { return ui.BackMsg{} }
	}
	if !m.pathSearching && !m.searching && msg.String() == "F" {
		return m.requestFollow()
	}
	if msg.Type == tea.KeyCtrlS {
		next := m
		next.pathSearching = true
		next.pathInput.SetValue(m.activeScanPath)
		next.pathInput.CursorEnd()
		return next, next.pathInput.Focus()
	}
	if msg.Type == tea.KeyTab || msg.Type == tea.KeyShiftTab {
		next := m
		next.previewFocused = !m.previewFocused
		return next, nil
	}
	if !m.previewFocused && (msg.String() == " " || msg.Type == tea.KeyEnter) {
		// Both keys commit the highlighted row, and what committing means
		// depends on what it is: a log is fetched to a local snapshot, a
		// directory is descended into, since a directory has nothing to
		// fetch or read (ui.ScanEntry.IsDir).
		if entry, ok := m.Highlighted(); ok && entry.IsDir {
			return m, pathScanCmd(m.host, entry.Path)
		}
		return m.startSelection()
	}
	if m.previewFocused {
		switch msg.String() {
		case "/":
			if m.selectedLocal == "" {
				return m, nil
			}
			next := m
			next.searching = true
			return next, next.searchInput.Focus()
		case "n":
			return m.stepSearch(1), nil
		case "N":
			return m.stepSearch(-1), nil
		}
		next := m
		var cmd tea.Cmd
		next.previewViewport, cmd = next.previewViewport.Update(msg)
		return next, cmd
	}

	next := m
	var listCmd tea.Cmd
	next.list, listCmd = next.list.Update(msg)
	result, hiCmd := next.applyHighlightChange()
	return result, tea.Batch(listCmd, hiCmd)
}

// pathScanCmd returns the command asking the composition layer for an
// unfiltered listing of path on host: every file and directory under it,
// with the profile's log patterns ignored (see ui.PathScanRequestedMsg).
// Ctrl+S with a typed path and enter on a directory both go through it, so
// descending is the same operation as typing the child path by hand.
func pathScanCmd(host, path string) tea.Cmd {
	return func() tea.Msg { return ui.PathScanRequestedMsg{Host: host, Path: path} }
}

// followCap is how many live-tail lines the preview keeps (DESIGN.md 9.5).
const followCap = 5000

func (m Model) requestFollow() (ui.ScreenModel, tea.Cmd) {
	if m.following {
		path := m.followPath
		if entry, ok := m.Highlighted(); ok && !entry.IsDir {
			path = entry.Path
		}
		if path == "" {
			return m, nil
		}
		return m, func() tea.Msg {
			return ui.FollowRequestedMsg{Host: m.host, Path: path}
		}
	}
	entry, ok := m.Highlighted()
	if !ok || entry.IsDir {
		return m, nil
	}
	return m, func() tea.Msg {
		return ui.FollowRequestedMsg{Host: m.host, Path: entry.Path}
	}
}

func (m Model) handleFollowStarted(msg ui.FollowStartedMsg) (ui.ScreenModel, tea.Cmd) {
	if msg.Host != m.host {
		return m, nil
	}
	next := m
	if next.followPath != msg.Path {
		next.followLines = nil
	}
	next.following = true
	next.followPath = msg.Path
	next.previewFocused = true
	if next.cancel != nil {
		next.cancel()
		next.cancel = nil
	}
	if len(next.followLines) == 0 {
		next.previewViewport.SetContent("Following " + msg.Path + "…")
	}
	next.previewViewport.GotoBottom()
	return next, nil
}

func (m Model) handleFollowLines(msg ui.LinesMsg) (ui.ScreenModel, tea.Cmd) {
	if len(msg.Lines) == 0 {
		return m, nil
	}
	next := m
	next.following = true
	next.followLines = appendFollow(next.followLines, msg.Lines, followCap)
	next.previewLines = next.followLines
	next.previewViewport.SetContent(strings.Join(next.followLines, "\n"))
	next.previewViewport.GotoBottom()
	next.previewFocused = true
	return next, nil
}

func (m Model) handleFollowClosed() (ui.ScreenModel, tea.Cmd) {
	if !m.following {
		return m, nil
	}
	next := m
	next.following = false
	if len(next.followLines) == 0 {
		next.previewViewport.SetContent("Follow ended.")
	}
	return next, nil
}

func appendFollow(held, batch []string, cap int) []string {
	if cap <= 0 {
		return nil
	}
	total := len(held) + len(batch)
	if total <= cap {
		out := make([]string, total)
		copy(out, held)
		copy(out[len(held):], batch)
		return out
	}
	out := make([]string, cap)
	skip := total - cap
	if skip >= len(held) {
		copy(out, batch[skip-len(held):])
		return out
	}
	copy(out, held[skip:])
	copy(out[len(held)-skip:], batch)
	return out
}

func (m Model) startSelection() (ui.ScreenModel, tea.Cmd) {
	entry, ok := m.Highlighted()
	if !ok || m.selecting {
		return m, nil
	}
	if m.selectedPath == entry.Path && m.selectedLocal != "" {
		next := m
		next.previewFocused = true
		return next, nil
	}

	ctx, cancel := m.ctxFunc()
	next := m
	if next.selectCancel != nil {
		next.selectCancel()
	}
	next.selectCancel = cancel
	next.selecting = true
	next.previewViewport.SetContent("Fetching local snapshot for search…")
	selector := m.selectLog
	host := m.host
	cmd := func() tea.Msg {
		defer cancel()
		localPath, lines, err := selector(ctx, host, entry)
		return logSelectedMsg{host: host, remote: entry.Path, local: localPath, lines: lines, err: err}
	}
	return next, cmd
}

func (m Model) startLiveSearch() (Model, tea.Cmd) {
	if m.searchCancel != nil {
		m.searchCancel()
	}
	m.searchCancel = nil
	m.searchGen++
	query := m.searchInput.Value()
	if query == "" || m.selectedLocal == "" {
		m.searchQuery = ""
		m.matches = nil
		m.matchIdx = -1
		m.filteredLineNumbers = nil
		m.previewViewport.SetContent(strings.Join(m.selectedLines, "\n"))
		m.previewViewport.GotoTop()
		return m, nil
	}

	search := m.search
	ctx, cancel := m.ctxFunc()
	m.searchCancel = cancel
	localPath := m.selectedLocal
	gen := m.searchGen
	cmd := func() tea.Msg {
		defer cancel()
		matches, err := search(ctx, localPath, query, false)
		return browserSearchResultsMsg{local: localPath, query: query, gen: gen, matches: matches, err: err}
	}
	return m, cmd
}

func (m Model) stepSearch(delta int) Model {
	if len(m.matches) == 0 {
		return m
	}
	m.matchIdx = (m.matchIdx + delta + len(m.matches)) % len(m.matches)
	m.refreshSelectedContent()
	line := m.matches[m.matchIdx].Line
	for index, sourceLine := range m.filteredLineNumbers {
		if sourceLine == line {
			m.previewViewport.SetYOffset(index)
			break
		}
	}
	return m
}

func (m *Model) refreshSelectedContent() {
	if m.searchQuery == "" {
		m.filteredLineNumbers = nil
		m.previewViewport.SetContent(strings.Join(m.selectedLines, "\n"))
		return
	}

	currentLine := -1
	if m.matchIdx >= 0 && m.matchIdx < len(m.matches) {
		currentLine = m.matches[m.matchIdx].Line
	}
	seen := make(map[int]bool, len(m.matches))
	filtered := make([]string, 0, len(m.matches))
	m.filteredLineNumbers = m.filteredLineNumbers[:0]
	for _, match := range m.matches {
		if seen[match.Line] || match.Line <= 0 || match.Line > len(m.selectedLines) {
			continue
		}
		seen[match.Line] = true
		line := fmt.Sprintf("%6d │ %s", match.Line, m.selectedLines[match.Line-1])
		if match.Line == currentLine {
			line = m.theme.LineMatch.Reverse(true).Render(line)
		}
		filtered = append(filtered, line)
		m.filteredLineNumbers = append(m.filteredLineNumbers, match.Line)
	}
	if len(filtered) == 0 {
		m.previewViewport.SetContent("No matching lines.")
		return
	}
	m.previewViewport.SetContent(strings.Join(filtered, "\n"))
}

// withEntries returns a copy of m with entries replaced by newEntries and
// list rebuilt over them. picker.Model offers no way to grow its item set
// other than constructing a fresh one over the whole slice (New always
// starts its cursor at 0), so when a highlighted path is still present in
// newEntries, the fresh list's cursor is restored onto it with a single
// SetCursor call, which keeps a streaming scan from yanking the user's
// highlight back to the top of the list every time a new batch arrives.
func (m Model) withEntries(newEntries []ui.ScanEntry) Model {
	m.entries = newEntries

	items := make([]entryItem, len(newEntries))
	for i, e := range newEntries {
		items[i] = entryItem{Entry: e}
	}
	layout := calculateLayout(m.width, m.height)
	newList := picker.New(items).
		SetHeight(max(1, layout.leftContentHeight-5)).
		SetWidth(max(1, layout.leftContentWidth-2))

	if m.hasHighlight {
		if idx := indexOfPath(newEntries, m.highlightedPath); idx >= 0 {
			newList = newList.SetCursor(idx)
		}
	}

	m.list = newList
	return m
}

// indexOfPath returns the index of the entry whose list value is value, or
// -1 if none matches. It takes a list value, not a ui.ScanEntry.Path; see
// listValue.
//
// The trailing slash is split off once, before the loop, rather than
// rebuilt per entry with listValue: this runs over every entry on the
// render path and a browse listing can hold ten thousand of them, so
// comparing against a value the loop does not have to allocate matters.
// strings.CutSuffix returns a slice of value, never a copy.
func indexOfPath(entries []ui.ScanEntry, value string) int {
	path, isDir := strings.CutSuffix(value, "/")
	for i, e := range entries {
		if e.Path == path && e.IsDir == isDir {
			return i
		}
	}
	return -1
}

// currentListPath returns the path of the item currently under list's
// cursor, and true, or "" and false if the list has no matches.
func (m Model) currentListPath() (string, bool) {
	matches := m.list.Matches()
	if len(matches) == 0 {
		return "", false
	}
	return matches[m.list.Cursor()].Str, true
}

// applyHighlightChange compares which path the list currently has
// highlighted against what m recorded last and, if it changed - including
// the transition from no highlight to the first entry - runs the
// cancel/generation/debounce sequence documented on Model. It returns m
// unchanged with a nil Cmd when the highlight did not move.
func (m Model) applyHighlightChange() (Model, tea.Cmd) {
	path, ok := m.currentListPath()

	if ok == m.hasHighlight && (!ok || path == m.highlightedPath) {
		return m, nil
	}

	next := m
	if next.cancel != nil {
		next.cancel()
	}
	next.cancel = nil
	next.gen++
	next.hasHighlight = ok
	next.highlightedPath = path
	if ok {
		next.highlightedHost = next.host
	} else {
		next.highlightedHost = ""
	}

	// Space commits a local snapshot. Once committed, moving around the left
	// list must not replace the searchable right-pane content with hover
	// previews; another Space explicitly replaces the selection. A live
	// follow owns the pane the same way until it is stopped.
	if next.selectedLocal != "" || next.following {
		return next, nil
	}

	next.previewLines = nil
	next.previewViewport.GotoTop()
	if !ok {
		next.previewViewport.SetContent("Select a log to load its preview.")
		return next, nil
	}

	// A directory is somewhere to look, not something to read: the tail
	// behind PreviewFunc fails on one on every host, so no request is ever
	// issued for it and no generation is burned waiting for the reply
	// (ui.ScanEntry.IsDir). Enter descends instead.
	if entry, found := next.Highlighted(); found && entry.IsDir {
		next.previewViewport.SetContent("Directory — press enter to list what is inside it.")
		return next, nil
	}

	next.previewViewport.SetContent("Loading preview…")

	gen := next.gen
	host := next.host
	debounce := next.debounce
	cmd := tea.Tick(debounce, func(time.Time) tea.Msg {
		return ui.PreviewDebounceMsg{Gen: gen, Host: host, Path: path}
	})
	return next, cmd
}

// handleDebounce handles a ui.PreviewDebounceMsg: dropping it if it is
// stale (mechanic 3), rendering a cache hit without a request (mechanic
// 4), or issuing the tea.Cmd that performs the preview request on a miss.
func (m Model) handleDebounce(msg ui.PreviewDebounceMsg) (ui.ScreenModel, tea.Cmd) {
	if msg.Gen != m.gen || msg.Host != m.host {
		return m, nil
	}

	// "Never tail a directory" is enforced here as well as in
	// applyHighlightChange, which does not issue a tick for one: this is
	// the single point every preview request passes through, so the
	// invariant holds even for a tick that arrived by some other route.
	if idx := indexOfPath(m.entries, msg.Path); idx >= 0 && m.entries[idx].IsDir {
		return m, nil
	}

	if entry, reordered, ok := cacheLookup(m.cache, msg.Host, msg.Path); ok {
		next := m
		next.cache = reordered
		next.previewLines = entry.lines
		next.previewViewport.SetContent(strings.Join(entry.lines, "\n"))
		next.previewViewport.GotoTop()
		return next, nil
	}

	ctx, cancel := m.ctxFunc()

	next := m
	next.cancel = cancel
	next.requestCount = m.requestCount + 1

	gen := m.gen
	host := msg.Host
	path := msg.Path
	preview := m.preview

	cmd := func() tea.Msg {
		lines, err := preview(ctx, host, path)
		if err != nil {
			return ui.ErrorMsg{Err: fmt.Errorf("preview %s:%s: %w", host, path, err)}
		}
		return ui.PreviewMsg{Gen: gen, Host: host, Path: path, Lines: lines}
	}

	return next, cmd
}

// handlePreviewMsg handles a ui.PreviewMsg reply: always writing it into
// the cache, but only applying it to previewLines, and only clearing the
// in-flight CancelFunc, when its Gen still matches the current generation.
func (m Model) handlePreviewMsg(msg ui.PreviewMsg) (ui.ScreenModel, tea.Cmd) {
	if msg.Host != m.host {
		return m, nil
	}

	lines := append([]string(nil), msg.Lines...)

	next := m
	next.cache = cacheStore(m.cache, cacheEntry{host: msg.Host, path: msg.Path, lines: lines})

	if msg.Gen == m.gen {
		next.previewLines = lines
		next.previewViewport.SetContent(strings.Join(lines, "\n"))
		next.previewViewport.GotoTop()
		next.cancel = nil
	}

	return next, nil
}

// cacheLookup looks up (host, path) in cache and, on a hit, returns the
// entry found and a new slice with that entry moved to the front (the
// LRU touch). It never mutates cache.
func cacheLookup(cache []cacheEntry, host, path string) (cacheEntry, []cacheEntry, bool) {
	for i, e := range cache {
		if e.host == host && e.path == path {
			reordered := make([]cacheEntry, 0, len(cache))
			reordered = append(reordered, e)
			reordered = append(reordered, cache[:i]...)
			reordered = append(reordered, cache[i+1:]...)
			return e, reordered, true
		}
	}
	return cacheEntry{}, cache, false
}

// cacheStore returns a new slice with entry at the front, replacing any
// existing entry for the same (host, path), evicting the back of the
// ordering if that would grow the cache past PreviewCacheSize. It never
// mutates cache.
func cacheStore(cache []cacheEntry, entry cacheEntry) []cacheEntry {
	updated := make([]cacheEntry, 0, len(cache)+1)
	updated = append(updated, entry)
	for _, e := range cache {
		if e.host == entry.host && e.path == entry.path {
			continue
		}
		updated = append(updated, e)
	}
	if len(updated) > PreviewCacheSize {
		updated = updated[:PreviewCacheSize]
	}
	return updated
}

// browseSummary is the count line View shows under the title once a
// one-off listing has finished: what was found, split by kind so an empty
// result reads as an answer rather than as a list that failed to load.
func (m Model) browseSummary() string {
	if len(m.entries) == 0 {
		return "Nothing readable here — ctrl+s to try another path"
	}
	dirs := 0
	for _, e := range m.entries {
		if e.IsDir {
			dirs++
		}
	}
	files := len(m.entries) - dirs
	return fmt.Sprintf("%d files, %d directories — type to filter", files, dirs)
}

// clipLines returns s with at most n lines, dropping any beyond that. A
// trailing newline is not a line: "a\n" is one line, so clipping it to one
// leaves it unchanged.
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

type browserLayout struct {
	stacked                 bool
	leftWidth, rightWidth   int
	leftHeight, rightHeight int
	leftContentWidth        int
	rightContentWidth       int
	leftContentHeight       int
	rightContentHeight      int
}

func calculateLayout(width, height int) browserLayout {
	if width <= 0 {
		width = 100
	}
	if height <= 0 {
		height = 30
	}
	layout := browserLayout{}
	if width < 72 {
		layout.stacked = true
		layout.leftWidth, layout.rightWidth = width, width
		layout.leftHeight = height / 2
		layout.rightHeight = height - layout.leftHeight
	} else {
		layout.leftWidth = width * 2 / 5
		layout.rightWidth = width - layout.leftWidth - 1
		layout.leftHeight, layout.rightHeight = height, height
	}
	// Content is the pane less its border and its one column of padding on
	// each side. The floor is 1, not a comfortable minimum: a pane whose
	// content is floored above what its share of the terminal can hold
	// renders taller or wider than the space it was given, and in stacked
	// mode two of those stack into a frame taller than the screen. A
	// terminal too small to be useful should render small and intact
	// rather than correctly proportioned and broken.
	layout.leftContentWidth = max(1, layout.leftWidth-4)
	layout.rightContentWidth = max(1, layout.rightWidth-4)
	layout.leftContentHeight = max(1, layout.leftHeight-2)
	layout.rightContentHeight = max(1, layout.rightHeight-2)
	return layout
}

// View renders a fixed-size split pane. Preview content is supplied by the
// viewport, so long lines and large files cannot grow either pane.
func (m Model) View() string {
	layout := calculateLayout(m.width, m.height)

	var left strings.Builder
	leftTextWidth := max(1, layout.leftContentWidth-2)
	rightTextWidth := max(1, layout.rightContentWidth-2)

	leftTitle := "Logs — " + m.host
	if m.activeScanPath != "" {
		leftTitle = "Browsing — " + m.activeScanPath
	}
	left.WriteString(m.theme.Title.Render(fitLine(leftTitle, leftTextWidth)))
	left.WriteByte('\n')
	switch {
	case m.scanDone && m.activeScanPath != "":
		left.WriteString(fitLine(m.browseSummary(), leftTextWidth))
		left.WriteString("\n\n")
	case m.scanDone:
		fmt.Fprintf(&left, "%d files\n\n", len(m.entries))
	case m.activeScanPath != "":
		left.WriteString(fitLine("Listing "+m.activeScanPath+"…", leftTextWidth))
		left.WriteString("\n\n")
	default:
		left.WriteString("Scanning…\n\n")
	}
	left.WriteString(m.list.View())
	leftControl := "space select • F follow • ctrl+s path • ctrl+l library • tab preview • esc hosts"
	if m.activeScanPath != "" {
		leftControl = "enter open • space select • F follow • ctrl+s path • esc hosts"
	}
	if m.pathSearching {
		leftControl = m.pathInput.View()
	}
	left.WriteString(m.theme.Dim.Render(fitLine(leftControl, leftTextWidth)))

	var right strings.Builder
	title := "Preview"
	switch entry, ok := m.Highlighted(); {
	case m.following:
		title = "Follow — " + m.followPath
	case m.selectedPath != "":
		title = "Selected — " + m.selectedPath
	case ok && entry.IsDir:
		title = "Directory — " + m.highlightedPath
	case m.hasHighlight:
		title += " — " + m.highlightedPath
	}
	titleStyle := m.theme.Title
	if m.following || m.selectedPath != "" {
		titleStyle = m.theme.Match
	}
	right.WriteString(titleStyle.Render(fitLine(title, rightTextWidth)))
	right.WriteString("\n\n")
	right.WriteString(m.previewViewport.View())
	right.WriteByte('\n')
	control := "tab files • ↑/↓/pgup/pgdn scroll • F follow • esc hosts"
	if m.following {
		control = "live follow • F or esc stop • tab files"
	} else if m.selectedLocal != "" {
		control = fmt.Sprintf("/ search • n/N match • %d/%d • tab files", m.matchIdx+1, len(m.matches))
	}
	if m.searching {
		control = fmt.Sprintf("%s • %d matching lines", m.searchInput.View(), len(m.filteredLineNumbers))
	}
	right.WriteString(m.theme.Dim.Render(fitLine(control, rightTextWidth)))

	pane := func(contentWidth, contentHeight int, active bool) lipgloss.Style {
		style := m.theme.PaneInactive
		if active {
			style = m.theme.PaneActive
		}
		return style.
			Width(contentWidth).
			Height(contentHeight).
			Padding(0, 1).
			Border(lipgloss.RoundedBorder())
	}
	// Both panes are clipped to the height they were laid out for before
	// they are rendered. lipgloss pads content shorter than Height but
	// does not trim content longer than it, so one pane overrunning its
	// budget - a terminal too short for a pane's own chrome, a component
	// that renders one line more than it was configured for - makes
	// JoinHorizontal below align two boxes of different heights, and the
	// screen comes apart. Clipping keeps that a local defect in one pane
	// instead of a broken frame.
	leftPane := pane(layout.leftContentWidth, layout.leftContentHeight, !m.previewFocused).
		Render(clipLines(left.String(), layout.leftContentHeight))
	rightPane := pane(layout.rightContentWidth, layout.rightContentHeight, m.previewFocused).
		Render(clipLines(right.String(), layout.rightContentHeight))
	if layout.stacked {
		return lipgloss.JoinVertical(lipgloss.Left, leftPane, rightPane)
	}
	return lipgloss.JoinHorizontal(lipgloss.Top, leftPane, " ", rightPane)
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

// Resize records the fixed pane dimensions and sizes both independently
// scrollable components to their content boxes.
func (m Model) Resize(width, height int) ui.ScreenModel {
	next := m
	next.width = width
	next.height = height
	layout := calculateLayout(width, height)

	listHeight := max(1, layout.leftContentHeight-5)
	next.list = next.list.SetHeight(listHeight).SetWidth(max(1, layout.leftContentWidth-2))
	next.previewViewport.Width = max(1, layout.rightContentWidth-2)
	next.previewViewport.Height = max(1, layout.rightContentHeight-3)
	next.searchInput.Width = max(1, layout.rightContentWidth-4)
	next.pathInput.Width = max(1, layout.leftContentWidth-4)
	return next
}

// Host returns the hostname this Model was constructed with.
func (m Model) Host() string {
	return m.host
}

// Entries returns every scan result received so far, in arrival order. The
// returned slice is owned by Model; a caller must not mutate it.
func (m Model) Entries() []ui.ScanEntry {
	return m.entries
}

// Highlighted returns the entry currently under the file list's cursor, and
// true. It returns the zero value and false before any entry has ever been
// highlighted.
func (m Model) Highlighted() (ui.ScanEntry, bool) {
	if !m.hasHighlight {
		return ui.ScanEntry{}, false
	}
	if idx := indexOfPath(m.entries, m.highlightedPath); idx >= 0 {
		return m.entries[idx], true
	}
	return ui.ScanEntry{}, false
}

// PreviewLines returns the content currently rendered in the preview pane:
// the lines of the last cache hit or completed, still-current-generation
// preview reply. It is nil until one of those has happened.
func (m Model) PreviewLines() []string {
	return m.previewLines
}

// PreviewRequestCount returns how many preview requests Update has issued
// in this Model's history: once per ui.PreviewDebounceMsg whose Gen
// matched the current generation and whose (host, path) was a cache miss.
// A selection change alone, and a debounce tick that arrives stale or
// hits the cache, never advance it (mechanics 2, 3, 4).
func (m Model) PreviewRequestCount() int {
	return m.requestCount
}

// Gen returns the current generation counter: the value the next
// ui.PreviewDebounceMsg issued for the current selection will carry, and
// the value any ui.PreviewDebounceMsg or ui.PreviewMsg must carry to still
// be current rather than stale (mechanic 3).
func (m Model) Gen() uint64 {
	return m.gen
}

// PreviewOffset returns the first preview row currently visible.
func (m Model) PreviewOffset() int {
	return m.previewViewport.YOffset
}

// PreviewFocused reports whether navigation keys scroll the preview pane.
func (m Model) PreviewFocused() bool {
	return m.previewFocused
}

// SelectedPath returns the remote path committed with Space.
func (m Model) SelectedPath() string {
	return m.selectedPath
}

// SelectedLocal returns the fetched snapshot searched by the right pane.
func (m Model) SelectedLocal() string {
	return m.selectedLocal
}

// Searching reports whether the right-pane search input is focused.
func (m Model) Searching() bool {
	return m.searching
}

// SearchMatchCount returns the number of matches from the latest search.
func (m Model) SearchMatchCount() int {
	return len(m.matches)
}

// CurrentSearchMatchIndex returns the active search result index, or -1.
func (m Model) CurrentSearchMatchIndex() int {
	return m.matchIdx
}

// FilteredLineCount returns how many source lines remain visible for the live
// query. Multiple hits on one source line count as one visible line.
func (m Model) FilteredLineCount() int {
	return len(m.filteredLineNumbers)
}

// ActiveScanPath returns the one-off path currently being discovered.
func (m Model) ActiveScanPath() string {
	return m.activeScanPath
}

// Following reports whether the preview pane is a live tail.
func (m Model) Following() bool {
	return m.following
}

// FollowPath returns the remote file currently being tailed, or empty.
func (m Model) FollowPath() string {
	return m.followPath
}

// FollowLineCount returns how many live-tail lines the preview currently holds.
func (m Model) FollowLineCount() int {
	return len(m.followLines)
}
