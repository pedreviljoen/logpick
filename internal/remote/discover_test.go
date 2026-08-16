package remote

// Test plan for T09 (AGENTS.md sections 5 and 8, T09). shellquote_test.go
// covers mechanic 1 and the Quote fuzz target; this file covers the rest.
//
//  2. BuildScan in GNU mode emits -printf, in BSD mode emits the ls -ldn
//     form.
//  3. Literal paths are quoted, paths containing glob metacharacters are
//     not.
//  4. Include and exclude patterns become the expected -name clauses.
//  5. The generated command always ends with 2>/dev/null.
//
// One extra mechanic beyond the five listed in AGENTS.md: BuildScan's
// contract (discover.go) documents two zero-value edge cases that DESIGN.md
// does not spell out and that a caller depends on getting right --
// spec.Paths empty, and spec.MaxDepth == 0 meaning "not set" per T07's
// resolution. Both are behaviour that can break independently of mechanics
// 2 through 5, so:
//
//  6. Zero paths returns the empty string, in both GNU and BSD mode, and a
//     zero MaxDepth omits -maxdepth entirely rather than emitting
//     "-maxdepth 0".

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/pedreviljoen/logpick/internal/config"
)

func TestBuildScan_GNUvsBSD(t *testing.T) {
	spec := config.ScanSpec{
		Paths:    []string{"/var/log", "/opt/app/logs"},
		MaxDepth: 3,
		Include:  []string{"*.log", "*.log.*", "*.out"},
		Exclude:  []string{"*.gz"},
	}

	tests := []struct {
		name    string
		gnuFind bool
		want    string
	}{
		{
			name:    "GNU mode emits -printf",
			gnuFind: true,
			want:    `find '/var/log' '/opt/app/logs' -maxdepth 3 -type f \( -name '*.log' -o -name '*.log.*' -o -name '*.out' \) ! -name '*.gz' -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name:    "BSD mode emits the ls -ldn form",
			gnuFind: false,
			want:    `find '/var/log' '/opt/app/logs' -maxdepth 3 -type f \( -name '*.log' -o -name '*.log.*' -o -name '*.out' \) ! -name '*.gz' -exec ls -ldn -- {} + 2>/dev/null`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildScan(spec, tt.gnuFind)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("BuildScan mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildScan_PathQuoting(t *testing.T) {
	tests := []struct {
		name string
		spec config.ScanSpec
		want string
	}{
		{
			name: "a literal path is quoted",
			spec: config.ScanSpec{Paths: []string{"/var/log"}},
			want: `find '/var/log' -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name: "a glob path is left unquoted so the remote shell expands it",
			spec: config.ScanSpec{Paths: []string{"/srv/*/logs"}},
			want: `find /srv/*/logs -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name: "a literal path with a space survives only because it is quoted",
			spec: config.ScanSpec{Paths: []string{"/opt/app logs"}},
			want: `find '/opt/app logs' -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name: "? and [ are glob metacharacters too",
			spec: config.ScanSpec{Paths: []string{"/var/log?", "/var/lo[gG]"}},
			want: `find /var/log? /var/lo[gG] -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name: "glob and literal paths mix, each quoted independently",
			spec: config.ScanSpec{Paths: []string{"/srv/*/logs", "/var/log", "/opt/app logs"}},
			want: `find /srv/*/logs '/var/log' '/opt/app logs' -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildScan(tt.spec, true)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("BuildScan mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildScan_IncludeExclude(t *testing.T) {
	tests := []struct {
		name string
		spec config.ScanSpec
		want string
	}{
		{
			name: "multiple include and exclude patterns",
			spec: config.ScanSpec{
				Paths:   []string{"/var/log"},
				Include: []string{"*.log", "*.out"},
				Exclude: []string{"*.gz", "*.zip"},
			},
			want: `find '/var/log' -type f \( -name '*.log' -o -name '*.out' \) ! -name '*.gz' ! -name '*.zip' -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name: "include only, no exclude clause is emitted",
			spec: config.ScanSpec{
				Paths:   []string{"/var/log"},
				Include: []string{"*.log"},
			},
			want: `find '/var/log' -type f \( -name '*.log' \) -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name: "exclude only, no include group is emitted",
			spec: config.ScanSpec{
				Paths:   []string{"/var/log"},
				Exclude: []string{"*.gz"},
			},
			want: `find '/var/log' -type f ! -name '*.gz' -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name: "neither include nor exclude emits neither clause",
			spec: config.ScanSpec{
				Paths: []string{"/var/log"},
			},
			want: `find '/var/log' -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildScan(tt.spec, true)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("BuildScan mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

func TestBuildScan_AlwaysRedirectsStderr(t *testing.T) {
	tests := []struct {
		name    string
		spec    config.ScanSpec
		gnuFind bool
	}{
		{
			name:    "GNU mode, full spec",
			spec:    config.ScanSpec{Paths: []string{"/var/log"}, MaxDepth: 3, Include: []string{"*.log"}, Exclude: []string{"*.gz"}},
			gnuFind: true,
		},
		{
			name:    "BSD mode, full spec",
			spec:    config.ScanSpec{Paths: []string{"/var/log"}, MaxDepth: 3, Include: []string{"*.log"}, Exclude: []string{"*.gz"}},
			gnuFind: false,
		},
		{
			name:    "GNU mode, bare spec with only a path",
			spec:    config.ScanSpec{Paths: []string{"/var/log"}},
			gnuFind: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildScan(tt.spec, tt.gnuFind)
			assertEndsWithDevNullRedirect(t, got)
		})
	}
}

func TestBuildScan_ZeroValueEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		spec    config.ScanSpec
		gnuFind bool
		want    string
	}{
		{
			name:    "nil Paths returns the empty string in GNU mode",
			spec:    config.ScanSpec{Paths: nil},
			gnuFind: true,
			want:    "",
		},
		{
			name:    "nil Paths returns the empty string in BSD mode",
			spec:    config.ScanSpec{Paths: nil},
			gnuFind: false,
			want:    "",
		},
		{
			name:    "explicitly empty Paths also returns the empty string",
			spec:    config.ScanSpec{Paths: []string{}},
			gnuFind: true,
			want:    "",
		},
		{
			name:    "MaxDepth 0 omits -maxdepth rather than emitting -maxdepth 0",
			spec:    config.ScanSpec{Paths: []string{"/var/log"}, MaxDepth: 0},
			gnuFind: true,
			want:    `find '/var/log' -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
		{
			name:    "a positive MaxDepth is emitted before -type f",
			spec:    config.ScanSpec{Paths: []string{"/var/log"}, MaxDepth: 1},
			gnuFind: true,
			want:    `find '/var/log' -maxdepth 1 -type f -printf '%s\t%T@\t%p\n' 2>/dev/null`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BuildScan(tt.spec, tt.gnuFind)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("BuildScan mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// assertEndsWithDevNullRedirect fails the test unless got ends with the
// stderr-discarding redirect every BuildScan output must carry (DESIGN.md
// 8.3, AGENTS.md invariant 2: this command is read-only, no redirection
// besides this one).
func assertEndsWithDevNullRedirect(t *testing.T, got string) {
	t.Helper()

	const want = "2>/dev/null"
	if !strings.HasSuffix(got, want) {
		t.Errorf("BuildScan output %q does not end with %q", got, want)
	}
}
