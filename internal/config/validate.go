package config

import (
	"fmt"
	"io/fs"
	"regexp"
	"sort"
)

// placeholderPattern matches a {word} style template placeholder, e.g.
// {host} or the unknown {oops} used in the multi-fault test fixture.
var placeholderPattern = regexp.MustCompile(`\{([a-zA-Z]+)\}`)
var hexColorPattern = regexp.MustCompile(`^#[0-9a-fA-F]{6}$`)

// knownPlaceholders is the set of substitutions Substitute (T03) honours.
// See DESIGN.md sections 7.2 and 12: anything outside this set is a config
// load error, never a silent literal.
var knownPlaceholders = map[string]bool{
	"host":   true,
	"user":   true,
	"port":   true,
	"cmd":    true,
	"remote": true,
	"local":  true,
}

// validatePlaceholders checks every exec and copy template in every profile
// for placeholders outside knownPlaceholders, returning one fault per
// offending placeholder.
func validatePlaceholders(cfg *Config) []error {
	var faults []error
	for _, name := range sortedKeys(cfg.Profiles) {
		p := cfg.Profiles[name]
		faults = append(faults, checkTemplate(name, "exec", p.Exec)...)
		faults = append(faults, checkTemplate(name, "copy", p.Copy)...)
		faults = append(faults, checkExecPlaceholders(name, p)...)
	}
	return faults
}

// checkExecPlaceholders enforces that a profile's exec template carries the
// placeholders it cannot function without. It is separate from
// checkTemplate, which only rejects *unknown* placeholders: this rejects a
// syntactically valid template that is missing a *required* one.
//
// The rules follow how transport.Substitute (T03) fills a template: it only
// replaces placeholders that literally appear, so a value with nowhere to go
// is silently dropped rather than errored. That is exactly how a template
// like ["ec2-ssh", "{host}"] produces a session that connects but runs no
// command - the {cmd} the scanner passes has no slot and vanishes, so `find`
// never runs and the file list comes back empty with no error anywhere.
// Catching it here turns that silent dead end into a load-time error naming
// the profile.
//
//   - {host} is required whenever exec is defined: a Profile describes a
//     class of hosts (DESIGN.md 6.2), so the specific host the user picked
//     can only reach the command line through {host}.
//   - {cmd} is required unless the profile is persistent. A non-persistent
//     profile runs one process per command and must have somewhere to put
//     that command. A persistent profile (DESIGN.md 7.3) instead spawns an
//     interactive shell once and writes commands to its stdin, so its exec
//     template names only how to open that shell - ec2-ssh's `ec2-ssh
//     <host>`, which takes no command argument at all - and legitimately
//     omits {cmd}.
//
// A profile that omits exec entirely is not checked: it inherits the
// built-in DefaultProfile exec at resolution (see mergeProfileFields in
// resolve.go), which already carries both placeholders.
func checkExecPlaceholders(profile string, p Profile) []error {
	if len(p.Exec) == 0 {
		return nil
	}

	var faults []error
	if !execHasPlaceholder(p.Exec, "host") {
		faults = append(faults, fmt.Errorf(
			"profile %q: exec template must contain {host} so the target host reaches the command: %w",
			profile, ErrInvalidConfig,
		))
	}
	if !p.Persistent && !execHasPlaceholder(p.Exec, "cmd") {
		faults = append(faults, fmt.Errorf(
			"profile %q: exec template must contain {cmd} (or set persistent = true for a ssh wrapper): %w",
			profile, ErrInvalidConfig,
		))
	}
	return faults
}

// execHasPlaceholder reports whether any element of tmpl contains the
// {name} placeholder, matching the same {word} syntax placeholderPattern
// recognises so "{host}" is found but a bare "host" substring is not.
func execHasPlaceholder(tmpl []string, name string) bool {
	target := "{" + name + "}"
	for _, elem := range tmpl {
		for _, m := range placeholderPattern.FindAllStringSubmatch(elem, -1) {
			if "{"+m[1]+"}" == target {
				return true
			}
		}
	}
	return false
}

// checkTemplate reports a fault for every placeholder in tmpl that is not
// in knownPlaceholders.
func checkTemplate(profile, kind string, tmpl []string) []error {
	var faults []error
	for _, elem := range tmpl {
		for _, m := range placeholderPattern.FindAllStringSubmatch(elem, -1) {
			ph := m[1]
			if !knownPlaceholders[ph] {
				faults = append(faults, fmt.Errorf(
					"profile %q: unknown placeholder {%s} in %s template: %w",
					profile, ph, kind, ErrInvalidConfig,
				))
			}
		}
	}
	return faults
}

// validateProfileReferences checks that every profile named by a [[match]]
// rule or a [host.<name>] entry is defined under [profile.*].
func validateTheme(cfg *Config) []error {
	var faults []error
	fields := []struct{ name, value string }{
		{"primary", cfg.Theme.Primary},
		{"secondary", cfg.Theme.Secondary},
	}
	for _, field := range fields {
		if field.value != "" && !hexColorPattern.MatchString(field.value) {
			faults = append(faults, fmt.Errorf("theme.%s must be a #RRGGBB hex color: %w", field.name, ErrInvalidConfig))
		}
	}
	return faults
}

func validateProfileReferences(cfg *Config) []error {
	var faults []error

	for i, m := range cfg.Matches {
		if _, ok := cfg.Profiles[m.Profile]; !ok {
			faults = append(faults, fmt.Errorf(
				"match rule %d (host %q): undefined profile %q: %w",
				i, m.Host, m.Profile, ErrInvalidConfig,
			))
		}
	}

	for _, host := range sortedKeys(cfg.Hosts) {
		entry := cfg.Hosts[host]
		if _, ok := cfg.Profiles[entry.Profile]; !ok {
			faults = append(faults, fmt.Errorf(
				"host %q: undefined profile %q: %w",
				host, entry.Profile, ErrInvalidConfig,
			))
		}
	}

	return faults
}

// writabilityWarning returns a human-readable warning if info describes a
// group- or world-writable file, or the empty string otherwise (DESIGN.md
// section 12).
func writabilityWarning(path string, info fs.FileInfo) string {
	const groupOrWorldWritable = 0o022
	if info.Mode().Perm()&groupOrWorldWritable == 0 {
		return ""
	}
	return fmt.Sprintf("%s is group- or world-writable, consider chmod 600 %s", path, path)
}

// sortedKeys returns the keys of m in sorted order, so validation faults
// are reported in a deterministic sequence regardless of Go's randomised
// map iteration order.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
