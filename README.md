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
│ enter open  f fetch  F follow  / filter  g top  G bottom  F1 help      │
└────────────────────────────────────────────────────────────────────────┘
```

## First run

When no host history exists, logpick opens a connection form in the terminal:

- **Host** — include the remote user when needed, for example
  `ec2-user@ec2-203-0-113-10.compute.amazonaws.com` or `ubuntu@host`.
- **Identity file** — optional private key path passed to the command as `-i`.
  `~` is expanded and only the path is saved; logpick never reads or stores the
  key contents.
- **Command** — defaults to `ssh`. Arguments are supported. Custom wrappers may
  include `{host}` and `{cmd}` placeholders; if omitted, they are appended.

Use `Tab`/`Shift+Tab` to move between fields and `Enter` to connect. The
identity file is optional: leave it blank to use your SSH agent or existing SSH
configuration. The profile is written to `~/.config/logpick/config.toml` only
after the connection probe succeeds.

## Theme

From the saved-host screen, press `Ctrl+T` to open the live theme editor. Enter
`#RRGGBB` values for the primary color (titles, active borders and focus) and
secondary color (warnings, matches and highlights). The preview changes while
you type. `Enter` saves the palette to tool-owned state, while `Esc` restores
the previous palette.

The same defaults can be supplied in config:

```toml
[theme]
primary = "#7aa2f7"
secondary = "#e0af68"
```

## Selecting and searching a log

In the browser, press `Space` on a log to fetch a local snapshot and commit it
to the right pane. Press `/` there to search: results update on every keystroke
and unmatched source lines disappear immediately. `Enter` keeps the current
filter, `Esc` cancels it and restores the complete snapshot, and `n`/`N` step
through matches.

Search is local by design. The remote preview remains a debounced `tail`; a
selected snapshot is stored under `${XDG_DATA_HOME}/logpick/fetched` and searched
with ripgrep when available or the native scanner otherwise.

If discovery did not include the expected log, press `Ctrl+S` in the browser and
enter a one-off remote directory or glob such as `/opt/app/logs` or
`/srv/*/logs`. The browser replaces the list with that remote scan's results
without modifying `config.toml`.

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
