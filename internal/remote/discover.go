package remote

import (
	"strconv"
	"strings"

	"github.com/pedreviljoen/logpick/internal/config"
)

// BuildScan builds the single-round-trip `find` command used to discover
// candidate log files on a host in one shot, rather than one round trip per
// path (DESIGN.md section 8.1). The returned string is shell syntax meant
// for the transport's {cmd} placeholder (see Transport in
// internal/transport); BuildScan itself does no I/O and never touches a
// shell locally (AGENTS.md invariant 1 governs the local side only).
//
// # Output clause
//
// gnuFind selects the trailing clause that produces the listing:
//
//   - true emits GNU find's `-printf '%s\t%T@\t%p\n'`: tab-separated size in
//     bytes, mtime as Unix epoch seconds with a fractional part, and path.
//   - false emits the BSD/macOS/busybox-portable fallback
//     `-exec ls -ldn -- {} +`, whose output T10 parses instead.
//
// Callers choose gnuFind by probing the host once per connection with
// `find --version >/dev/null 2>&1 && echo gnu || echo other` and caching
// the result in state.toml (DESIGN.md section 8.2). BuildScan does not
// probe; it only renders the clause for the answer it is given.
//
// # Paths
//
// Each entry in spec.Paths is emitted space-separated right after `find`.
// A path containing a glob metacharacter - '*', '?' or '[' - is emitted
// unquoted, so the remote shell expands it before find ever sees it (a
// path like /srv/*/logs only means something if the shell gets to glob
// it). A path containing none of those three characters is emitted through
// Quote, so spaces and shell-special punctuation in a literal path survive
// intact instead of being split or reinterpreted. A literal path that
// happens to need a literal '*', '?' or '[' byte cannot be expressed by
// this scheme; that is a known, accepted limitation (DESIGN.md 8.3), not a
// bug to fix here.
//
// spec.Paths empty (nil, or explicitly set to [] by --path with no values
// or paths = [] in config - both reachable per T07's resolution) gives
// find no starting point. Scanning from "/" or emitting a bare `find` is
// exactly the silent-whole-filesystem failure DESIGN.md warns against, so
// BuildScan returns the empty string instead. Callers must treat an empty
// return as "nothing to scan" and skip calling Transport.Exec entirely,
// never pass "" through to a shell as if it were a valid command.
//
// # Depth
//
// spec.MaxDepth of 0 means "not set", per config.Resolve's documented
// merge rule (see mergeScan in internal/config/resolve.go): depth zero is
// not independently expressible, because a depth-zero find can never reach
// a file inside a directory and so has no useful meaning for this tool.
// BuildScan mirrors that: MaxDepth <= 0 omits `-maxdepth` entirely, so find
// recurses without a depth limit. It never emits `-maxdepth 0`, which would
// find nothing. A positive MaxDepth is emitted as `-maxdepth N` directly
// after the path list and before `-type f`, because both GNU and BSD find
// require -maxdepth to precede other expressions or reject the command.
//
// # Include and exclude
//
// spec.Include becomes one OR group right after `-type f`:
//
//	\( -name 'A' -o -name 'B' \)
//
// A single-pattern Include still gets wrapped in the group, degenerating
// to `\( -name 'A' \)` with no `-o`, so the shape is uniform regardless of
// how many patterns there are - callers matching against the command shape
// (T05) do not special-case a count of one.
//
// spec.Exclude becomes one `! -name 'X'` clause per pattern, appended
// after the include group. An empty Include list omits the group
// entirely, and an empty Exclude list emits no clause; either emitted
// empty, `\( \)` or a bare `!`, is a syntax error on the remote. Every
// pattern in both lists is passed through Quote regardless of whether it
// contains a glob character, because -name's argument is interpreted by
// find itself, not by the shell that dispatches this command line, so
// there is no unquoted path here.
//
// # Trailing clause
//
// The command always ends with `2>/dev/null`, after the output clause.
// Permission-denied noise under directories like /var/log is routine and
// must never surface as a scan error (DESIGN.md 8.3). No other redirection
// is ever emitted: this command is read-only (AGENTS.md invariant 2).
//
// # Shape, as a cross-task contract
//
// T05's mock backend recognises commands by shape and T10 parses stdout by
// the format named above, so the emitted shape is deliberately fixed and
// simple: `find <paths...> [-maxdepth N] -type f [include group]
// [exclude clauses] <output clause> 2>/dev/null`, all on one line, fields
// separated by single spaces.
func BuildScan(spec config.ScanSpec, gnuFind bool) string {
	if len(spec.Paths) == 0 {
		return ""
	}

	var b strings.Builder
	first := true
	field := func(s string) {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		b.WriteString(s)
	}

	field("find")
	writePaths(field, spec.Paths)

	if spec.MaxDepth > 0 {
		field("-maxdepth")
		field(strconv.Itoa(spec.MaxDepth))
	}

	field("-type")
	field("f")

	if len(spec.Include) > 0 {
		field(`\(`)
		for i, pat := range spec.Include {
			if i > 0 {
				field("-o")
			}
			field("-name")
			field(Quote(pat))
		}
		field(`\)`)
	}

	for _, pat := range spec.Exclude {
		field("!")
		field("-name")
		field(Quote(pat))
	}

	if gnuFind {
		field("-printf")
		field(`'%s\t%T@\t%p\n'`)
	} else {
		field("-exec")
		field("ls")
		field("-ldn")
		field("--")
		field("{}")
		field("+")
	}

	field("2>/dev/null")

	return b.String()
}

