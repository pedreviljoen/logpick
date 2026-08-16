package local

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// Query describes an in-file search request.
type Query struct {
	// Pattern is the text to search for.
	Pattern string
	// Regex, when true, treats Pattern as a regular expression using
	// Go's RE2 syntax instead of a literal substring. Literal is the
	// default (DESIGN.md 10.3).
	Regex bool
}

// Match is one hit from an in-file search.
//
// Field names and types are chosen to match ui.SearchMatch
// (internal/ui/msg.go) exactly, so that ui.SearchMatch can become a type
// alias for this type once T17 lands, per that file's doc comment. Do not
// rename or retype a field here without updating ui.SearchMatch to match.
type Match struct {
	// Line is the one-based line number of the hit.
	Line int
	// Start and End are the byte offsets of the hit within the line,
	// [Start, End), which the viewer uses to highlight the match.
	Start int
	End   int
	// Text is the full text of the matching line.
	Text string
}

// ErrInvalidPattern is the sentinel wrapped into the error a Searcher
// returns when Query.Regex is true and Query.Pattern fails to compile as
// a regular expression. Callers match it with errors.Is; the returned
// error's message also names the offending pattern, so it is readable on
// its own rather than surfacing regexp's raw panic-shaped message.
var ErrInvalidPattern = errors.New("invalid search pattern")

// Searcher searches one local file for a Query and returns every match,
// in file order.
//
// Go's regexp package compiles Query.Pattern with RE2 semantics, which
// runs in time linear in the size of the input regardless of the pattern.
// A user pasting an adversarial pattern, for example a stack trace used
// as a search term, cannot trigger the catastrophic backtracking a
// backtracking engine could (DESIGN.md 10.3). This is a deliberate
// property of using the standard library's regexp package and must not be
// replaced with a backtracking engine.
type Searcher interface {
	// Search reads path and returns every line matching q. ctx governs
	// cancellation of the read and, for Ripgrep, the subprocess.
	Search(ctx context.Context, path string, q Query) ([]Match, error)
}

// scannerBufSize is the enlarged bufio.Scanner buffer used everywhere a
// scanner reads log content, per AGENTS.md section 9: the default 64KB
// token limit fails opaquely on minified JSON log lines.
const scannerBufSize = 1 << 20 // 1MB

// Ripgrep is a Searcher backed by shelling out to the rg binary on PATH.
// It runs `rg --json` and parses the streamed events (DESIGN.md 10.3).
type Ripgrep struct{}

// NewRipgrep returns a Searcher backed by the rg binary.
//
// NewRipgrep does not itself check whether rg is present on PATH; Choose
// is responsible for that decision. Calling Search when rg is absent
// surfaces the resulting exec error.
func NewRipgrep() *Ripgrep {
	return &Ripgrep{}
}

// rgMessage is the subset of ripgrep's --json event schema this package
// needs. Only "match" events are consumed; other event types (begin, end,
// summary, context) are skipped.
type rgMessage struct {
	Type string `json:"type"`
	Data struct {
		Lines struct {
			Text string `json:"text"`
		} `json:"lines"`
		LineNumber int `json:"line_number"`
		Submatches []struct {
			Start int `json:"start"`
			End   int `json:"end"`
		} `json:"submatches"`
	} `json:"data"`
}

