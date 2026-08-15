// Package config parses and resolves the user-owned config.toml.
//
// It holds no I/O beyond reading that file: resolution of a host into a
// concrete profile is a pure function, see Resolve.
package config
