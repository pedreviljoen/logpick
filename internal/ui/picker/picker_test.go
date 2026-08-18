package picker_test

// Test plan (AGENTS.md section 5, copied verbatim from T13's contract before
// any test below was written):
//
//  1. Typing filters the list and the match set reflects the query.
//  2. ctrl+n and ctrl+p move the cursor and stop at the bounds.
//  3. The scroll offset follows the cursor past the viewport edge.
//  4. enter on a match reports it via Selected, enter on an empty match set
//     reports nothing.
//  5. Match highlight indices come back from fuzzy.Find unmodified.
//
// Every test below asserts on the model returned by Update (Matches, Cursor,
// Offset, Selected), never on View's rendered output (DESIGN.md section 13).

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/google/go-cmp/cmp"
	"github.com/sahilm/fuzzy"

	"github.com/pedreviljoen/logpick/internal/ui/picker"
)

// testItem is the small concrete Item used by every test in this file.
type testItem struct {
	name string
}

func (i testItem) FilterValue() string { return i.name }

// newTestItems builds a []testItem, one per name, in the given order.
func newTestItems(names ...string) []testItem {
	items := make([]testItem, len(names))
	for i, n := range names {
		items[i] = testItem{name: n}
	}
	return items
}

// itemValues returns the FilterValue of every item, in order, which is what
// fuzzy.Find expects as its data slice.
func itemValues(items []testItem) []string {
	values := make([]string, len(items))
	for i, it := range items {
		values[i] = it.FilterValue()
	}
	return values
}

// matchStrings extracts the Str field of every match, in order.
func matchStrings(matches []fuzzy.Match) []string {
	strs := make([]string, len(matches))
	for i, m := range matches {
		strs[i] = m.Str
	}
	return strs
}

