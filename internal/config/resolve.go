package config

import (
	"errors"
	"fmt"
	"path"
)

// ErrUnknownProfile is the sentinel wrapped by every Resolve failure.
//
// Match it with errors.Is to detect the class, then use strings.Contains
// against the message for the identifying detail: Resolve names both the
// host being resolved and the profile that could not be found.
//
// It is deliberately distinct from ErrInvalidConfig, which load.go
// documents as "the sentinel wrapped by every validation failure Load
// reports". A Config that came from Load can never reach Resolve with a
// dangling profile reference, because validateProfileReferences rejects it
// first. Reaching this error therefore means one of two things that are
// not config-file faults: a --profile flag naming a profile that does not
// exist, or a Config assembled in Go rather than parsed from disk (the
// first-run flow of T18). Callers branch differently on those than on "the
// user's config.toml is malformed", so the two sentinels stay separate.
var ErrUnknownProfile = errors.New("unknown profile")

// DefaultProfileName is the profile name Resolve reports when no level of
// the configuration names one: neither the --profile flag, nor a
// [host.<name>] entry, nor the first matching [[match]] rule.
//
// If cfg defines a [profile.default], it is applied as precedence level 4
// in the usual way. If it does not, resolution stops at the built-in
// DefaultProfile and this is still the reported name, so ProfileName is
// never empty in a successful result.
const DefaultProfileName = "default"

// DefaultProfile is precedence level 5, the built-in defaults, and the
// reason a zero Config (and a nil *Config) still resolves to something
// usable: see the Config doc comment and AGENTS.md section 4.1, "zero value
// useful".
//
// Exec comes verbatim from the [profile.default] example in DESIGN.md
// section 6.1. Scan is modelled on the [profile.corp.scan] example in the
// same section, narrowed to the one path that exists on every Unix host.
// DESIGN.md does not spell these values out; they are chosen here so that
// the tool works against a host with no config.toml at all.
//
// Treat this value as read-only. It is a var rather than a func so that
// tests and callers can name the defaults directly, which means its slices
// are addressable: Resolve must not return them by reference or append to
// them, and no caller may mutate them in place.
var DefaultProfile = Profile{
	Exec: []string{"ssh", "{host}", "--", "{cmd}"},
	Scan: ScanSpec{
		Paths:    []string{"/var/log"},
		MaxDepth: 3,
		Include:  []string{"*.log", "*.log.*", "*.out", "messages", "syslog"},
		Exclude:  []string{"*.gz", "*.zip", "lastlog", "wtmp", "btmp"},
	},
}

// Overrides carries the command-line flags that outrank every level of the
// config file: --profile and --path. It is precedence level 1 in DESIGN.md
// section 6.4.
//
// Both fields are pointers because "the flag was not given" and "the flag
// was given an empty value" are different intents, and conflating them is
// the exact bug class this type exists to prevent. A nil field means the
// flag was absent and that level contributes nothing. A non-nil field means
// the flag was given, and its value wins even when that value is empty:
// --path with no paths resolves to a ScanSpec with no paths, it does not
// silently fall back to the profile's paths.
//
// The zero Overrides therefore means "no flags given", which is what a
// caller with nothing to override passes.
//
// Callers populate it from the flag set's own notion of presence, for
// example cobra's Flags().Changed, rather than from emptiness:
//
//	var ov config.Overrides
//	if cmd.Flags().Changed("profile") {
//		ov.Profile = &profileFlag
//	}
//	if cmd.Flags().Changed("path") {
//		ov.Paths = &pathFlags
//	}
type Overrides struct {
	// Profile is the --profile flag, or nil if it was not given. A
	// non-nil pointer to an empty string names no profile and falls
	// through to the level below, the same as an empty Profile field on a
	// HostEntry or a MatchRule.
	Profile *string

	// Paths is the --path flag, or nil if it was not given. A non-nil
	// pointer to an empty or nil slice overrides ScanSpec.Paths to empty
	// rather than inheriting it.
	Paths *[]string
}

