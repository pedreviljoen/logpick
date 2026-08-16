package local

// Test plan for T17 (AGENTS.md sections 5 and 9, T17).
//
//  1. A literal query returns the expected matches with correct line
//     numbers.
//  2. A regex query returns the expected matches.
//  3. An invalid regex returns a readable error, not a regexp panic
//     message.
//  4. Both implementations return identical results for the same query on
//     the same fixture. One test, run against both via a shared table.
//  5. Choose honours the config override even when rg is present.

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// rgAvailable reports whether the rg binary can be found on PATH. Ripgrep
// cases must be skipped rather than failed when it is absent, per
// AGENTS.md section 13: this suite runs on machines that may not have rg
// installed, and CI is Linux and macOS only.
func rgAvailable(t *testing.T) bool {
	t.Helper()
	_, err := exec.LookPath("rg")
	return err == nil
}

func TestSearch(t *testing.T) {
	t.Run("a literal query returns the expected matches with correct line numbers", func(t *testing.T) {
		s := NewNative()

		got, err := s.Search(context.Background(), "testdata/literal.txt", Query{Pattern: "alpha"})
		if err != nil {
			t.Fatalf("Search returned error: %v", err)
		}

		// literal.txt line 2 contains "alpha" twice, so its two matches
		// have distinct, unambiguous offsets.
		want := []Match{
			{Line: 2, Start: 0, End: 5, Text: "alpha here alpha again"},
			{Line: 2, Start: 11, End: 16, Text: "alpha here alpha again"},
			{Line: 3, Start: 15, End: 20, Text: "last line only alpha once"},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("matches mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a regex query returns the expected matches", func(t *testing.T) {
		s := NewNative()

		got, err := s.Search(context.Background(), "testdata/regex.txt", Query{Pattern: `err\d+`, Regex: true})
		if err != nil {
			t.Fatalf("Search returned error: %v", err)
		}

		// regex.txt line 3 contains two err\d+ matches, so their offsets
		// are unambiguous.
		want := []Match{
			{Line: 2, Start: 7, End: 13, Text: "error: err404 not found"},
			{Line: 3, Start: 12, End: 17, Text: "warn: retry err12 then err404 again"},
			{Line: 3, Start: 23, End: 29, Text: "warn: retry err12 then err404 again"},
		}
		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("matches mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("an invalid regex returns a readable error, not a regexp panic message", func(t *testing.T) {
		s := NewNative()

		_, err := s.Search(context.Background(), "testdata/regex.txt", Query{Pattern: "[unclosed(", Regex: true})
		if err == nil {
			t.Fatal("Search returned no error for an invalid regex")
		}
		if !errors.Is(err, ErrInvalidPattern) {
			t.Errorf("error does not wrap ErrInvalidPattern: %v", err)
		}
		if !strings.Contains(err.Error(), "[unclosed(") {
			t.Errorf("error does not name the offending pattern: %v", err)
		}
	})

	t.Run("both implementations return identical results for the same query on the same fixture", func(t *testing.T) {
		cases := []struct {
			name  string
			path  string
			query Query
			want  []Match
		}{
			{
				name: "literal query",
				path: "testdata/shared.txt",
				query: Query{
					Pattern: "needle",
				},
				// shared.txt line 2 contains "needle" twice, so its two
				// matches have distinct, unambiguous offsets.
				want: []Match{
					{Line: 2, Start: 0, End: 6, Text: "needle in a haystack, needle again"},
					{Line: 2, Start: 22, End: 28, Text: "needle in a haystack, needle again"},
					{Line: 3, Start: 4, End: 10, Text: "one needle only on this line"},
				},
			},
			{
				name: "regex query",
				path: "testdata/regex.txt",
				query: Query{
					Pattern: `err\d+`,
					Regex:   true,
				},
				want: []Match{
					{Line: 2, Start: 7, End: 13, Text: "error: err404 not found"},
					{Line: 3, Start: 12, End: 17, Text: "warn: retry err12 then err404 again"},
					{Line: 3, Start: 23, End: 29, Text: "warn: retry err12 then err404 again"},
				},
			},
		}

		impls := []struct {
			name     string
			searcher Searcher
		}{
			{"native", NewNative()},
			{"ripgrep", NewRipgrep()},
		}

		for _, impl := range impls {
			t.Run(impl.name, func(t *testing.T) {
				if impl.name == "ripgrep" && !rgAvailable(t) {
					t.Skip("rg not found on PATH")
				}

				for _, tc := range cases {
					t.Run(tc.name, func(t *testing.T) {
						got, err := impl.searcher.Search(context.Background(), tc.path, tc.query)
						if err != nil {
							t.Fatalf("Search returned error: %v", err)
						}
						if diff := cmp.Diff(tc.want, got); diff != "" {
							t.Errorf("matches mismatch (-want +got):\n%s", diff)
						}
					})
				}
			})
		}
	})

	t.Run("Choose honours the config override even when rg is present", func(t *testing.T) {
		// This must hold regardless of whether rg is actually on PATH on
		// the machine running the suite, so neither case needs t.Skip:
		// the override is checked before the PATH probe either way.
		t.Run("override rg selects Ripgrep", func(t *testing.T) {
			got := Choose("rg")
			if _, ok := got.(*Ripgrep); !ok {
				t.Errorf(`Choose("rg") = %T, want *Ripgrep`, got)
			}
		})

		t.Run("override native selects Native", func(t *testing.T) {
			got := Choose("native")
			if _, ok := got.(*Native); !ok {
				t.Errorf(`Choose("native") = %T, want *Native`, got)
			}
		})
	})
}
