package config

import (
	"fmt"
	"time"
)

// Duration is time.Duration with a TOML-aware text unmarshaller.
//
// go-toml/v2 has no built-in conversion from a TOML string such as "30s"
// into a time.Duration (see AGENTS.md section 9, "go-toml and
// time.Duration"). Because you cannot attach a method to the stdlib
// time.Duration type from this package, Profile.ConnectTimeout is declared
// as Duration instead. It is defined directly in terms of time.Duration, so
// callers convert with a plain type conversion: time.Duration(d).
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler so go-toml/v2 parses a
// duration string, e.g. connect_timeout = "30s", directly into d using the
// same syntax as time.ParseDuration.
func (d *Duration) UnmarshalText(text []byte) error {
	parsed, err := time.ParseDuration(string(text))
	if err != nil {
		return fmt.Errorf("parsing duration %q: %w", text, err)
	}
	*d = Duration(parsed)
	return nil
}

// Config is the parsed and validated contents of config.toml.
//
// The zero Config is useful: it has no profiles, match rules or hosts, and
// resolving any host against it succeeds using built-in defaults (see
// DESIGN.md section 6.4). It never panics.
type Config struct {
	Profiles map[string]Profile   `toml:"profile"`
	Matches  []MatchRule          `toml:"match"`
	Hosts    map[string]HostEntry `toml:"host"`

	// Warnings holds non-fatal problems noticed while loading, such as a
	// config.toml that is group- or world-writable (DESIGN.md section 12).
	// Unlike a validation error, a warning does not stop Load from
	// returning a usable Config. Warnings is never populated from the TOML
	// file itself; Load is the only writer.
	Warnings []string `toml:"-"`
}

// Profile describes how to reach a class of hosts: the command template
// used to run something remotely, an optional native copy template, and the
// default scan behaviour for that class of host.
//
// Fields and TOML tags come verbatim from DESIGN.md section 6.2, except
// ConnectTimeout, which uses the Duration wrapper type documented above in
// place of a bare time.Duration.
type Profile struct {
	Exec           []string `toml:"exec"`
	Copy           []string `toml:"copy"`
	Persistent     bool     `toml:"persistent"`
	ConnectTimeout Duration `toml:"connect_timeout"`
	AuthHint       string   `toml:"auth_hint"`
	SudoPrefix     []string `toml:"sudo_prefix"`
	Scan           ScanSpec `toml:"scan"`
}

// ScanSpec describes which remote paths to search for logs, and how deep
// and which names to include or exclude.
type ScanSpec struct {
	Paths    []string `toml:"paths"`
	MaxDepth int      `toml:"max_depth"`
	Include  []string `toml:"include"`
	Exclude  []string `toml:"exclude"`
}

// MatchRule assigns a profile to any host whose name matches Host, a
// path.Match-style glob (see DESIGN.md section 6.1 and 6.4), with optional
// per-rule overrides to that profile's ScanSpec. Rules are evaluated in
// file order; the first match wins.
//
// A zero-valued field in Scan means "not overridden here" and is left to
// resolution (T07) to fill in from the named profile.
type MatchRule struct {
	Host    string   `toml:"host"`
	Profile string   `toml:"profile"`
	Scan    ScanSpec `toml:"scan"`
}

// HostEntry pins a single, named host to a profile, with optional
// per-host overrides to that profile's ScanSpec and a human-readable label
// shown in the UI in place of the raw hostname.
//
// A zero-valued field in Scan means "not overridden here", the same as in
// MatchRule.
type HostEntry struct {
	Profile string   `toml:"profile"`
	Scan    ScanSpec `toml:"scan"`
	Label   string   `toml:"label"`
}

// ResolvedHost is the outcome of layering CLI overrides, a host entry, a
// match rule and a named profile into one concrete configuration for a
// single host. See Resolve in resolve.go (T07).
type ResolvedHost struct {
	Name        string
	ProfileName string
	Profile     Profile
	Label       string
}
