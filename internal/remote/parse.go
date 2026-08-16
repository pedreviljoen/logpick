package remote

import (
	"bufio"
	"io"
	"strconv"
	"strings"
	"time"
)

// Entry is one log file discovered by a scan: the parsed form of a single
// line of whatever BuildScan (discover.go) asked the remote find to
// produce, either GNU find's -printf output or the BSD/macOS/busybox
// ls -ldn fallback.
//
// Field names and types are chosen to match ui.ScanEntry
// (internal/ui/msg.go) exactly, so that ui.ScanEntry can become a type
// alias for this type once T10 lands, per that file's doc comment. Do not
// rename or retype a field here without updating ui.ScanEntry to match.
type Entry struct {
	// Path is the absolute path on the remote host.
	Path string
	// Size is the file size in bytes, as reported by find (GNU) or ls
	// (BSD).
	Size int64
	// ModTime is the file's last modification time, the default sort key
	// (DESIGN.md 8.1).
	//
	// GNU input carries it exactly: find's %T@ field is Unix epoch seconds
	// with a fractional part, for example:
	//
	//	1755180171.4400000000
	//
	// That fraction must be parsed as a float, or split on the '.' and
	// handled digit by digit; parsing %T@ as an integer truncates the
	// string at the decimal point on some parsers and simply fails on
	// others, and either way is wrong on every real host, since %T@ is
	// never a bare integer.
	//
	// BSD input (ls -ldn) does not carry a usable mtime here: its date
	// column has no year ("Aug 14 16:02") and its meaning is
	// locale-dependent, so recovering a real timestamp means guessing a
	// year. A wrong guess sorts a log up to a year out of place, which is
	// worse than not sorting it by time at all. The decision for T10 is to
	// leave ModTime as the zero time.Time for every BSD-parsed Entry
	// rather than guess. This means hosts without GNU find lose mtime sort
	// precision - BSD entries all sort together, most naturally last, when
	// the browser sorts by mtime descending - and that loss is accepted
	// and documented here rather than silently masked by a fabricated
	// timestamp.
	ModTime time.Time
}

// Report summarises one call to ParseScan.
type Report struct {
	// Count is how many entries were parsed and sent on the out channel
	// (or would have been, had out been non-nil; see ParseScan's "Nil out"
	// section).
	Count int
	// Skipped is how many input lines could not be parsed into an Entry
	// and were dropped instead. This includes malformed lines and, per
	// mechanic 4, an incomplete final line. It is never fatal; see
	// ParseScan's "Errors" section for what is.
	Skipped int
	// Truncated reports whether parsing stopped at the 10,000-entry cap
	// (DESIGN.md 8.3) before the input was exhausted. See ParseScan's "The
	// 10,000-entry cap" section for what happens to the remainder of r
	// when this is true.
	Truncated bool
}

