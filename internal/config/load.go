package config

import (
	"errors"
	"fmt"
	"os"

	"github.com/pelletier/go-toml/v2"
)

// ErrInvalidConfig is the sentinel wrapped by every validation failure Load
// reports. Match it with errors.Is to detect a validation failure as a
// class, then use strings.Contains against the error's message for the
// identifying detail: the offending profile name, placeholder, or match
// rule index.
//
// Load aggregates every problem it finds into a single error built with
// errors.Join, rather than stopping at the first, so errors.Is still
// matches and the message names every distinct fault.
var ErrInvalidConfig = errors.New("invalid config")

// Load reads the TOML file at path, parses it into a Config, and validates
// it before returning.
//
// Validation checks, all reported together rather than one at a time:
//
//   - Every profile named by a [[match]] rule or a [host.<name>] entry
//     exists in [profile.*].
//   - Every {placeholder} in an exec or copy template is one of the known
//     set: {host}, {user}, {port}, {cmd}, {remote}, {local}. An unknown
//     placeholder is a load error, never a silent literal (DESIGN.md
//     sections 7.2 and 12).
//
// On a validation failure, Load returns a nil *Config and an error that
// wraps ErrInvalidConfig and can be unwrapped with errors.Is to find every
// individual fault.
//
// A config.toml that is group- or world-writable is not a validation
// failure. Load appends a human-readable warning to the returned Config's
// Warnings field and still returns a usable Config (DESIGN.md section 12).
//
// Load never panics; I/O and parse failures come back as a plain wrapped
// error, not ErrInvalidConfig, which is reserved for validation failures.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is the caller-chosen config location (XDG default or a CLI flag), not untrusted input; this is Load's whole job.
	if err != nil {
		return nil, fmt.Errorf("loading %s: %w", path, err)
	}

	var cfg Config
	if err := toml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var faults []error
	faults = append(faults, validatePlaceholders(&cfg)...)
	faults = append(faults, validateProfileReferences(&cfg)...)

	if len(faults) > 0 {
		return nil, errors.Join(faults...)
	}

	if info, statErr := os.Stat(path); statErr == nil {
		if warning := writabilityWarning(path, info); warning != "" {
			cfg.Warnings = append(cfg.Warnings, warning)
		}
	}

	return &cfg, nil
}
