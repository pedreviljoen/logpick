// Package transport runs commands on a remote host and streams their stdout.
//
// Everything remote is that one capability. Discovery is find, preview is
// tail -n, follow is tail -f and fetch is cat. There is no SSH library and no
// protocol code: a backend substitutes into an argv template and runs it.
package transport