// Resolve layers the CLI overrides, the [host.<name>] entry, the first
// matching [[match]] rule, the named profile and the built-in defaults into
// one concrete ResolvedHost for host.
//
// It is a pure function: no I/O, no context, no package state read or
// written beyond DefaultProfile. A nil cfg is treated as an empty Config,
// so Resolve(nil, host, Overrides{}) succeeds and returns the built-in
// defaults.
//
// # Precedence
//
// Highest first, per DESIGN.md section 6.4:
//
//  1. CLI flags, the Overrides argument
//  2. the cfg.Hosts[host] entry
//  3. the first cfg.Matches rule whose Host pattern matches, in file order
//  4. the named profile in cfg.Profiles
//  5. DefaultProfile
//
// The levels merge field by field, and each level wins only over the fields
// it actually sets. A level that sets nothing contributes nothing, and a
// level that sets one field does not displace the others. This applies
// across levels as well as within them: when a [host.<name>] entry supplies
// the profile name and a [[match]] rule also matches the same host, the
// host entry wins the profile name (level 2 over level 3) while the rule's
// scan overrides still apply over the profile's (level 3 over level 4).
// Levels are not short-circuited by the level above them.
//
// # Choosing the profile
//
// The effective profile name is the first non-empty of: *ov.Profile if
// ov.Profile is non-nil, then the host entry's Profile, then the matching
// rule's Profile, then DefaultProfileName. An empty string at any of those
// levels means "this level names no profile" and falls through, so a
// HostEntry that carries only a Label resolves rather than failing. Note
// that Load is stricter: validateProfileReferences already rejects an empty
// profile reference in a file-loaded Config, so this leniency only shows up
// for a Config built in Go.
//
// Only the effective name is looked up. A profile named by a level that
// lost is never consulted and never validated.
//
// If the effective name is non-empty and absent from cfg.Profiles, Resolve
// returns the zero ResolvedHost and an error wrapping ErrUnknownProfile
// that names both host and the missing profile. The one exception is
// DefaultProfileName reached by fallback, when no level named anything: a
// config with no [profile.default] is not an error, it resolves to the
// built-in defaults. That is mechanic 4.
//
// # Matching
//
// Rule patterns are matched with path.Match against the bare host string,
// in slice order, and the first match wins. path.Match's wildcards do not
// cross '/', but a hostname contains no '/', so '*' spans dots freely:
// "*.prod.internal" matches both "web-01.prod.internal" and
// "a.b.prod.internal". A pattern that path.Match rejects as malformed is
// not silently skipped; Resolve returns an error wrapping path.ErrBadPattern
// and naming the pattern.
//
// # Merging a ScanSpec
//
// ScanSpec fields merge individually, never wholesale. MatchRule.Scan and
// HostEntry.Scan are plain values, so "not overridden at this level" is the
// zero value for the field:
//
//   - Paths, Include and Exclude: a nil slice inherits from the level
//     below. A non-nil slice overrides, including a non-nil empty one.
//     TOML's paths = [] unmarshals to a non-nil empty slice, so writing it
//     means "scan nothing here", not "inherit". This is the same rule as
//     Overrides: presence decides, not emptiness.
//   - MaxDepth: 0 means inherit. Depth zero is not expressible, and that is
//     deliberate rather than an oversight, because a depth-zero find can
//     never reach a file inside a directory and so has no useful meaning
//     for this tool. The built-in default is DefaultProfile.Scan.MaxDepth.
//
// The remaining Profile fields merge by the same presence rule against the
// level below: a nil Exec, a nil Copy, a nil SudoPrefix, an empty AuthHint
// and a zero ConnectTimeout all inherit, and Persistent is a plain
// override, which is unambiguous only because the built-in default is
// false.
//
// # Result
//
// Name is host verbatim. ProfileName is the effective profile name, never
// empty. Profile is the fully merged profile, with Profile.Scan carrying
// the merged ScanSpec. Label is the host entry's Label, or empty when there
// is no entry or no label; callers display Name in that case.
//
// The returned value shares no slice backing array with cfg or with
// DefaultProfile, so a caller may mutate it without corrupting either.
func Resolve(cfg *Config, host string, ov Overrides) (ResolvedHost, error) {
	if cfg == nil {
		cfg = &Config{}
	}

	hostEntry, hasHostEntry := cfg.Hosts[host]

	var matchRule MatchRule
	hasMatchRule := false
	for _, m := range cfg.Matches {
		matched, err := path.Match(m.Host, host)
		if err != nil {
			return ResolvedHost{}, fmt.Errorf("resolving host %q: match pattern %q: %w", host, m.Host, err)
		}
		if matched {
			matchRule = m
			hasMatchRule = true
			break
		}
	}

	// The effective profile name is the first non-empty of: the --profile
	// flag, the host entry's Profile, the matching rule's Profile. An empty
	// string at any level means "names nothing" and falls through to the
	// level below (mechanic 7). fellThrough tracks whether no level named
	// anything at all, which controls whether a missing DefaultProfileName
	// entry is an error or the built-in defaults (mechanic 6 in the doc
	// comment above).
	name := ""
	switch {
	case ov.Profile != nil && *ov.Profile != "":
		name = *ov.Profile
	case hasHostEntry && hostEntry.Profile != "":
		name = hostEntry.Profile
	case hasMatchRule && matchRule.Profile != "":
		name = matchRule.Profile
	}
	fellThrough := name == ""
	if fellThrough {
		name = DefaultProfileName
	}

	profile, ok := cfg.Profiles[name]
	if !ok {
		if !fellThrough {
			return ResolvedHost{}, fmt.Errorf("resolving host %q: %w: profile %q not found", host, ErrUnknownProfile, name)
		}
		profile = DefaultProfile
	}

	merged := mergeProfileFields(profile, DefaultProfile)

	scan := mergeScan(DefaultProfile.Scan, profile.Scan)
	if hasMatchRule {
		scan = mergeScan(scan, matchRule.Scan)
	}
	if hasHostEntry {
		scan = mergeScan(scan, hostEntry.Scan)
	}
	if ov.Paths != nil {
		scan.Paths = *ov.Paths
	}
	merged.Scan = scan

	label := ""
	if hasHostEntry {
		label = hostEntry.Label
	}

	result := ResolvedHost{
		Name:        host,
		ProfileName: name,
		Profile:     merged,
		Label:       label,
	}
	cloneResolvedHostSlices(&result)
	return result, nil
}

