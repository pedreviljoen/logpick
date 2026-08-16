package remote

import "strings"

// Quote wraps s in single quotes so it survives as exactly one argument to
// a POSIX shell (sh -c), which is how every remote command in this package
// is dispatched (AGENTS.md invariant 1: never build a shell string and
// pass it to a local sh -c, but the remote side is exactly that, a shell
// string handed to the transport template's {cmd} placeholder).
//
// Inside single quotes a POSIX shell treats every character literally
// except the single quote itself, which cannot appear inside a
// single-quoted string at all. The standard escape is to close the quoted
// string, emit a backslash-escaped literal quote, and reopen it. That is
// the entire algorithm; no other character needs any special handling, and
// no shell-quoting library is used (DESIGN.md section 5.1). For example:
//
//	Quote(`it's`)  ->  'it'\''s'
//	Quote(``)      ->  ''
func Quote(s string) string {
	var b strings.Builder
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			b.WriteString(`'\''`)
			continue
		}
		b.WriteByte(s[i])
	}
	b.WriteByte('\'')
	return b.String()
}
