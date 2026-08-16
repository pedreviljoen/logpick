package transport

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sentinel errors Mock.Exec returns. Callers match them with errors.Is; the
// wrapped message additionally names the offending command or path, per
// AGENTS.md section 5's "errors.Is plus strings.Contains" rule.
var (
	// ErrUnrecognisedCommand is returned when cmd does not match one of the
	// five shapes documented on Mock: the find scan, tail -n N <path>,
	// tail -f <path>, cat <path>, and find --version.
	ErrUnrecognisedCommand = errors.New("mock: unrecognised command")

	// ErrPathOutsideRoot is returned when a path argument to a recognised
	// shape (a find scan root, or the argument to tail or cat) maps, after
	// cleaning, to a location outside the directory Mock serves.
	ErrPathOutsideRoot = errors.New("mock: path escapes fixture root")
)

// Mock serves a fixture directory (see internal/transport/testdata/fixtures)
// as if it were a remote filesystem reachable over Transport.Exec. Per
// DESIGN.md 7.4 and 13, it is the test seam every package above transport is
// tested against, and it also backs the tool's --mock <dir> flag, which is
// why it lives in mock.go rather than a _test.go file: non-test code
// depends on it too.
//
// Mock recognises exactly five command shapes, the ones the rest of the
// tool ever issues over Exec, and returns an error naming the command for
// anything else. It does not spawn or interpret a shell: cmd is parsed
// directly by this package's Exec, so nothing beyond the single-quoting
// BuildScan (T09) uses around literal paths and -name patterns is treated
// as syntax.
//
// # Path mapping
//
// dir is served as the remote "/". A remote path P (always absolute,
// always "/"-separated, since the remote is modelled as a POSIX host
// regardless of what this test binary itself runs on) maps to
// filepath.Join(dir, filepath.FromSlash(strings.TrimPrefix(P, "/"))). The
// joined path is cleaned and checked against dir: if cleaning needed to
// walk above dir (a ".." that escapes the served tree), Exec returns an
// error wrapping ErrPathOutsideRoot instead of touching the filesystem.
// This guard applies to every path Mock reads: every root of a find scan,
// and the sole path argument of tail and cat.
//
// # Recognised shapes
//
//  1. The find scan (DESIGN.md 8.1, T09's BuildScan). Parsed permissively —
//     the roots, -maxdepth, -name include patterns and ! -name excludes are
//     extracted, not string-matched against one fixed layout — so a small
//     change to BuildScan's flag order or spacing does not break every
//     downstream task that exercises a scan through Mock. Recognised
//     fields, in the order BuildScan emits them:
//
//     - one or more roots. A root written as a single-quoted string is a
//     literal path. A root written unquoted, containing '*', '?' or '[',
//     is matched as a glob against the fixture tree (filepath.Glob
//     semantics) and each match becomes its own candidate the same way a
//     remote shell would have expanded it before find ever saw it.
//     - an optional "-maxdepth N": N is the number of levels below a root
//     Mock will descend, the root itself being depth 0 — matching GNU
//     find's own definition. Omitted, descent is unbounded.
//     - "-type f": required; every match is a regular file.
//     - an optional "\( -name 'PAT' [-o -name 'PAT' ...] \)": a file is a
//     candidate only if its base name matches at least one PAT
//     (path.Match semantics). Absent, every regular file found under a
//     root is a candidate.
//     - zero or more "! -name 'PAT'": a candidate matching any of these
//     patterns is excluded.
//     - the output clause, which selects the response format below:
//     "-printf '%s\t%T@\t%p\n'" selects the GNU form; "-exec ls -ldn --
//     {} +" selects the BSD form. Either may be followed by
//     "2>/dev/null", which is recognised and ignored, since Mock is not
//     a shell and there is no stderr redirection for it to perform.
//
//     GNU form: one line per match, in "%d\t%d.%09d\t%s\n" — the file's
//     size in bytes, then its modification time as Unix seconds and a
//     nine-digit nanosecond fraction (both taken from the fixture file's
//     own os.Stat, exactly as GNU find's %s and %T@ would report them for
//     a real file), then the match's absolute remote path reconstructed
//     with "/" separators.
//
//     BSD form: one line per match, in "-rw-r--r-- 1 501 20 %d %s %d
//     %d:%d %s\n" — a fixed mode and a fixed uid/gid Mock always reports
//     (501/20; their value carries no meaning beyond being present as
//     fields), the size, the match's modification time as a three-letter
//     month and an unpadded day, then its hour and minute zero-padded to
//     two digits, then the absolute remote path. Real ls right-pads and
//     aligns these columns; Mock deliberately does not, since the only
//     contract a consumer can rely on (T10's parser) is that the path is
//     everything after the first eight whitespace-separated fields, and
//     column alignment is not part of that contract. Mock never emits the
//     year-form ls uses for files older than about six months: every
//     fixture is "recent" by construction.
//
//     Matches are emitted in a stable order, lexical by full remote path,
//     so a test can assert on exact output.
//
//  2. "tail -n N <path>": the last N lines of the mapped file, exactly as
//     GNU tail -n would produce them — if the file has fewer than N lines
//     the whole file is returned, and the output ends in a newline if and
//     only if the source file itself does.
//
//  3. "tail -f <path>": the mapped file's full current content, followed by
//     a stream that stays open with no further writes — the fixture corpus
//     is static, so there is never new content to follow — until ctx is
//     cancelled. This is the shape T20's follow mode is tested against.
//     Once ctx is cancelled, Process.Stdout reports io.EOF and Wait
//     returns ctx.Err(), never nil: unlike the other four shapes, a
//     tail -f that ends because the caller cancelled it did not "succeed"
//     in the sense Process's doc comment otherwise means by that word, it
//     was cut off, exactly as cancelling a real tail -f would cut it off.
//
//  4. "cat <path>": the mapped file's full content, verbatim, including an
//     empty result for a zero-length file.
//
//  5. "find --version": whether this succeeds is Mock's answer to the GNU
//     probe (DESIGN.md 8.2), and it is controlled entirely by
//     Caps().GNUFind, which WithCaps overrides — there is no separate
//     option for it, on the view that "which find dialect this Mock
//     pretends to be" and "what its Caps say" must never be able to
//     disagree. When GNUFind is nil (the default, see "Caps" below) or
//     true, Exec succeeds with a one-line GNU version banner on Stdout and
//     Wait returns nil. When GNUFind is false, Exec returns a *ExitError
//     with a non-zero code and a BSD-style "illegal option" Stderr, so
//     Wait returns a non-nil error. Callers should treat any non-nil error
//     from this shape as "not GNU", the same way DESIGN.md 8.2's `find
//     --version >/dev/null 2>&1 && echo gnu || echo other` treats it,
//     rather than inspecting the code.
//
// Any cmd that does not match one of the five shapes above returns an
// error wrapping ErrUnrecognisedCommand and naming cmd.
//
// # Latency and failure injection
//
// WithLatency(d) makes every call to Exec — including the one call
// WithFailure fails, and an unrecognised-command call — wait d before doing
// anything else. The wait selects on ctx.Done() against a timer rather than
// calling time.Sleep, so a caller that cancels ctx during the delay is not
// blocked for the rest of it: Exec returns ctx.Err() as soon as ctx is
// done, whichever comes first.
//
// WithFailure(err) is consumed, not permanent: it makes exactly the next
// call to Exec return (nil, err) without inspecting cmd at all — even an
// otherwise-unrecognised command "fails" with err, not
// ErrUnrecognisedCommand, on that one call — and every call after it is
// evaluated normally. A Mock with no WithFailure configured, or one whose
// injected failure has already been consumed, never fails Exec on its own
// account.
//
// # Caps
//
// Caps() returns Caps{Follow: true, ConcurrentExec: true, BinarySafe:
// true, NativeCopy: false, GNUFind: <points to true>} unless WithCaps
// overrides it, in which case Caps() returns exactly the value WithCaps
// was given, including a nil GNUFind if that is what the caller passed —
// WithCaps replaces the default outright, it does not merge into it.
// NativeCopy is false because Mock has no native copy shape to fake: Fetch
// is inherited, unmodified, from the embedded FallbackFetcher, which
// streams "cat <remote>" over this same Exec — the shape Mock already
// recognises — so Fetch transparently goes through the same path mapping,
// latency and failure injection Exec does, with no separate code path to
// keep in sync.
//
// # Concurrency
//
// Exec may be called concurrently (Caps().ConcurrentExec defaults to
// true); the one-shot failure injection and the caps override are both
// guarded so concurrent Exec calls never race on them.
type Mock struct {
	FallbackFetcher

	// dir is the fixture directory Mock serves as the remote "/".
	dir string

	// mu guards nextErr and caps, both of which a concurrent Exec or Caps
	// call may read or, for nextErr, consume.
	mu sync.Mutex

	// latency is the cancellable delay WithLatency configures. Zero means
	// no delay.
	latency time.Duration

	// nextErr is the error WithFailure injected, if any, not yet consumed
	// by an Exec call. Exec clears it back to nil the one time it is used.
	nextErr error

	// caps is the value Caps() returns. WithCaps overrides it wholesale;
	// otherwise it holds the default described in the Mock doc comment's
	// "Caps" section.
	caps Caps
}

