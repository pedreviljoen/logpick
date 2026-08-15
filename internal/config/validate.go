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
	}
	return faults
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
