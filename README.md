# logpick

A terminal tool for finding, reading and pulling logs off remote hosts.

Pick a host, pick a log, read it, pull it down. There is no agent to install
on the far side and no credentials to hand over: every remote operation is a
command run through the SSH wrapper you already use.

```
logpick  /  browser
╭─────────────────────────────────────╮ ╭──────────────────────────────────────────────────────────╮
│ Logs — jenkins-01.prod.internal     │ │ Preview — /var/log/jenkins/jenkins.log                   │
│ 4 files                             │ │                                                          │
│                                     │ │ 2026-08-14 16:02:11 INFO  Started build #4812            │
│ >                                   │ │ 2026-08-14 16:02:11 INFO  Fetching from origin/main      │
│ > /var/log/jenkins/jenkins.log      │ │ 2026-08-14 16:02:12 WARN  Retry 1 of 3                   │
│   /var/log/jenkins/jenkins.log.1    │ │ 2026-08-14 16:02:14 ERROR Connection reset by peer       │
│   /var/log/jenkins/gc.log           │ │ 2026-08-14 16:02:18 INFO  Recovered, resuming            │
│   /var/log/syslog                   │ │                                                          │
│ space select • ctrl+s find path • … │ │                                                          │
│                                     │ │                                                          │
│                                     │ │ tab files • ↑/↓/pgup/pgdn scroll • esc hosts             │
╰─────────────────────────────────────╯ ╰──────────────────────────────────────────────────────────╯
```

## Install

Go 1.25 or newer:

```sh
go install github.com/pedreviljoen/logpick/cmd/logpick@latest
```

That puts `logpick` in `$(go env GOPATH)/bin`; add it to your `PATH` if it is
not there already. To build from a checkout instead:

```sh
git clone https://github.com/pedreviljoen/logpick
cd logpick
go build ./cmd/logpick      
```

## First run

Run `logpick` with no arguments. When no host history exists yet, it opens a
connection form:

- **Host** — include the remote user when needed, for example
  `ec2-user@ec2-203-0-113-10.compute.amazonaws.com` or `ubuntu@host`.
- **Identity file** — optional private key path passed to the command as `-i`.
  `~` is expanded and only the path is saved; logpick never reads or stores the
  key contents.
- **Command** — defaults to `ssh`. Arguments are supported. Custom wrappers may
  include `{host}` and `{cmd}` placeholders; if omitted, they are appended.
- **Persistent session** — a checkbox (toggle with `Space`). Turn it on for a
  wrapper that takes a host and *no* remote command, such as Amazon's
  `ec2-ssh`. logpick then opens the wrapper once and runs commands through the
  shell it drops you into, instead of appending a command the wrapper would
  reject. Leave it off for `ssh` and any wrapper that forwards a trailing
  command; those get the usual `{host} -- {cmd}` template.

Use `Tab`/`Shift+Tab` to move between fields, `Space` to toggle the persistent
checkbox and `Enter` to connect. The identity file is optional: leave it blank
to use your SSH agent or existing SSH configuration. The profile is written to
`~/.config/logpick/config.toml` only after the connection probe succeeds.

After that, `logpick` opens on your saved hosts, and `logpick <host>` skips the
list and connects straight away.

## Keys

Everything is one keystroke from the file list. Typing anything else filters.

| Key | Where | Action |
| --- | --- | --- |
| `enter` | hosts | connect to the highlighted host |
| `ctrl+n` / `ctrl+d` / `ctrl+p` | hosts | new host, delete, pin |
| `ctrl+t` | hosts | live theme editor |
| type | browser | fuzzy-filter the file list |
| `↑` `↓` (or `ctrl+p` `ctrl+n`) | browser | move the highlight |
| `enter` or `space` | browser | fetch the log locally, or open a directory |
| `ctrl+s` | browser | list a remote path discovery missed |
| `tab` | browser | move focus between the file list and the pane |
| `/` `n` `N` | pane | search the fetched log, next match, previous match |
| `esc` | anywhere | back one screen, or dismiss an error |
| `F1` | anywhere | key map overlay |
| `ctrl+c` | anywhere | quit |

## When discovery misses a log

Discovery only looks where the profile tells it to, and only at names the
profile matches — which is exactly why the log you want is sometimes not in
the list. `Ctrl+S` is the way out. Type any remote path and logpick lists
**everything** under it: every file, every directory, with the profile's
`include` and `exclude` patterns ignored, because those patterns are what hid
the file in the first place.

```
logpick  /  browser
╭─────────────────────────────────────╮ ╭──────────────────────────────────────────────────────────╮
│ Browsing — /opt/app                 │ │ Directory — /opt/app/                                    │
│ 3 files, 4 directories — type to f… │ │                                                          │
│                                     │ │ Directory — press enter to list what is inside it.       │
│ >                                   │ │                                                          │
│ > /opt/app/                         │ │                                                          │
│   /opt/app/conf/                    │ │                                                          │
│   /opt/app/conf/app.yaml            │ │                                                          │
│   /opt/app/logs/                    │ │                                                          │
│   /opt/app/logs/worker.err          │ │                                                          │
│   /opt/app/logs/worker.out          │ │                                                          │
│   /opt/app/logs/archive/            │ │                                                          │
│ enter open • space select • ctrl+s… │ │                                                          │
╰─────────────────────────────────────╯ ╰──────────────────────────────────────────────────────────╯
```

From there it behaves like any other fuzzy finder:

- Type to filter the whole listing by any part of the path.
- Directories are shown with a trailing `/`. `Enter` on one lists that
  directory, so you can walk down a tree you do not know by heart.
- `Enter` on a file fetches it and opens it in the pane, the same as in a
  configured scan.
- `Ctrl+S` again is prefilled with the path you are on, so editing it by hand
  is a small edit rather than retyping.

Globs work too, so `/srv/*/logs` is a valid thing to type. A listing is
capped at 10,000 entries and never touches `config.toml` — it lasts as long
as you are looking at it. If a path turns out to be right, add it to the
profile's `paths` to have discovery find it next time.


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

## It holds no secrets

logpick does not authenticate. It has no password prompt, no key parsing, no
token storage and no credential cache. Every remote operation is a command run
through the SSH wrapper you already use, so auth stays entirely with that
wrapper or your agent. When it fails, logpick prints the hint you configured
and stops.

It is also read only on the remote. No command in the codebase writes, moves
or deletes anything on a host.

## Status

Under construction. Working today: the host list and first-run flow, discovery
and the browser, remote previews, local fetch and search, one-off path
listings, and the theme editor. The library and viewer screens are built but
not yet reachable from a key, and follow mode is wired end to end but not yet
bound to one.

