package transport

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ErrBadTemplate is the sentinel for every way a template can fail to
// substitute: an unknown placeholder or an empty template. Callers match it
// with errors.Is; the error message additionally names the offending
// placeholder and, for an unknown placeholder, the element index within
// tmpl, since AGENTS.md section 5 requires those identifying details to be
// asserted with strings.Contains rather than baked into a full string
// comparison.
var ErrBadTemplate = errors.New("bad template")

// placeholderPattern matches {name} placeholders in a template element. It
// is deliberately more permissive than internal/config/validate.go's
// \{([a-zA-Z]+)\} so that anything a looser config-time check lets through
// (e.g. "{my_var}") is still caught here before it reaches an argv.
var placeholderPattern = regexp.MustCompile(`\{([a-zA-Z0-9_-]+)\}`)

// Substitute replaces {placeholder} occurrences in tmpl with values from
// vals, returning a new argv slice. tmpl is never itself a shell string: it
// is the []string form an argv is built from (AGENTS.md invariant 1), and
// Substitute preserves that shape rather than joining and re-splitting.
//
// Substitution happens within an element, so multiple placeholders in one
// element are all replaced, e.g. "{host}:{remote}". A placeholder that
// repeats, within an element or across several, is replaced at every
// occurrence. An element with no placeholders passes through unchanged.
//
// The known placeholder set is {host}, {user}, {port}, {cmd}, {remote} and
// {local} (DESIGN.md 7.2), but Substitute does not special-case that set: any
// placeholder syntactically present in tmpl must have a matching key in
// vals, known or not. A placeholder that is in the known set but missing
// from vals is still an error, not a silent empty-string substitution,
// because a config template with a placeholder nobody filled in is a caller
// bug, and inserting "" into an argv is how that bug turns into a command
// that runs and does the wrong thing without complaint. An unknown
// placeholder is rejected with an error naming the placeholder and the
// index, within tmpl, of the element it appeared in, wrapping
// ErrBadTemplate. An empty tmpl is also rejected, wrapping ErrBadTemplate.
//
// Substitution is not recursive. A value in vals that itself contains
// placeholder syntax, e.g. vals["cmd"] == "{host}", is inserted literally
// and the result is never re-scanned for further placeholders. Only tmpl is
// scanned for placeholder syntax; vals is scanned for nothing.
//
// The placeholder pattern accepted here is deliberately more permissive than
// the one internal/config/validate.go uses to reject unknown placeholders at
// config load time, so that anything a looser config-time check lets through
// is still caught here before it reaches an argv.
func Substitute(tmpl []string, vals map[string]string) ([]string, error) {
	if len(tmpl) == 0 {
		return nil, fmt.Errorf("empty template: %w", ErrBadTemplate)
	}

	result := make([]string, len(tmpl))
	for i, elem := range tmpl {
		matches := placeholderPattern.FindAllStringSubmatchIndex(elem, -1)
		if matches == nil {
			result[i] = elem
			continue
		}

		var b strings.Builder
		b.Grow(len(elem))
		last := 0
		for _, m := range matches {
			start, end := m[0], m[1]
			nameStart, nameEnd := m[2], m[3]
			name := elem[nameStart:nameEnd]

			val, ok := vals[name]
			if !ok {
				return nil, fmt.Errorf("unknown placeholder %q in element %d: %w", name, i, ErrBadTemplate)
			}

			b.WriteString(elem[last:start])
			b.WriteString(val)
			last = end
		}
		b.WriteString(elem[last:])
		result[i] = b.String()
	}

	return result, nil
}
