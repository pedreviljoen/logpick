// Package state owns state.toml, the tool's own record of host history,
// probed capabilities, cached listings and fetched files.
//
// It is safe to delete at any time; the tool rebuilds it. Every write is
// atomic and guarded by an advisory lock so concurrent sessions do not
// clobber each other.
package state