var _ Transport = (*Mock)(nil)

// MockOption configures a Mock at construction time, the functional options
// shape AGENTS.md 4.1 requires once a constructor grows past three
// parameters. Options are applied once, in NewMock, in the order given;
// there is no way to reconfigure a Mock after construction.
type MockOption func(*Mock)

// NewMock builds a Mock that serves dir as a remote filesystem rooted at
// "/". See the Mock doc comment for the command shapes it recognises, the
// path mapping and traversal guard, and what each option controls. NewMock
// does not stat dir: a missing or unreadable directory surfaces as an error
// on the first Exec that tries to read from it, not at construction.
func NewMock(dir string, opts ...MockOption) *Mock {
	m := &Mock{dir: dir, caps: defaultMockCaps()}
	m.FallbackFetcher = FallbackFetcher{T: m}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// defaultMockCaps builds the Caps value Mock reports until WithCaps
// overrides it. See the Mock doc comment's "Caps" section.
func defaultMockCaps() Caps {
	gnuFind := true
	return Caps{
		NativeCopy:     false,
		Follow:         true,
		ConcurrentExec: true,
		BinarySafe:     true,
		GNUFind:        &gnuFind,
	}
}

// WithLatency makes every Exec call wait d, cancellably, before doing
// anything else. See the Mock doc comment's "Latency and failure injection"
// section for the exact cancellation contract.
func WithLatency(d time.Duration) MockOption {
	return func(m *Mock) {
		m.latency = d
	}
}

// WithFailure makes exactly the next call to Exec return (nil, err), with
// cmd left uninspected, and every call after that evaluated normally. See
// the Mock doc comment's "Latency and failure injection" section.
func WithFailure(err error) MockOption {
	return func(m *Mock) {
		m.nextErr = err
	}
}

// WithCaps overrides the Caps Mock reports, replacing the default entirely
// rather than merging into it. It is also, per the Mock doc comment's
// "Recognised shapes" section on find --version, how a caller selects
// whether this Mock behaves as a GNU or a BSD find.
func WithCaps(caps Caps) MockOption {
	return func(m *Mock) {
		m.caps = caps
	}
}

// Exec runs cmd against the fixture directory this Mock serves. See the
// Mock doc comment for the five shapes it recognises, their exact output
// formats, the path mapping and traversal guard, and how latency and
// failure injection interact with it.
func (m *Mock) Exec(ctx context.Context, cmd string) (*Process, error) {
	if err := m.waitLatency(ctx); err != nil {
		return nil, err
	}
	if err := m.takeFailure(); err != nil {
		return nil, err
	}
	return m.exec(ctx, cmd)
}

// waitLatency implements the cancellable delay WithLatency configures. It
// selects on ctx.Done() against a timer rather than calling time.Sleep, so
// a caller that cancels ctx during the delay is not blocked for the rest of
// it.
func (m *Mock) waitLatency(ctx context.Context) error {
	if m.latency <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(m.latency)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// takeFailure consumes and returns the pending WithFailure injection, if
// any, guarded against concurrent Exec calls.
func (m *Mock) takeFailure() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	err := m.nextErr
	m.nextErr = nil
	return err
}

// exec dispatches cmd, already past latency and failure injection, to
// whichever of the five recognised shapes it matches.
func (m *Mock) exec(ctx context.Context, cmd string) (*Process, error) {
	tokens, err := tokenize(cmd)
	if err != nil {
		return nil, unrecognised(cmd)
	}
	if len(tokens) == 0 {
		return nil, unrecognised(cmd)
	}

	switch {
	case len(tokens) == 2 && tokens[0] == "find" && tokens[1] == "--version":
		return m.execFindVersion(), nil

	case tokens[0] == "find":
		spec, ok := parseScan(tokens)
		if !ok {
			return nil, unrecognised(cmd)
		}
		return m.execScan(spec)

	case tokens[0] == "tail" && len(tokens) >= 2 && tokens[1] == "-n":
		return m.execTailN(cmd, tokens)

	case tokens[0] == "tail" && len(tokens) >= 2 && tokens[1] == "-f":
		return m.execTailF(ctx, cmd, tokens)

	case tokens[0] == "cat":
		return m.execCat(cmd, tokens)

	default:
		return nil, unrecognised(cmd)
	}
}

// unrecognised wraps ErrUnrecognisedCommand, naming cmd.
func unrecognised(cmd string) error {
	return fmt.Errorf("%w: %s", ErrUnrecognisedCommand, cmd)
}

// mapPath maps a remote path to its local counterpart under m.dir, per the
// Mock doc comment's "Path mapping" section. It is pure string manipulation
// — no filesystem access — so the traversal guard is checked before
// anything is read.
func (m *Mock) mapPath(remote string) (string, error) {
	rel := strings.TrimPrefix(remote, "/")
	joined := filepath.Join(m.dir, filepath.FromSlash(rel))
	cleanDir := filepath.Clean(m.dir)
	if joined != cleanDir && !strings.HasPrefix(joined, cleanDir+string(filepath.Separator)) {
		return "", fmt.Errorf("%s: %w", remote, ErrPathOutsideRoot)
	}
	return joined, nil
}

// readFile reads the local file at path. path has already been produced by
// mapPath's traversal guard by the time this is called, so this is not
// reading an arbitrary caller-supplied path.
func readFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // path is validated by mapPath's traversal guard before this is ever called.
}