// writePaths emits spec.Paths as the root arguments of a find command,
// using the quoting rule BuildScan documents under "Paths": a path
// containing a glob metacharacter goes out unquoted so the remote shell
// expands it, anything else goes through Quote so spaces and punctuation
// survive. BuildScan and BuildBrowse share it so the two can never drift
// into quoting the same path differently.
func writePaths(field func(string), paths []string) {
	for _, p := range paths {
		if strings.ContainsAny(p, "*?[") {
			field(p)
		} else {
			field(Quote(p))
		}
	}
}

// BuildBrowse builds the `find` command behind the browser's one-off path
// scan: the listing shown when discovery did not turn up the log the user
// was after and they name a path themselves (DESIGN.md 8.3, "when the
// configured paths are wrong").
//
// It differs from BuildScan in exactly two ways, both deliberate:
//
//   - No `-type f`, so directories are listed alongside regular files. A
//     user who names a path they are unsure about wants to see what is
//     actually there, including the subdirectory the logs really live in,
//     and the browser lets them descend into one.
//   - No include or exclude clause. spec.Include and spec.Exclude are
//     ignored entirely rather than applied, because the profile's log
//     patterns are the very thing that hid the file: re-applying them to
//     a hand-typed path would reproduce the empty list the user is trying
//     to escape.
//
// spec.Paths and spec.MaxDepth are honoured exactly as BuildScan honours
// them, including the two contracts documented there: empty Paths returns
// "" and the caller must skip the exec rather than run a bare find, and
// MaxDepth <= 0 means unlimited depth rather than `-maxdepth 0`.
//
// # Output clause
//
// gnuFind selects the same two dialects BuildScan selects between, with
// the directory mark Entry.IsDir documents:
//
//	\( -type d -printf '%s\t%T@\t%p/\n' -o -printf '%s\t%T@\t%p\n' \)
//
// The group is one find expression: a directory matches `-type d` and is
// printed with a trailing slash, anything else falls through the `-o` to
// the plain form. The fields either branch prints are byte for byte
// BuildScan's, so ParseScan needs no separate browse format.
//
// The BSD fallback is `-exec ls -ldn -- {} +` unchanged. `-d` already
// makes ls describe a directory itself rather than list its contents, and
// its mode column starts with 'd', which is the mark ParseScan reads
// there.
//
// As in BuildScan the command always ends `2>/dev/null`: browsing an
// unreadable tree is routine and must not surface as an error.
func BuildBrowse(spec config.ScanSpec, gnuFind bool) string {
	if len(spec.Paths) == 0 {
		return ""
	}

	var b strings.Builder
	first := true
	field := func(s string) {
		if !first {
			b.WriteByte(' ')
		}
		first = false
		b.WriteString(s)
	}

	field("find")
	writePaths(field, spec.Paths)

	if spec.MaxDepth > 0 {
		field("-maxdepth")
		field(strconv.Itoa(spec.MaxDepth))
	}

	if gnuFind {
		field(`\(`)
		field("-type")
		field("d")
		field("-printf")
		field(`'%s\t%T@\t%p/\n'`)
		field("-o")
		field("-printf")
		field(`'%s\t%T@\t%p\n'`)
		field(`\)`)
	} else {
		field("-exec")
		field("ls")
		field("-ldn")
		field("--")
		field("{}")
		field("+")
	}

	field("2>/dev/null")

	return b.String()
}
