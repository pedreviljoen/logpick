package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// Test plan for T06 (DESIGN.md section 6, AGENTS.md section 5). Copied
// verbatim from the task before any test below was written.
//
// Mechanics:
//  1. A valid config round-trips into the expected struct.
//  2. connect_timeout = "30s" parses to a time.Duration.
//  3. A config with several distinct faults reports all of them in one
//     error.
//  4. A [[match]] naming an undefined profile is a validation error.
//  5. A group or world writable file produces a warning, not an error, and
//     the config still loads.

// fixture returns the path to a file under testdata, relative to the
// package directory that "go test" runs in.
func fixture(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join("testdata", name)
}

func TestLoad(t *testing.T) {
	t.Run("valid config round-trips into the expected struct", func(t *testing.T) {
		got, err := Load(fixture(t, "valid.toml"))
		if err != nil {
			t.Fatalf("Load returned an unexpected error: %v", err)
		}

		want := &Config{
			Profiles: map[string]Profile{
				"default": {
					Exec: []string{"ssh", "{host}", "--", "{cmd}"},
				},
				"corp": {
					Exec:           []string{"corp-bastion", "connect", "{host}", "--exec", "{cmd}"},
					Copy:           []string{"corp-bastion", "scp", "{host}:{remote}", "{local}"},
					Persistent:     true,
					ConnectTimeout: Duration(30 * time.Second),
					AuthHint:       "corp-bastion login",
					Scan: ScanSpec{
						Paths:    []string{"/var/log", "/opt/app/logs", "/srv/*/logs"},
						MaxDepth: 3,
						Include:  []string{"*.log", "*.log.*", "*.out", "messages", "syslog"},
						Exclude:  []string{"*.gz", "*.zip", "lastlog", "wtmp", "btmp"},
					},
				},
			},
			Matches: []MatchRule{
				{Host: "*.prod.internal", Profile: "corp"},
				{
					Host:    "*.staging.internal",
					Profile: "corp",
					Scan: ScanSpec{
						Paths: []string{"/var/log", "/opt/app/logs", "/tmp/debug"},
					},
				},
			},
			Hosts: map[string]HostEntry{
				"jenkins-01": {
					Profile: "corp",
					Scan: ScanSpec{
						Paths: []string{"/var/lib/jenkins/logs"},
					},
					Label: "CI primary",
				},
			},
		}

		if diff := cmp.Diff(want, got); diff != "" {
			t.Errorf("Load(valid.toml) mismatch (-want +got):\n%s", diff)
		}
	})

	t.Run("connect_timeout string parses to a time.Duration", func(t *testing.T) {
		got, err := Load(fixture(t, "duration.toml"))
		if err != nil {
			t.Fatalf("Load returned an unexpected error: %v", err)
		}

		profile, ok := got.Profiles["corp"]
		if !ok {
			t.Fatalf("expected profile %q to be present, got %v", "corp", got.Profiles)
		}

		want := 30 * time.Second
		if got := time.Duration(profile.ConnectTimeout); got != want {
			t.Errorf("ConnectTimeout = %v, want %v", got, want)
		}
	})

	t.Run("several distinct faults are reported in one error", func(t *testing.T) {
		got, err := Load(fixture(t, "multi_fault.toml"))
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if got != nil {
			t.Errorf("expected a nil Config on validation failure, got %+v", got)
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("expected error to wrap ErrInvalidConfig, got: %v", err)
		}

		// Each of the three independent faults baked into multi_fault.toml
		// must be identifiable in the aggregated error.
		details := []string{
			"oops",    // the unknown placeholder in profile "solid"
			"ghost",   // the undefined profile named by the [[match]] rule
			"phantom", // the undefined profile named by [host.myhost]
		}
		for _, d := range details {
			d := d
			t.Run(d, func(t *testing.T) {
				if !strings.Contains(err.Error(), d) {
					t.Errorf("expected error to mention %q, got: %v", d, err)
				}
			})
		}
	})

	t.Run("match rule naming an undefined profile is a validation error", func(t *testing.T) {
		got, err := Load(fixture(t, "match_undefined_profile.toml"))
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		if got != nil {
			t.Errorf("expected a nil Config on validation failure, got %+v", got)
		}
		if !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("expected error to wrap ErrInvalidConfig, got: %v", err)
		}
		if !strings.Contains(err.Error(), "missing") {
			t.Errorf("expected error to name the undefined profile %q, got: %v", "missing", err)
		}
	})

	t.Run("group or world writable file produces a warning, not an error", func(t *testing.T) {
		const content = `[profile.default]
exec = ["ssh", "{host}", "--", "{cmd}"]
`

		cases := []struct {
			name string
			mode os.FileMode
		}{
			{name: "group writable", mode: 0o664},
			{name: "world writable", mode: 0o646},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				dir := t.TempDir()
				path := filepath.Join(dir, "config.toml")

				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatalf("writing fixture: %v", err)
				}
				if err := os.Chmod(path, tc.mode); err != nil {
					t.Fatalf("chmod fixture: %v", err)
				}

				got, err := Load(path)
				if err != nil {
					t.Fatalf("Load returned an unexpected error: %v", err)
				}
				if got == nil {
					t.Fatal("expected a usable Config, got nil")
				}
				if len(got.Warnings) == 0 {
					t.Fatal("expected at least one warning about file permissions")
				}
			})
		}
	})
}