// execCat implements the "cat <path>" shape.
func (m *Mock) execCat(cmd string, tokens []string) (*Process, error) {
	if len(tokens) != 2 {
		return nil, unrecognised(cmd)
	}
	remote := unquote(tokens[1])
	local, err := m.mapPath(remote)
	if err != nil {
		return nil, err
	}
	data, err := readFile(local)
	if err != nil {
		return nil, fmt.Errorf("cat %s: %w", remote, err)
	}
	return &Process{
		Stdout: io.NopCloser(bytes.NewReader(data)),
		Stderr: io.NopCloser(strings.NewReader("")),
		Wait:   func() error { return nil },
	}, nil
}

// execTailN implements the "tail -n N <path>" shape.
func (m *Mock) execTailN(cmd string, tokens []string) (*Process, error) {
	if len(tokens) != 4 {
		return nil, unrecognised(cmd)
	}
	n, err := strconv.Atoi(tokens[2])
	if err != nil {
		return nil, unrecognised(cmd)
	}
	remote := unquote(tokens[3])
	local, err := m.mapPath(remote)
	if err != nil {
		return nil, err
	}
	data, err := readFile(local)
	if err != nil {
		return nil, fmt.Errorf("tail -n %d %s: %w", n, remote, err)
	}
	out := tailN(data, n)
	return &Process{
		Stdout: io.NopCloser(bytes.NewReader(out)),
		Stderr: io.NopCloser(strings.NewReader("")),
		Wait:   func() error { return nil },
	}, nil
}

