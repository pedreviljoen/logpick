# Contributing to logpick

## Start with the Transport interface

The contribution this project expects most is "add a backend for my company's
weird bastion". That is one interface, in `internal/transport`:

```go
type Transport interface {
	Exec(ctx context.Context, cmd string) (*Process, error)
	Fetch(ctx context.Context, remote, local string, prog chan<- Progress) (int64, error)
	Caps() Caps
	Check(ctx context.Context) error
	Close() error
}
```

`Exec` is the only method that has to do real work. Embed `FallbackFetcher`
and you get `Fetch` for free, as `cat` streamed over `Exec`. Override it only
if your wrapper has a native copy command worth using.

Before writing a backend, check whether a config template already covers you.
`[profile.x] exec = [...]` handles most wrappers without any Go at all.

## Rules that are not negotiable

- Never build a shell string and hand it to `sh -c` locally. Templates are
  argv slices.
- Never write to a remote host.
- Never handle credentials. Auth failures surface the profile's `auth_hint`
  and stop.
- Never block the bubbletea `Update` loop on I/O.
- `Caps` must be honest. If a backend cannot do something, say so, and let
  callers check rather than try and catch.

## Verifying

```sh
just build
just test
just race     # not optional, the streaming design is goroutine-heavy
just lint
just run-mock # drive the whole UI against fixtures, no network
```

## Tests

Unit tests only. The mock transport is the seam: nothing above
`internal/transport` spawns a process in a test. UI tests feed a `tea.Msg` to
`Update` and assert on the returned model, never on rendered frames.

Design tests before writing them. List the distinct mechanics of the unit, then
write one test per mechanic. Three to six per unit; if you want more, the unit
is doing too much.

Automated contributors: read `AGENTS.md` first. It is binding.
