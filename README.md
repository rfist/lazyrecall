# lazyrecall

lazyrecall is a retrospective index over the sessions coding agents already
wrote to disk. It reads the transcripts and history files that are already
there, makes them searchable, lets you annotate them with tags and comments,
and can drop you back into any of them with the original agent. It is not a
live monitor and does not run agents on its own; resuming a session is the one
deliberate, user-initiated exception, and it happens in your terminal.

## Screenshot

![lazyrecall](docs/screenshot.png)

## Install

lazyrecall needs no runtime dependencies: it is a single statically linked
binary (`CGO_ENABLED=0`, pure-Go SQLite).

Via the install script (macOS and Linux):

```sh
curl -fsSL https://raw.githubusercontent.com/OWNER/lazyrecall/main/install.sh | sh
```

Or build from source:

```sh
git clone https://github.com/OWNER/lazyrecall.git
cd lazyrecall
CGO_ENABLED=0 go build -o lazyrecall ./cmd/lazyrecall
```

## Usage

`lazyrecall` with no arguments opens the interactive browser; the same
interface is available as `lazyrecall browse`, which falls back to a plain
listing when stdout is not a terminal.

```
lazyrecall                  open the interactive browser
lazyrecall list      [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--since=DAYS] [--json] [--profile=NAME]
lazyrecall search    QUERY [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--json] [--profile=NAME]
lazyrecall review    [--json] [--profile=NAME]
lazyrecall resume    [SESSION_ID] [--profile=NAME]
lazyrecall comment   add SESSION_ID TEXT... | list SESSION_ID | rm COMMENT_ID
lazyrecall tag       add SESSION_ID TAG | rm SESSION_ID TAG | list
lazyrecall archive   SESSION_ID | list
lazyrecall unarchive SESSION_ID
lazyrecall refresh   [--full] [--profile=NAME]
lazyrecall browse    [QUERY] [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--profile=NAME]
lazyrecall profiles  [--json]
lazyrecall config    path|init|show
lazyrecall version, --version, -v
```

A session held somewhere other than the agent's own terminal - a Neovim
CodeCompanion chat, say, which reaches Claude Code over ACP - is listed as
`[claude·acp]` and selected by `--client=acp`.

## Key bindings

| Key | Action |
| --- | --- |
| `1`-`5` | Jump to panel |
| `tab` / `shift-tab` | Focus the next / previous panel |
| `j`/`k`, `↑`/`↓` | Move within the focused panel |
| `Ctrl-D` / `Ctrl-U` | Move by half a panel |
| `g` / `G` | First / last row |
| `enter` | Resume the selected session (Sessions); filter by the selected value (Agents, Repos, Tags); switch to the selected profile (Profiles) |
| `esc` | Clear what this panel is filtering by |
| `[` / `]` | Previous / next tab in the detail pane |
| `/` | Narrow the focused panel's rows as you type |
| `s` | Full-text search over your own prompts |
| `x` | Action menu for the focused panel |
| `X` | Clear all filters |
| `R` | Refresh the index |
| `m` / `M` | Add / remove a tag on the selected session |
| `c` / `C` | Add / remove a comment on the selected session |
| `?` | Show this list |
| `q`, `Ctrl-C` | Quit |

## Supported sources

| Agent | Where it stores sessions | Status |
| --- | --- | --- |
| `claude` (Claude Code) | `$CLAUDE_CONFIG_DIR/history.jsonl` plus transcripts under `projects/` | primary |
| `pi` | Transcripts under `~/.pi/agent/sessions/` | niche |
| `omp` | SQLite index at `~/.omp/agent/history.db`, transcripts under `~/.omp/agent/sessions/` | niche |
| `hermes` | SQLite at `~/.hermes/state.db` | niche |

Plainly: if you use Claude Code, `claude` is the one you will actually have;
`pi`, `omp`, and `hermes` are niche tools most people will not have installed
at all. Each source is a set of roots to scan plus an adapter; new sources are
added via the `[sources]` config table (see [Configuration](#configuration))
and an adapter in the code.

## Configuration

Configuration is optional and lives at `~/.config/lazyrecall/config.toml`
(`$LAZYRECALL_CONFIG` overrides the location; otherwise
`$XDG_CONFIG_HOME/lazyrecall/config.toml` is used). With no file, lazyrecall
runs on the defaults below.

| Key | Default |
| --- | --- |
| `default_profile` | (none; the program applies its own rule) |
| `sources.<agent>.roots` | `claude`: `["~/.claude-personal", "~/.claude"]`, `pi`: `["~/.pi"]`, `omp`: `["~/.omp"]`, `hermes`: `["~/.hermes"]` |
| `sources.<agent>.resume` | `claude --resume {id}`, `pi --session {id}`, `omp --resume {id}`, `hermes --resume {id}` |
| `sources.<agent>.env_var` | `claude`: `CLAUDE_CONFIG_DIR`; the rest: none |
| `sources.<agent>.single_install` | `pi`, `omp`, `hermes`: `true`; `claude`: `false` |
| `hide.non_interactive` | `true` |
| `hide.min_messages` | `0` |
| `hide.paths` | `[]` |
| `browse.show_archived` | `false` |

`resume` is an argv template in which `{id}` is replaced with the session id;
`env_var` names the environment variable set to the profile's root when
resuming; `single_install` marks a source that has exactly one installation
per machine and so cannot be split work/personal.

`lazyrecall config init` writes a commented-out copy of these defaults, and
`lazyrecall config show` prints the effective config with each value's
provenance (default, file, env, or flag).

Environment variables: `LAZYRECALL_CONFIG` (config file path),
`LAZYRECALL_HOME` (data directory, default `~/.lazyrecall`), and
`LAZYRECALL_PROFILE` (default profile).

## Profiles

Profiles isolate configuration roots from each other: each discovered root
(for example a work and a personal Claude install) becomes its own profile
with its own database. Work and personal data never mix in one listing.

## License

MIT — see [LICENSE](LICENSE).