// typeString drives m through Update once per rune of s, the same way real
// key presses arrive: one tea.KeyMsg of type KeyRunes per character.
func typeString(t *testing.T, m picker.Model[testItem], s string) picker.Model[testItem] {
	t.Helper()
	for _, r := range s {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	return m
}

// pressKeys drives m through Update once per key type given, in order, the
// same way ctrl+n, ctrl+p and enter arrive as bare tea.KeyMsg values with no
// Runes.
func pressKeys(t *testing.T, m picker.Model[testItem], keys ...tea.KeyType) picker.Model[testItem] {
	t.Helper()
	for _, k := range keys {
		m, _ = m.Update(tea.KeyMsg{Type: k})
	}
	return m
}

// Mechanic 1: typing filters the list and the match set reflects the query.
func TestTypingFiltersMatchSet(t *testing.T) {
	items := newTestItems("apple", "banana", "apricot", "grape", "pineapple")
	values := itemValues(items)

	tests := []struct {
		name  string
		typed string
	}{
		{"an empty query shows every item in original order", ""},
		{"a single character narrows the set", "a"},
		{"a longer query narrows further", "ap"},
		{"a query matching nothing empties the set", "xyz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := typeString(t, picker.New(items), tt.typed)

			var want []string
			if tt.typed == "" {
				want = values
			} else {
				want = matchStrings(fuzzy.Find(tt.typed, values))
			}

			got := matchStrings(m.Matches())
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Matches() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// Mechanic 2: ctrl+n and ctrl+p move the cursor and stop at the bounds.
func TestCtrlNCtrlPMoveCursorAndStopAtBounds(t *testing.T) {
	items := newTestItems("one", "two", "three")

	tests := []struct {
		name    string
		presses []tea.KeyType
		want    int
	}{
		{
			name:    "ctrl+n moves the cursor to the next match",
			presses: []tea.KeyType{tea.KeyCtrlN},
			want:    1,
		},
		{
			name:    "down arrow moves the cursor to the next match",
			presses: []tea.KeyType{tea.KeyDown},
			want:    1,
		},
		{
			name:    "up arrow moves the cursor to the previous match",
			presses: []tea.KeyType{tea.KeyDown, tea.KeyDown, tea.KeyUp},
			want:    1,
		},
		{
			name:    "ctrl+n stops at the last match",
			presses: []tea.KeyType{tea.KeyCtrlN, tea.KeyCtrlN, tea.KeyCtrlN, tea.KeyCtrlN},
			want:    2,
		},
		{
			name:    "ctrl+p moves the cursor to the previous match",
			presses: []tea.KeyType{tea.KeyCtrlN, tea.KeyCtrlN, tea.KeyCtrlP},
			want:    1,
		},
		{
			name:    "ctrl+p stops at the first match",
			presses: []tea.KeyType{tea.KeyCtrlP, tea.KeyCtrlP},
			want:    0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := pressKeys(t, picker.New(items), tt.presses...)

			if got := m.Cursor(); got != tt.want {
				t.Errorf("Cursor() = %d, want %d", got, tt.want)
			}
		})
	}
}

// Mechanic 3: the scroll offset follows the cursor past the viewport edge.
//
// Ten items, a three row viewport. The expected offsets follow the standard
// viewport-follow rule: offset only moves when the cursor would otherwise
// land outside [offset, offset+height-1], and then moves by exactly enough
// to bring the cursor back to the edge it crossed.
func TestScrollOffsetFollowsCursorPastViewportEdge(t *testing.T) {
	items := newTestItems("i0", "i1", "i2", "i3", "i4", "i5", "i6", "i7", "i8", "i9")
	const height = 3

	tests := []struct {
		name       string
		ctrlN      int
		ctrlP      int
		wantCursor int
		wantOffset int
	}{
		{
			name:       "offset stays at 0 while the cursor is within the viewport",
			ctrlN:      2,
			wantCursor: 2,
			wantOffset: 0,
		},
		{
			name:       "offset advances just enough to keep the cursor visible past the bottom edge",
			ctrlN:      3,
			wantCursor: 3,
			wantOffset: 1,
		},
		{
			name:       "offset keeps advancing as the cursor keeps moving past the bottom edge",
			ctrlN:      9,
			wantCursor: 9,
			wantOffset: 7,
		},
		{
			name:       "offset retreats as the cursor moves back up past the top edge",
			ctrlN:      9,
			ctrlP:      7,
			wantCursor: 2,
			wantOffset: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := picker.New(items).SetHeight(height)
			for i := 0; i < tt.ctrlN; i++ {
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
			}
			for i := 0; i < tt.ctrlP; i++ {
				m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlP})
			}

			if got := m.Cursor(); got != tt.wantCursor {
				t.Fatalf("Cursor() = %d, want %d", got, tt.wantCursor)
			}
			if got := m.Offset(); got != tt.wantOffset {
				t.Errorf("Offset() = %d, want %d", got, tt.wantOffset)
			}
		})
	}
}

