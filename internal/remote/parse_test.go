package remote

// Test plan for T10 (AGENTS.md sections 5 and 8, T10).
//
//  1. GNU tab-separated output parses into the expected entries.
//  2. `ls -ldn` output parses, including a path containing spaces.
//  3. Malformed lines are skipped and counted, not fatal.
//  4. A truncated final line is skipped without error.
//  5. Output exceeding 10,000 entries stops at the cap and sets the
//     truncated flag.

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// drainEntries calls ParseScan against a buffered channel sized for the
// small fixtures this file uses, so the call never blocks and no goroutine
// or sleep is needed. ParseScan is documented to close a non-nil out
// before returning, so ranging over it after the call returns is safe and
// terminates.
func drainEntries(t *testing.T, r *strings.Reader, gnuFind bool) ([]Entry, Report, error) {
	t.Helper()

	out := make(chan Entry, 32)
	report, err := ParseScan(r, gnuFind, out)

	var entries []Entry
	for e := range out {
		entries = append(entries, e)
	}
	return entries, report, err
}

func TestParseScan(t *testing.T) {
	timeCmp := cmp.Comparer(func(a, b time.Time) bool { return a.Equal(b) })

	t.Run("GNU tab-separated output parses into the expected entries", func(t *testing.T) {
		input := "88213441\t1755180171.5\t/var/log/app.log\n" +
			"1024\t1700000000.0\t/opt/app/logs/gc.log\n"

		entries, report, err := drainEntries(t, strings.NewReader(input), true)
		if err != nil {
			t.Fatalf("ParseScan returned error: %v", err)
		}

		want := []Entry{
			{Path: "/var/log/app.log", Size: 88213441, ModTime: time.Unix(1755180171, 500000000)},
			{Path: "/opt/app/logs/gc.log", Size: 1024, ModTime: time.Unix(1700000000, 0)},
		}
		if diff := cmp.Diff(want, entries, timeCmp); diff != "" {
			t.Errorf("entries mismatch (-want +got):\n%s", diff)
		}

		wantReport := Report{Count: 2, Skipped: 0, Truncated: false}
		if diff := cmp.Diff(wantReport, report); diff != "" {
			t.Errorf("report mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("ls -ldn output parses, including a path containing spaces", func(t *testing.T) {
		input := "-rw-r--r--  1 0  0  88213441 Aug 14 16:02 /var/log/app name/app.log\n" +
			"-rw-r--r--  1 501  20  1024 Jan  5 09:33 /opt/app/logs/gc.log\n"

		entries, report, err := drainEntries(t, strings.NewReader(input), false)
		if err != nil {
			t.Fatalf("ParseScan returned error: %v", err)
		}

		// BSD ls has no year in its date column, so ParseScan documents
		// leaving ModTime zero rather than guessing one (parse.go,
		// Entry.ModTime).
		want := []Entry{
			{Path: "/var/log/app name/app.log", Size: 88213441, ModTime: time.Time{}},
			{Path: "/opt/app/logs/gc.log", Size: 1024, ModTime: time.Time{}},
		}
		if diff := cmp.Diff(want, entries, timeCmp); diff != "" {
			t.Errorf("entries mismatch (-want +got):\n%s", diff)
		}

		wantReport := Report{Count: 2, Skipped: 0, Truncated: false}
		if diff := cmp.Diff(wantReport, report); diff != "" {
			t.Errorf("report mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("malformed lines are skipped and counted, not fatal", func(t *testing.T) {
		input := "1024\t1700000000.0\t/var/log/good.log\n" +
			"garbage line with no tab separators at all\n" +
			"abc\t1700000000.0\t/var/log/bad-size.log\n" +
			"2048\t1700000001.0\t/var/log/good2.log\n"

		entries, report, err := drainEntries(t, strings.NewReader(input), true)
		if err != nil {
			t.Fatalf("ParseScan returned error: %v", err)
		}

		want := []Entry{
			{Path: "/var/log/good.log", Size: 1024, ModTime: time.Unix(1700000000, 0)},
			{Path: "/var/log/good2.log", Size: 2048, ModTime: time.Unix(1700000001, 0)},
		}
		if diff := cmp.Diff(want, entries, timeCmp); diff != "" {
			t.Errorf("entries mismatch (-want +got):\n%s", diff)
		}

		wantReport := Report{Count: 2, Skipped: 2, Truncated: false}
		if diff := cmp.Diff(wantReport, report); diff != "" {
			t.Errorf("report mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("a truncated final line is skipped without error", func(t *testing.T) {
		// The final record is cut mid-field and has no trailing newline,
		// as if the underlying stream ended abruptly.
		input := "1024\t1700000000.0\t/var/log/good.log\n" +
			"2048\t1700000001.0\t/var/log/good2.log\n" +
			"512\t170000000"

		entries, report, err := drainEntries(t, strings.NewReader(input), true)
		if err != nil {
			t.Fatalf("ParseScan returned error: %v", err)
		}

		want := []Entry{
			{Path: "/var/log/good.log", Size: 1024, ModTime: time.Unix(1700000000, 0)},
			{Path: "/var/log/good2.log", Size: 2048, ModTime: time.Unix(1700000001, 0)},
		}
		if diff := cmp.Diff(want, entries, timeCmp); diff != "" {
			t.Errorf("entries mismatch (-want +got):\n%s", diff)
		}

		wantReport := Report{Count: 2, Skipped: 1, Truncated: false}
		if diff := cmp.Diff(wantReport, report); diff != "" {
			t.Errorf("report mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("output exceeding 10,000 entries stops at the cap and sets the truncated flag", func(t *testing.T) {
		const lineCount = 10_005

		var b strings.Builder
		for i := 0; i < lineCount; i++ {
			b.WriteString(strconv.Itoa(i))
			b.WriteByte('\t')
			b.WriteString("1700000000.0")
			b.WriteByte('\t')
			fmt.Fprintf(&b, "/var/log/app-%d.log\n", i)
		}

		// nil is a documented-legal out (parse.go, "Nil out"): this
		// mechanic only needs the Report, and generating 10,000 Entry
		// values just to discard them buys nothing.
		report, err := ParseScan(strings.NewReader(b.String()), true, nil)
		if err != nil {
			t.Fatalf("ParseScan returned error: %v", err)
		}

		wantReport := Report{Count: 10_000, Skipped: 0, Truncated: true}
		if diff := cmp.Diff(wantReport, report); diff != "" {
			t.Errorf("report mismatch (-want +got):\n%s", diff)
		}
	})
}
