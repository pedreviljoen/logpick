package transport

// Test plan for T03, one test per mechanic (AGENTS.md sections 5 and 8, T03).
//
//  1. Known placeholders are replaced, including two in one element.
//  2. An unknown placeholder returns an error naming it.
//  3. A repeated placeholder is replaced at every occurrence.
//  4. A template with no placeholders passes through unchanged.
//  5. An empty template returns an error.
//
// One point in mechanic 2 needed resolving before the assertions could be
// written, and is recorded here because it is contract, not test detail, and
// is stated in the doc comment on Substitute too:
//
//   - "Unknown placeholder" is decided to mean any placeholder syntactically
//     present in a template element that has no matching key in vals. A
//     placeholder from the known set (DESIGN.md 7.2) that happens to be
//     absent from vals is unknown by that definition and is rejected the
//     same way as a placeholder outside the known set entirely, never
//     substituted as an empty string. Mechanic 2's table exercises both
//     shapes of that one behaviour.
//
// Every error case is asserted with errors.Is against ErrBadTemplate, plus
// strings.Contains for the offending placeholder name and the element index,
// per AGENTS.md section 5. Never a full error string comparison.

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// assertSubstituted fails unless Substitute(tmpl, vals) succeeds and returns
// exactly want.
func assertSubstituted(t *testing.T, tmpl []string, vals map[string]string, want []string) {
	t.Helper()
	got, err := Substitute(tmpl, vals)
	if err != nil {
		t.Fatalf("Substitute(%q, %v) returned unexpected error: %v", tmpl, vals, err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Substitute(%q, %v) mismatch (-want +got):\n%s", tmpl, vals, diff)
	}
}

// assertBadTemplate fails unless Substitute(tmpl, vals) returns an error
// wrapping ErrBadTemplate whose message contains every string in wantHas.
func assertBadTemplate(t *testing.T, tmpl []string, vals map[string]string, wantHas []string) {
	t.Helper()
	got, err := Substitute(tmpl, vals)
	if err == nil {
		t.Fatalf("Substitute(%q, %v) = %q, nil, want an error wrapping ErrBadTemplate", tmpl, vals, got)
	}
	if !errors.Is(err, ErrBadTemplate) {
		t.Errorf("Substitute(%q, %v) error = %v, want errors.Is(err, ErrBadTemplate)", tmpl, vals, err)
	}
	for _, want := range wantHas {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Substitute(%q, %v) error = %q, want it to mention %q", tmpl, vals, err.Error(), want)
		}
	}
}

func TestSubstitute(t *testing.T) {
	t.Run("known placeholders are replaced, including two in one element", func(t *testing.T) {
		tests := []struct {
			name string
			tmpl []string
			vals map[string]string
			want []string
		}{
			{
				name: "one placeholder per element",
				tmpl: []string{"ssh", "{host}", "--", "{cmd}"},
				vals: map[string]string{"host": "jenkins-01", "cmd": "tail -n 100 /var/log/app.log"},
				want: []string{"ssh", "jenkins-01", "--", "tail -n 100 /var/log/app.log"},
			},
			{
				name: "two placeholders in one element",
				tmpl: []string{"corp-bastion", "scp", "{host}:{remote}", "{local}"},
				vals: map[string]string{
					"host":   "jenkins-01",
					"remote": "/var/log/app.log",
					"local":  "/tmp/app.log",
				},
				want: []string{"corp-bastion", "scp", "jenkins-01:/var/log/app.log", "/tmp/app.log"},
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assertSubstituted(t, tc.tmpl, tc.vals, tc.want)
			})
		}
	})

	t.Run("an unknown placeholder returns an error naming it", func(t *testing.T) {
		tests := []struct {
			name    string
			tmpl    []string
			vals    map[string]string
			idx     int
			missing string
		}{
			{
				name:    "placeholder outside the known set",
				tmpl:    []string{"ssh", "{host}", "{oops}"},
				vals:    map[string]string{"host": "jenkins-01"},
				idx:     2,
				missing: "oops",
			},
			{
				name:    "placeholder from the known set but absent from vals",
				tmpl:    []string{"ssh", "{host}", "--", "{cmd}"},
				vals:    map[string]string{"host": "jenkins-01"},
				idx:     3,
				missing: "cmd",
			},
		}
		for _, tc := range tests {
			t.Run(tc.name, func(t *testing.T) {
				assertBadTemplate(t, tc.tmpl, tc.vals, []string{tc.missing, strconv.Itoa(tc.idx)})
			})
		}
	})

	t.Run("a repeated placeholder is replaced at every occurrence", func(t *testing.T) {
		tmpl := []string{"{host}", "{host}:{host}", "echo {host} again {host}"}
		vals := map[string]string{"host": "jenkins-01"}
		want := []string{"jenkins-01", "jenkins-01:jenkins-01", "echo jenkins-01 again jenkins-01"}
		assertSubstituted(t, tmpl, vals, want)
	})

	t.Run("a template with no placeholders passes through unchanged", func(t *testing.T) {
		tmpl := []string{"find", "/var/log", "-maxdepth", "3"}
		assertSubstituted(t, tmpl, map[string]string{}, tmpl)
	})

	t.Run("an empty template returns an error", func(t *testing.T) {
		assertBadTemplate(t, []string{}, map[string]string{"host": "jenkins-01"}, nil)
	})
}