// Search implements Searcher.
func (r *Ripgrep) Search(ctx context.Context, path string, q Query) ([]Match, error) {
	args := []string{"--json", "--no-config"}
	if !q.Regex {
		args = append(args, "--fixed-strings")
	}
	args = append(args, "--", q.Pattern, path)

	cmd := exec.CommandContext(ctx, "rg", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			// rg exits 1 when the pattern compiled fine but found no
			// matches. That is not an error condition. Native returns a
			// nil slice in the same situation, so match this rather than
			// an empty non-nil slice, to keep the two implementations in
			// agreement.
			return nil, nil
		}
		if q.Regex && isRgPatternError(stderr.String()) {
			return nil, fmt.Errorf("%w: %q: %s", ErrInvalidPattern, q.Pattern, strings.TrimSpace(stderr.String()))
		}
		return nil, fmt.Errorf("rg: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	var matches []Match
	scanner := bufio.NewScanner(&stdout)
	scanner.Buffer(make([]byte, 0, scannerBufSize), scannerBufSize)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var msg rgMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			return nil, fmt.Errorf("rg: parsing json output: %w", err)
		}
		if msg.Type != "match" {
			continue
		}
		text := strings.TrimRight(msg.Data.Lines.Text, "\r\n")
		for _, sm := range msg.Data.Submatches {
			matches = append(matches, Match{
				Line:  msg.Data.LineNumber,
				Start: sm.Start,
				End:   sm.End,
				Text:  text,
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("rg: reading json output: %w", err)
	}

	return matches, nil
}

// isRgPatternError reports whether rg's stderr indicates the pattern
// failed to compile as a regular expression, as opposed to some other
// failure (missing file, permission error, and so on).
func isRgPatternError(stderr string) bool {
	lower := strings.ToLower(stderr)
	return strings.Contains(lower, "regex parse error") ||
		strings.Contains(lower, "unclosed") ||
		strings.Contains(lower, "invalid") && strings.Contains(lower, "regex")
}

// Native is a Searcher with no external dependency: a bufio.Scanner with
// an enlarged buffer, strings.Contains for literal queries and regexp for
// pattern queries (DESIGN.md 10.3).
type Native struct{}

// NewNative returns a Searcher that never shells out.
func NewNative() *Native {
	return &Native{}
}

// Search implements Searcher.
func (n *Native) Search(ctx context.Context, path string, q Query) ([]Match, error) {
	var re *regexp.Regexp
	if q.Regex {
		compiled, err := regexp.Compile(q.Pattern)
		if err != nil {
			return nil, fmt.Errorf("%w: %q: %v", ErrInvalidPattern, q.Pattern, err)
		}
		re = compiled
	}

	f, err := os.Open(path) //nolint:gosec // path is the caller-chosen local file to search, not untrusted input.
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	var matches []Match
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, scannerBufSize), scannerBufSize)

	line := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		line++
		text := scanner.Text()

		if q.Regex {
			for _, loc := range re.FindAllStringIndex(text, -1) {
				matches = append(matches, Match{
					Line:  line,
					Start: loc[0],
					End:   loc[1],
					Text:  text,
				})
			}
			continue
		}

		if q.Pattern == "" {
			continue
		}
		offset := 0
		for {
			idx := strings.Index(text[offset:], q.Pattern)
			if idx < 0 {
				break
			}
			start := offset + idx
			end := start + len(q.Pattern)
			matches = append(matches, Match{
				Line:  line,
				Start: start,
				End:   end,
				Text:  text,
			})
			offset = end
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning %s: %w", path, err)
	}

	return matches, nil
}

// Choose selects a Searcher implementation at runtime: Ripgrep if rg is on
// PATH, Native otherwise.
//
// override forces the choice regardless of what is on PATH: "rg" selects
// Ripgrep and "native" selects Native. Any other value, including the
// empty string, falls through to the PATH probe. The override is checked
// before the PATH probe, so it wins even when rg is present.
//
// Choose returns the Searcher interface rather than a concrete type. This
// is the one sanctioned exception to "constructors return concrete types"
// (AGENTS.md section 4.1, deviation 2): Choose genuinely selects an
// implementation at runtime, unlike NewRipgrep and NewNative, which each
// return their own concrete type.
func Choose(override string) Searcher {
	switch override {
	case "rg":
		return NewRipgrep()
	case "native":
		return NewNative()
	}

	if _, err := exec.LookPath("rg"); err == nil {
		return NewRipgrep()
	}
	return NewNative()
}