// ParseScan reads the stdout of the command BuildScan (discover.go) built,
// parses each line into an Entry, and returns a Report summarising the
// run. It performs no I/O of its own beyond reading r; it does not open a
// connection, run a command, or touch a shell (AGENTS.md invariant 1
// governs the local side, and this function has no local shell side at
// all).
//
// # Input formats
//
// gnuFind selects which of the two line formats BuildScan's output clause
// produced, matching the gnuFind argument that chose the clause in the
// first place (DESIGN.md 8.2):
//
//   - true: GNU find's `-printf '%s\t%T@\t%p\n'`, three tab-separated
//     fields per line - size, mtime, path, exactly as documented on
//     BuildScan and on Entry.ModTime above.
//
//   - false: the BSD/macOS/busybox fallback, `ls -ldn -- {} +` output,
//     one entry per line in the long-listing format, for example:
//
//     -rw-r--r--  1 0  0  88213441 Aug 14 16:02 /var/log/app.log
//
// The BSD line's fields - mode, link count, numeric uid, numeric gid
// (`-n` is what makes them digits instead of names), size, and a
// three-field date - are separated by a variable run of spaces used for
// column alignment, not a single space. The path is everything after
// that, verbatim, and can itself contain spaces:
//
//	-rw-r--r--  1 0  0  88213441 Aug 14 16:02 /var/log/app name/app.log
//
// A parser must never call strings.Fields on a full ls line to get the
// path: Fields collapses whitespace runs uniformly across the whole line,
// so it corrupts exactly this case by splitting "app name/app.log" into
// two fields (AGENTS.md section 9). The fixed leading fields must be
// consumed left to right - strings.SplitN is the tool - so that whatever
// remains, including any internal spaces, becomes the path unmodified.
//
// # Streaming and the out channel
//
// Entries are sent on out as they are parsed, not batched until the end,
// so a caller reading out progressively (for example to populate a list
// as DESIGN.md 8.3 requires) sees them arrive incrementally. ParseScan
// returns the Report only once r is exhausted, the 10,000-entry cap is
// hit, or a read error occurs.
//
// Sending on out is a blocking channel operation. If out is unbuffered, or
// buffered but full, ParseScan blocks until something reads it. The
// caller is responsible for draining out concurrently with the call to
// ParseScan - in a goroutine, or via a channel buffered generously enough
// for the expected input - or the call will stall for as long as nothing
// reads.
//
// If out is non-nil, ParseScan closes it before returning, whether it
// returns a nil or a non-nil error, so a caller may range over it to
// detect completion instead of separately synchronising on ParseScan's
// return. Do not close out yourself; ParseScan owns it as its only
// sender, per normal Go channel-ownership rules.
//
// # Nil out
//
// A nil out is legal. ParseScan then parses r exactly as it otherwise
// would - counting, capping and reporting exactly the same - but does not
// attempt to send or close it (closing a nil channel panics, so ParseScan
// never does that). This is for callers that only need the Report, for
// example a capability probe or a test that only checks counts.
//
// # The 10,000-entry cap
//
// DESIGN.md 8.3 caps a scan at 10,000 rows. Once Report.Count would
// reach that cap, ParseScan stops parsing immediately, sets
// Report.Truncated, and returns without reading the remainder of r. It
// does not drain the rest of the input first. A capped-out reader is, in
// practice, the stdout pipe of a still-running remote command; draining
// the remainder would mean buffering an effectively unbounded amount of
// output the cap exists specifically to avoid keeping around, and the
// caller already has everything ParseScan can usefully tell it (the cap
// was hit) without that read. A caller whose reader is tied to a live
// process is expected to cancel that process's context once it sees
// Report.Truncated, the same as it would for any other reason to stop an
// in-flight scan early.
//
// # Malformed lines
//
// A line that does not fit its format - wrong field count, a
// non-numeric size or mtime, or similar - is not fatal. It is dropped and
// counted in Report.Skipped, because permission-denied noise and odd
// filenames on a real host are routine (DESIGN.md 8.3), and a scan must
// not abort over one bad line among thousands of good ones.
//
// # A truncated final line
//
// If r ends mid-record - the underlying stream was cut before a final
// newline, so the last line read is incomplete - that line is treated
// like any other malformed line: skipped and counted in Report.Skipped,
// never returned as an error. bufio.Scanner already surfaces a final
// unterminated line as one more token, so this needs no special
// detection, only the same non-fatal handling as any other bad line.
//
// # Errors
//
// ParseScan returns a non-nil error only when reading r itself fails -
// for example the underlying reader's error, or bufio.Scanner's
// ErrTooLong if a line exceeds the scan buffer. A malformed line, on its
// own, is never an error; see "Malformed lines" above. When ParseScan
// returns a non-nil error, Report is still populated with everything
// parsed before the failure.
func ParseScan(r io.Reader, gnuFind bool, out chan<- Entry) (Report, error) {
	var report Report

	if out != nil {
		defer close(out)
	}

	scanner := bufio.NewScanner(r)
	// The default 64KB token limit is too small for some real remote log
	// paths and fails with an opaque bufio.ErrTooLong (AGENTS.md section
	// 9). Raise it well past anything a single find/ls line should need.
	scanner.Buffer(make([]byte, 0, 64*1024), scanBufferMax)

	for {
		if report.Count >= maxScanEntries {
			// Stop before reading another line at all, per "The
			// 10,000-entry cap" above: the remainder of r is left
			// untouched.
			report.Truncated = true
			break
		}

		if !scanner.Scan() {
			break
		}

		line := scanner.Text()

		var entry Entry
		var ok bool
		if gnuFind {
			entry, ok = parseGNULine(line)
		} else {
			entry, ok = parseBSDLine(line)
		}

		if !ok {
			report.Skipped++
			continue
		}

		report.Count++
		if out != nil {
			out <- entry
		}
	}

	if err := scanner.Err(); err != nil {
		return report, err
	}

	return report, nil
}