// tailN reproduces GNU tail -n's output for data: the last n lines, or the
// whole file if it has fewer, preserving a trailing newline if and only if
// the source has one.
func tailN(data []byte, n int) []byte {
	if len(data) == 0 {
		return []byte{}
	}
	text := string(data)
	trailingNL := strings.HasSuffix(text, "\n")
	if trailingNL {
		text = strings.TrimSuffix(text, "\n")
	}
	lines := strings.Split(text, "\n")
	if n < len(lines) {
		lines = lines[len(lines)-n:]
	}
	out := strings.Join(lines, "\n")
	if trailingNL {
		out += "\n"
	}
	return []byte(out)
}

// execTailF implements the "tail -f <path>" shape: the mapped file's
// current content, followed by a stream that stays open until ctx is
// cancelled.
func (m *Mock) execTailF(ctx context.Context, cmd string, tokens []string) (*Process, error) {
	if len(tokens) != 3 {
		return nil, unrecognised(cmd)
	}
	remote := unquote(tokens[2])
	local, err := m.mapPath(remote)
	if err != nil {
		return nil, err
	}
	data, err := readFile(local)
	if err != nil {
		return nil, fmt.Errorf("tail -f %s: %w", remote, err)
	}

	fr := &followReader{content: data, ctx: ctx}
	return &Process{
		Stdout: fr,
		Stderr: io.NopCloser(strings.NewReader("")),
		Wait: func() error {
			<-ctx.Done()
			return ctx.Err()
		},
	}, nil
}