// Mechanic 4: enter on a match reports it via Selected, enter on an empty
// match set reports nothing.
func TestEnterCommitsSelection(t *testing.T) {
	items := newTestItems("alpha", "beta", "gamma")

	t.Run("enter on a match reports it via Selected", func(t *testing.T) {
		m := pressKeys(t, picker.New(items), tea.KeyEnter)

		got, ok := m.Selected()
		if !ok {
			t.Fatal("Selected() ok = false, want true")
		}
		if diff := cmp.Diff(items[0], got, cmp.AllowUnexported(testItem{})); diff != "" {
			t.Errorf("Selected() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("enter commits the item under a moved cursor", func(t *testing.T) {
		m := pressKeys(t, picker.New(items), tea.KeyCtrlN, tea.KeyEnter)

		got, ok := m.Selected()
		if !ok {
			t.Fatal("Selected() ok = false, want true")
		}
		if diff := cmp.Diff(items[1], got, cmp.AllowUnexported(testItem{})); diff != "" {
			t.Errorf("Selected() mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("enter on an empty match set reports nothing", func(t *testing.T) {
		m := typeString(t, picker.New(items), "zzz")
		m = pressKeys(t, m, tea.KeyEnter)

		_, ok := m.Selected()
		if ok {
			t.Fatal("Selected() ok = true, want false")
		}
	})
}

// SetCursor test plan (AGENTS.md section 5, copied verbatim from F01's
// contract before any test below was written):
//
//  1. Setting the cursor to a valid index moves it there and the offset
//     follows the same rule as ctrl+n movement to that index.
//  2. An out-of-range index clamps to the bounds instead of panicking, on
//     both a populated and an empty match set.
//  3. Setting the cursor does not disturb the match set or a committed
//     selection.

// Mechanic 1: setting the cursor to a valid index moves it there and the
// offset follows the same rule as ctrl+n movement to that index. Asserted
// by comparing SetCursor's resulting Cursor/Offset against the
// Cursor/Offset produced by driving Update with i presses of ctrl+n from a
// freshly built model over the same items and height — that equivalence,
// not any particular offset value, is the actual contract.
func TestSetCursorMatchesCtrlNOffset(t *testing.T) {
	items := newTestItems("i0", "i1", "i2", "i3", "i4", "i5", "i6", "i7", "i8", "i9")
	const height = 3

	tests := []struct {
		name  string
		index int
	}{
		{"an index within the initial viewport", 0},
		{"an index within the initial viewport, non-zero", 2},
		{"an index just past the bottom edge", 3},
		{"an index near the end of the match set", 9},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := picker.New(items).SetHeight(height)
			for i := 0; i < tt.index; i++ {
				want, _ = want.Update(tea.KeyMsg{Type: tea.KeyCtrlN})
			}

			got := picker.New(items).SetHeight(height).SetCursor(tt.index)

			if got.Cursor() != want.Cursor() {
				t.Errorf("Cursor() = %d, want %d", got.Cursor(), want.Cursor())
			}
			if got.Offset() != want.Offset() {
				t.Errorf("Offset() = %d, want %d", got.Offset(), want.Offset())
			}
		})
	}
}

// Mechanic 2: an out-of-range index clamps to the bounds instead of
// panicking, on both a populated and an empty match set.
func TestSetCursorClampsOutOfRangeIndex(t *testing.T) {
	t.Run("a negative index clamps to 0 on a populated match set", func(t *testing.T) {
		items := newTestItems("one", "two", "three")
		m := picker.New(items).SetCursor(-1)

		if got := m.Cursor(); got != 0 {
			t.Errorf("Cursor() = %d, want 0", got)
		}
	})

	t.Run("an index past the end clamps to the last match", func(t *testing.T) {
		items := newTestItems("one", "two", "three")
		m := picker.New(items).SetCursor(100)

		if got := m.Cursor(); got != len(items)-1 {
			t.Errorf("Cursor() = %d, want %d", got, len(items)-1)
		}
	})

	t.Run("any index clamps to 0 on an empty match set without panicking", func(t *testing.T) {
		items := newTestItems("one", "two", "three")
		m := typeString(t, picker.New(items), "zzz")

		m = m.SetCursor(5)

		if got := m.Cursor(); got != 0 {
			t.Errorf("Cursor() = %d, want 0", got)
		}
	})
}

// Mechanic 3: setting the cursor does not disturb the match set or a
// committed selection.
func TestSetCursorLeavesMatchesAndSelectionUntouched(t *testing.T) {
	items := newTestItems("alpha", "beta", "gamma")

	t.Run("the match set is unchanged", func(t *testing.T) {
		m := picker.New(items)
		before := m.Matches()

		m = m.SetCursor(1)

		if diff := cmp.Diff(before, m.Matches()); diff != "" {
			t.Errorf("Matches() mismatch (-before +after):\n%s", diff)
		}
	})

	t.Run("a committed selection is unchanged", func(t *testing.T) {
		m := pressKeys(t, picker.New(items), tea.KeyEnter)

		m = m.SetCursor(2)

		got, ok := m.Selected()
		if !ok {
			t.Fatal("Selected() ok = false, want true")
		}
		if diff := cmp.Diff(items[0], got, cmp.AllowUnexported(testItem{})); diff != "" {
			t.Errorf("Selected() mismatch (-want +got):\n%s", diff)
		}
	})
}

// Mechanic 5: match highlight indices come back from fuzzy.Find unmodified.
func TestMatchIndexesComeFromFuzzyFindUnmodified(t *testing.T) {
	items := newTestItems("apple", "apricot", "grape", "pineapple")
	values := itemValues(items)

	tests := []struct {
		name  string
		query string
	}{
		{"a query matching one contiguous run", "gr"},
		{"a query matching scattered characters across several items", "ap"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := typeString(t, picker.New(items), tt.query)

			want := []fuzzy.Match(fuzzy.Find(tt.query, values))
			got := m.Matches()

			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("Matches() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