// maxScanEntries is the row cap documented in DESIGN.md 8.3.
const maxScanEntries = 10_000

// scanBufferMax is the maximum single-line size ParseScan's bufio.Scanner
// will accept, well above the 64KB default that trips on long real-world
// log paths (AGENTS.md section 9).
const scanBufferMax = 1 << 20 // 1MB

// bsdFixedFields is the number of whitespace-separated fixed-width fields
// that precede the path in `ls -ldn` output: mode, link count, uid, gid,
// size, month, day, time-or-year.
const bsdFixedFields = 8

// bsdSizeField is the index, within the fields bsdSplitFields returns, of
// the file size column.
const bsdSizeField = 4

// parseGNULine parses one line of GNU find's `-printf '%s\t%T@\t%p\n'`
// output into an Entry. It reports false if the line does not have
// exactly three tab-separated fields, or if the size or mtime field is
// not parseable.
func parseGNULine(line string) (Entry, bool) {
	parts := strings.SplitN(line, "\t", 3)
	if len(parts) != 3 {
		return Entry{}, false
	}

	size, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Entry{}, false
	}

	modTime, ok := parseEpoch(parts[1])
	if !ok {
		return Entry{}, false
	}

	path := parts[2]
	if path == "" {
		return Entry{}, false
	}

	return Entry{Path: path, Size: size, ModTime: modTime}, true
}

// parseEpoch parses find's %T@ field: Unix epoch seconds with a
// fractional part, for example "1755180171.4400000000". The fraction is
// handled digit by digit rather than via float64 parsing, so it is exact
// regardless of how many fractional digits the remote find emits.
func parseEpoch(s string) (time.Time, bool) {
	secStr, fracStr, hasFrac := strings.Cut(s, ".")

	sec, err := strconv.ParseInt(secStr, 10, 64)
	if err != nil {
		return time.Time{}, false
	}

	if !hasFrac {
		return time.Unix(sec, 0), true
	}

	switch {
	case len(fracStr) > 9:
		fracStr = fracStr[:9]
	case len(fracStr) < 9:
		fracStr += strings.Repeat("0", 9-len(fracStr))
	}

	nsec, err := strconv.ParseInt(fracStr, 10, 64)
	if err != nil {
		return time.Time{}, false
	}

	return time.Unix(sec, nsec), true
}

// parseBSDLine parses one line of `ls -ldn` output into an Entry.
// ModTime is left zero; see Entry.ModTime for why. It reports false if
// the line does not have at least bsdFixedFields leading fields followed
// by a non-empty path, or if the size field is not parseable.
func parseBSDLine(line string) (Entry, bool) {
	fields, path, ok := bsdSplitFields(line, bsdFixedFields)
	if !ok || path == "" {
		return Entry{}, false
	}

	size, err := strconv.ParseInt(fields[bsdSizeField], 10, 64)
	if err != nil {
		return Entry{}, false
	}

	return Entry{Path: path, Size: size}, true
}

// bsdSplitFields consumes n whitespace-separated fields from the left of
// line, where fields may be separated by a variable run of spaces used
// for column alignment. It returns those n fields and the remainder of
// line, with only its leading separator run trimmed, so that any spaces
// inside the remainder (as in a path containing spaces) are preserved
// verbatim. It reports false if line does not contain n fields at all.
//
// This deliberately never calls strings.Fields on the whole line: Fields
// collapses whitespace uniformly across the entire input, which would
// corrupt a path containing spaces (AGENTS.md section 9).
func bsdSplitFields(line string, n int) (fields []string, rest string, ok bool) {
	fields = make([]string, 0, n)
	rest = line
	for i := 0; i < n; i++ {
		rest = strings.TrimLeft(rest, " ")
		idx := strings.IndexByte(rest, ' ')
		if idx == -1 {
			return nil, "", false
		}
		fields = append(fields, rest[:idx])
		rest = rest[idx:]
	}
	rest = strings.TrimLeft(rest, " ")
	return fields, rest, true
}