// mergeProfileFields merges top's non-Scan fields over base, field by
// field: a nil slice, a zero Duration and an empty string all inherit from
// base, and Persistent is a plain override (see the "Merging a ScanSpec"
// and surrounding doc comment on Resolve for the rationale). Scan is left
// as base.Scan; callers overwrite it with the result of mergeScan, which
// folds in the match rule and host entry levels that Profile alone does
// not carry.
func mergeProfileFields(top, base Profile) Profile {
	result := base
	if top.Exec != nil {
		result.Exec = top.Exec
	}
	if top.Copy != nil {
		result.Copy = top.Copy
	}
	result.Persistent = top.Persistent
	if top.ConnectTimeout != 0 {
		result.ConnectTimeout = top.ConnectTimeout
	}
	if top.AuthHint != "" {
		result.AuthHint = top.AuthHint
	}
	if top.SudoPrefix != nil {
		result.SudoPrefix = top.SudoPrefix
	}
	return result
}

// mergeScan merges override on top of base field by field: a nil slice
// inherits base's value, a non-nil slice (including a non-nil empty one)
// overrides it, and a zero MaxDepth inherits.
func mergeScan(base, override ScanSpec) ScanSpec {
	result := base
	if override.Paths != nil {
		result.Paths = override.Paths
	}
	if override.MaxDepth != 0 {
		result.MaxDepth = override.MaxDepth
	}
	if override.Include != nil {
		result.Include = override.Include
	}
	if override.Exclude != nil {
		result.Exclude = override.Exclude
	}
	return result
}

// cloneResolvedHostSlices replaces every slice field reachable from r with
// a fresh copy, preserving nil-ness. Resolve builds r's fields by
// reassigning slice headers from cfg and from DefaultProfile without
// copying, for simplicity of the merge logic above; this is the single
// point where that sharing is cut, so the returned ResolvedHost aliases
// neither cfg nor DefaultProfile (see the "no shared slice backing array"
// paragraph on Resolve's doc comment).
func cloneResolvedHostSlices(r *ResolvedHost) {
	r.Profile.Exec = cloneStrings(r.Profile.Exec)
	r.Profile.Copy = cloneStrings(r.Profile.Copy)
	r.Profile.SudoPrefix = cloneStrings(r.Profile.SudoPrefix)
	r.Profile.Scan.Paths = cloneStrings(r.Profile.Scan.Paths)
	r.Profile.Scan.Include = cloneStrings(r.Profile.Scan.Include)
	r.Profile.Scan.Exclude = cloneStrings(r.Profile.Scan.Exclude)
}

// cloneStrings returns a copy of s with its own backing array, preserving
// whether s was nil: a nil slice clones to nil, a non-nil (including
// empty) slice clones to a non-nil slice of the same length. That
// distinction matters throughout this file; see mechanic 4 on Resolve.
func cloneStrings(s []string) []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