// followReader serves content and then blocks on ctx.Done() before
// reporting io.EOF, implementing tail -f's "stay open until cancelled"
// contract without spawning a goroutine: the block happens inside the
// caller's own Read call, so nothing is left running once Read returns.
type followReader struct {
	content []byte
	pos     int
	ctx     context.Context
}

func (f *followReader) Read(p []byte) (int, error) {
	if f.pos < len(f.content) {
		n := copy(p, f.content[f.pos:])
		f.pos += n
		return n, nil
	}
	<-f.ctx.Done()
	return 0, io.EOF
}

func (f *followReader) Close() error { return nil }

// execFindVersion implements the "find --version" shape, driven entirely by
// Caps().GNUFind.
func (m *Mock) execFindVersion() *Process {
	caps := m.Caps()
	if caps.GNUFind == nil || *caps.GNUFind {
		return &Process{
			Stdout: io.NopCloser(strings.NewReader("find (GNU findutils) 4.9.0\n")),
			Stderr: io.NopCloser(strings.NewReader("")),
			Wait:   func() error { return nil },
		}
	}

	stderr := "find: illegal option -- -version\nusage: find [-H | -L | -P] [-EXdsx] [-f path] path ... [expression]\n"
	return &Process{
		Stdout: io.NopCloser(strings.NewReader("")),
		Stderr: io.NopCloser(strings.NewReader(stderr)),
		Wait: func() error {
			return &ExitError{Code: 1, Stderr: stderr}
		},
	}
}

// scanSpec is a permissively parsed find scan command: DESIGN.md 8.1's
// shape, as BuildScan (T09) emits it.
type scanSpec struct {
	roots    []string // root tokens, as written: quoted literals or unquoted (possibly glob) paths.
	maxdepth *int
	includes []string
	excludes []string
	format   string // "gnu" or "bsd"
}

// parseScan attempts to parse tokens (already split by tokenize) as a find
// scan. It reports ok=false, rather than an error, for anything that does
// not match: the caller treats that the same as any other unrecognised
// command.
func parseScan(tokens []string) (*scanSpec, bool) {
	if len(tokens) == 0 || tokens[0] != "find" {
		return nil, false
	}
	idx := 1

	var roots []string
	for idx < len(tokens) && tokens[idx] != "-maxdepth" && tokens[idx] != "-type" {
		roots = append(roots, tokens[idx])
		idx++
	}
	if len(roots) == 0 {
		return nil, false
	}
	spec := &scanSpec{roots: roots}

	if idx < len(tokens) && tokens[idx] == "-maxdepth" {
		idx++
		if idx >= len(tokens) {
			return nil, false
		}
		n, err := strconv.Atoi(tokens[idx])
		if err != nil {
			return nil, false
		}
		spec.maxdepth = &n
		idx++
	}

	if idx >= len(tokens) || tokens[idx] != "-type" {
		return nil, false
	}
	idx++
	if idx >= len(tokens) || tokens[idx] != "f" {
		return nil, false
	}
	idx++

	if idx < len(tokens) && tokens[idx] == `\(` {
		idx++
		for {
			if idx >= len(tokens) || tokens[idx] != "-name" {
				return nil, false
			}
			idx++
			if idx >= len(tokens) {
				return nil, false
			}
			spec.includes = append(spec.includes, unquote(tokens[idx]))
			idx++
			if idx < len(tokens) && tokens[idx] == "-o" {
				idx++
				continue
			}
			break
		}
		if idx >= len(tokens) || tokens[idx] != `\)` {
			return nil, false
		}
		idx++
	}

	for idx < len(tokens) && tokens[idx] == "!" {
		idx++
		if idx >= len(tokens) || tokens[idx] != "-name" {
			return nil, false
		}
		idx++
		if idx >= len(tokens) {
			return nil, false
		}
		spec.excludes = append(spec.excludes, unquote(tokens[idx]))
		idx++
	}

	if idx >= len(tokens) {
		return nil, false
	}
	switch tokens[idx] {
	case "-printf":
		spec.format = "gnu"
		idx++
		if idx >= len(tokens) {
			return nil, false
		}
		idx++ // the -printf format string itself; its content is not part of the contract.
	case "-exec":
		spec.format = "bsd"
		idx++
		for _, want := range []string{"ls", "-ldn", "--", "{}", "+"} {
			if idx >= len(tokens) || tokens[idx] != want {
				return nil, false
			}
			idx++
		}
	default:
		return nil, false
	}

	if idx < len(tokens) && tokens[idx] == "2>/dev/null" {
		idx++
	}

	if idx != len(tokens) {
		return nil, false
	}
	return spec, true
}

