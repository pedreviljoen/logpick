# logpick

A terminal tool for finding, reading and pulling logs off remote hosts.

Pick a host, pick a log, read it, pull it down.

```
┌─ jenkins-01.prod.internal ──────────── corp ─ 34 logs ─ scanned 2s ago ─┐
│ > jenk                     │ 2026-08-14 16:02:11 INFO  Started build   │
│                            │ 2026-08-14 16:02:11 INFO  Fetching from   │
│ jenkins.log      84M  2m   │ 2026-08-14 16:02:12 WARN  Retry 1 of 3    │
│ jenkins.log.1    100M 1d   │ 2026-08-14 16:02:14 ERROR Connection rese │
│ gc.log           12M  2m   │ ...                                       │
├────────────────────────────┴───────────────────────────────────────────┤
│ enter open  f fetch  F follow  / filter  g top  G bottom  ? help       │
└────────────────────────────────────────────────────────────────────────┘
```

## It holds no secrets

logpick does not authenticate. It has no password prompt, no key parsing, no
token storage and no credential cache. Every remote operation is a command run
through the SSH wrapper you already use, so auth stays entirely with that
wrapper or your agent. When it fails, logpick prints the hint you configured
and stops.

It is also read only on the remote. No command in the codebase writes, moves
or deletes anything on a host.

## Status

Under construction. See `agents/DESIGN.md` for the design and
`CONTRIBUTING.md` to add a backend.
