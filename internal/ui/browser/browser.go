package browser

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

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
// path, which is what the picker's fuzzy match runs against.
type entryItem struct {
	// Entry is the wrapped scan result.
	Entry ui.ScanEntry
}

// FilterValue returns the entry's remote path.
func (i entryItem) FilterValue() string { return i.Entry.Path }

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
	// host is the hostname every entry, preview request and cache entry in
	// this Model belongs to.
	host string

	// preview fetches a preview's lines. See PreviewFunc.
	preview PreviewFunc

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
	m := Model{
		host:    host,
		preview: preview,
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

	case ui.ScanDoneMsg:
		if msg.Host != m.host {
			return m, nil
		}
		next := m
		next.scanDone = true
		return next, nil

	case tea.KeyMsg:
		next := m
		var listCmd tea.Cmd
		next.list, listCmd = next.list.Update(msg)
		result, hiCmd := next.applyHighlightChange()
		return result, tea.Batch(listCmd, hiCmd)

	case ui.PreviewDebounceMsg:
		return m.handleDebounce(msg)

	case ui.PreviewMsg:
		return m.handlePreviewMsg(msg)

	default:
		return m, nil
	}
}

// withEntries returns a copy of m with entries replaced by newEntries and
// list rebuilt over them. picker.Model offers no way to grow its item set
// other than constructing a fresh one over the whole slice (New always
// starts its cursor at 0), so when a highlighted path is still present in
// newEntries, the fresh list's cursor is advanced back onto it by replaying
// ctrl+n, which keeps a streaming scan from yanking the user's highlight
// back to the top of the list every time a new batch arrives.
func (m Model) withEntries(newEntries []ui.ScanEntry) Model {
	m.entries = newEntries

	items := make([]entryItem, len(newEntries))
	for i, e := range newEntries {
		items[i] = entryItem{Entry: e}
	}
	newList := picker.New(items)

	if m.hasHighlight {
		if idx := indexOfPath(newEntries, m.highlightedPath); idx > 0 {
			for i := 0; i < idx; i++ {
				newList, _ = newList.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
			}
		}
	}

	m.list = newList
	return m
}

// indexOfPath returns the index of the entry with the given path in
// entries, or -1 if none matches.
func indexOfPath(entries []ui.ScanEntry, path string) int {
	for i, e := range entries {
		if e.Path == path {
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

	if !ok {
		return next, nil
	}

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

	if entry, reordered, ok := cacheLookup(m.cache, msg.Host, msg.Path); ok {
		next := m
		next.cache = reordered
		next.previewLines = entry.lines
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

// View renders the split pane: the file list on the left, the preview pane
// on the right, sized to the area Resize was last called with.
func (m Model) View() string {
	var b strings.Builder

	b.WriteString(m.host)
	if m.scanDone {
		b.WriteString(" (scan done)")
	}
	b.WriteByte('\n')

	b.WriteString(m.list.View())
	b.WriteString("---\n")

	for _, line := range m.previewLines {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	return b.String()
}

// Resize records the area the screen has to render into and returns the
// updated Model.
func (m Model) Resize(width, height int) ui.ScreenModel {
	next := m
	next.width = width
	next.height = height

	listHeight := height - 4
	if listHeight < 0 {
		listHeight = 0
	}
	next.list = next.list.SetHeight(listHeight)

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
	for _, e := range m.entries {
		if e.Path == m.highlightedPath {
			return e, true
		}
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