// scanMatch is one file a find scan matched: its absolute remote path and
// the os.FileInfo Mock stat'd it with.
type scanMatch struct {
	remote string
	info   os.FileInfo
}

// execScan runs a parsed find scan against the fixture tree and formats the
// result per spec.format.
func (m *Mock) execScan(spec *scanSpec) (*Process, error) {
	var candidates []scanMatch
	for _, rootTok := range spec.roots {
		remoteRoots, err := m.expandRoot(rootTok)
		if err != nil {
			return nil, err
		}
		for _, remoteRoot := range remoteRoots {
			localRoot, err := m.mapPath(remoteRoot)
			if err != nil {
				return nil, err
			}
			matches, err := m.collectMatches(localRoot, spec.maxdepth)
			if err != nil {
				return nil, fmt.Errorf("scanning %s: %w", remoteRoot, err)
			}
			candidates = append(candidates, matches...)
		}
	}

	filtered := candidates[:0]
	for _, c := range candidates {
		base := path.Base(c.remote)
		if len(spec.includes) > 0 && !matchesAny(spec.includes, base) {
			continue
		}
		if matchesAny(spec.excludes, base) {
			continue
		}
		filtered = append(filtered, c)
	}

	sort.Slice(filtered, func(i, j int) bool { return filtered[i].remote < filtered[j].remote })

	var buf bytes.Buffer
	for _, c := range filtered {
		mt := c.info.ModTime()
		switch spec.format {
		case "gnu":
			fmt.Fprintf(&buf, "%d\t%d.%09d\t%s\n", c.info.Size(), mt.Unix(), mt.Nanosecond(), c.remote)
		case "bsd":
			fmt.Fprintf(&buf, "-rw-r--r-- 1 501 20 %d %s %d %02d:%02d %s\n",
				c.info.Size(), mt.Format("Jan"), mt.Day(), mt.Hour(), mt.Minute(), c.remote)
		}
	}

	return &Process{
		Stdout: io.NopCloser(bytes.NewReader(buf.Bytes())),
		Stderr: io.NopCloser(strings.NewReader("")),
		Wait:   func() error { return nil },
	}, nil
}

// matchesAny reports whether name matches any of patterns, path.Match
// semantics. A malformed pattern is treated as not matching.
func matchesAny(patterns []string, name string) bool {
	for _, p := range patterns {
		if ok, err := path.Match(p, name); err == nil && ok {
			return true
		}
	}
	return false
}

// expandRoot turns one root token into the absolute remote paths it names:
// a single-quoted token is a literal path; an unquoted token containing
// '*', '?' or '[' is expanded via filepath.Glob against the fixture tree;
// anything else is a literal path too.
func (m *Mock) expandRoot(tok string) ([]string, error) {
	if isQuoted(tok) {
		return []string{unquote(tok)}, nil
	}
	if !strings.ContainsAny(tok, "*?[") {
		return []string{tok}, nil
	}

	rel := strings.TrimPrefix(tok, "/")
	localPattern := filepath.Join(m.dir, filepath.FromSlash(rel))
	matches, err := filepath.Glob(localPattern)
	if err != nil {
		return nil, fmt.Errorf("expanding %s: %w", tok, err)
	}

	out := make([]string, 0, len(matches))
	for _, localMatch := range matches {
		relMatch, err := filepath.Rel(m.dir, localMatch)
		if err != nil {
			return nil, fmt.Errorf("expanding %s: %w", tok, err)
		}
		out = append(out, "/"+filepath.ToSlash(relMatch))
	}
	return out, nil
}

