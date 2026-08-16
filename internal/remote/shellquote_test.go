package remote

// Test plan for T09 (AGENTS.md sections 5 and 8, T09). This file covers
// mechanic 1 against Quote, plus the fuzz target. discover_test.go covers
// mechanics 2 through 5, plus one extra mechanic this file's sibling adds
// for the zero-paths and MaxDepth-zero edge cases (see its own comment
// block for why).
//
//  1. Quote wraps in single quotes and escapes embedded single quotes.
//
// Plus one fuzz target, not a table: FuzzQuote asserts that quoting an
// arbitrary string and echoing it through `sh -c` round-trips to the
// original string. Seeded with the awkward cases: embedded quote, newline,
// backslash, $VAR, backtick, empty string. Inputs containing a NUL byte are
// skipped, because no shell argument can carry one.

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

func TestQuote(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty string",
			in:   "",
			want: `''`,
		},
		{
			name: "no special characters",
			in:   "abc",
			want: `'abc'`,
		},
		{
			name: "string containing a space is wrapped, not split",
			in:   "a b",
			want: `'a b'`,
		},
		{
			name: "one embedded single quote",
			in:   "it's",
			want: `'it'\''s'`,
		},
		{
			name: "the whole string is a single quote character",
			in:   "'",
			want: `''\'''`,
		},
		{
			name: "two adjacent embedded single quotes",
			in:   "''",
			want: `''\'''\'''`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Quote(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Quote(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// FuzzQuote asserts the one property that matters for Quote: whatever it
// produces, a POSIX shell reads back as the original string when used as a
// single argument. It spawns sh -c per input, which AGENTS.md section 5
// otherwise disallows in a unit test; this is the same exception T04 makes
// for process handling, because here the shell's own parser is the only
// honest judge of whether the quoting is correct.
func FuzzQuote(f *testing.F) {
	seeds := []string{
		"",
		"plain",
		"it's",
		"'",
		"''",
		"a\nb",
		"a\\b",
		"$VAR",
		"`echo hi`",
		"$(echo hi)",
		"a b\tc",
	}
	for _, s := range seeds {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		if strings.ContainsRune(s, 0) {
			t.Skip("no shell argument can carry a NUL byte")
		}

		quoted := Quote(s)

		cmd := exec.Command("sh", "-c", "printf %s "+quoted)
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("sh -c failed on Quote(%q) = %s: %v", s, quoted, err)
		}

		if got := string(out); got != s {
			t.Errorf("round trip mismatch: Quote(%q) = %s, sh -c printf gave %q", s, quoted, got)
		}
	})
}