// collectMatches walks localRoot (a file or a directory, already validated
// by mapPath) collecting every regular file at depth <= maxdepth (root
// itself is depth 0), or every regular file if maxdepth is nil. A missing
// root yields no matches rather than an error, the same way "2>/dev/null"
// silently drops a real find's complaint about it.
func (m *Mock) collectMatches(localRoot string, maxdepth *int) ([]scanMatch, error) {
	info, err := os.Lstat(localRoot) //nolint:gosec // localRoot is derived from a scan root already validated by mapPath's traversal guard.
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var out []scanMatch
	if err := m.walk(localRoot, info, 0, maxdepth, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Mock) walk(p string, info os.FileInfo, depth int, maxdepth *int, out *[]scanMatch) error {
	if maxdepth != nil && depth > *maxdepth {
		return nil
	}

	if info.Mode().IsRegular() {
		rel, err := filepath.Rel(m.dir, p)
		if err != nil {
			return err
		}
		*out = append(*out, scanMatch{remote: "/" + filepath.ToSlash(rel), info: info})
		return nil
	}

	if !info.IsDir() {
		return nil
	}

	entries, err := os.ReadDir(p) //nolint:gosec // p is derived from a scan root already validated by mapPath's traversal guard.
	if err != nil {
		return err
	}
	for _, e := range entries {
		childInfo, err := e.Info()
		if err != nil {
			return err
		}
		if err := m.walk(filepath.Join(p, e.Name()), childInfo, depth+1, maxdepth, out); err != nil {
			return err
		}
	}
	return nil
}

// tokenize splits cmd into whitespace-separated tokens, treating a
// single-quoted run (including its quotes, in the returned token) as one
// token regardless of any whitespace inside it. It is not a shell lexer:
// there is no escaping and no support for double quotes, since nothing this
// package's own callers construct (BuildScan, T09) ever needs them.
func tokenize(cmd string) ([]string, error) {
	var tokens []string
	i := 0
	n := len(cmd)
	for i < n {
		for i < n && isSpace(cmd[i]) {
			i++
		}
		if i >= n {
			break
		}
		if cmd[i] == '\'' {
			j := i + 1
			for j < n && cmd[j] != '\'' {
				j++
			}
			if j >= n {
				return nil, fmt.Errorf("unterminated quote in %q", cmd)
			}
			tokens = append(tokens, cmd[i:j+1])
			i = j + 1
			continue
		}
		j := i
		for j < n && !isSpace(cmd[j]) {
			j++
		}
		tokens = append(tokens, cmd[i:j])
		i = j
	}
	return tokens, nil
}

func isSpace(b byte) bool {
	return b == ' ' || b == '\t' || b == '\n' || b == '\r'
}

// isQuoted reports whether tok is a single-quoted token, as tokenize
// produces for a run of characters that started with a single quote.
func isQuoted(tok string) bool {
	return len(tok) >= 2 && tok[0] == '\'' && tok[len(tok)-1] == '\''
}

// unquote strips tok's enclosing single quotes if it is a quoted token, and
// returns it unchanged otherwise.
func unquote(tok string) string {
	if isQuoted(tok) {
		return tok[1 : len(tok)-1]
	}
	return tok
}

// Caps reports the Caps this Mock was built with: the default described in
// the Mock doc comment's "Caps" section, or WithCaps's argument verbatim if
// that option was given.
func (m *Mock) Caps() Caps {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.caps
}

// Check is a cheap liveness probe. It reports ctx.Err() if ctx is already
// done, an error if dir is no longer a readable directory, and nil
// otherwise. Check does not consume a WithFailure injection and does not
// apply WithLatency: both are specified in terms of Exec only.
func (m *Mock) Check(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Stat(m.dir)
	if err != nil {
		return fmt.Errorf("check %s: %w", m.dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("check %s: not a directory", m.dir)
	}
	return nil
}

// Close releases any resources Mock holds open between calls. Mock keeps no
// persistent session — every Exec reads the fixture tree directly — so
// there is nothing to release; Close always returns nil.
func (m *Mock) Close() error {
	return nil
}
