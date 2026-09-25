# lazyrecall

A terminal UI for every coding-agent session you have already had.

![lazyrecall](docs/screenshot.png)

You have been pairing with Claude Code. And with pi, and omp, and hermes, sometimes all of them in the same week. Somewhere in there you fixed the flaky retry test, worked out why the migration deadlocks, and wrote the one prompt that finally got the refactor right.

Now find it again. Which agent was it? Which repo? Was that Tuesday or the Tuesday before? Every tool keeps its own history, in its own format, in its own directory, and not one of them has any idea what the other three were doing. So you scroll back through a terminal that no longer has it, or you just do the work twice.

lazyrecall reads what those agents already wrote to your disk and turns it into one list. Every session from every agent, newest first, with the repository it ran in, how it ended, and what it was about. Search the prompts you actually typed. Tag the good ones, leave yourself a comment on the one you will need in a month, and filter down by repo or agent until the list is short.

Then press enter, and you are back in that session - the same agent, the same working directory, right where you left off.

It does not run agents, watch anything, or send your sessions anywhere. It is a reader over files that are already on your disk. Resuming is the only thing that starts a process, and only because you asked it to.

## Install

lazyrecall needs no runtime dependencies: it is a single statically linked binary (`CGO_ENABLED=0`, pure-Go SQLite).

Via the install script (macOS and Linux):

```sh
curl -fsSL https://raw.githubusercontent.com/rfist/lazyrecall/main/install.sh | sh
```

Or build from source:

```sh
git clone https://github.com/rfist/lazyrecall.git
cd lazyrecall
CGO_ENABLED=0 go build -o lazyrecall ./cmd/lazyrecall
```

## Usage

`lazyrecall` with no arguments opens the interactive browser; the same interface is available as `lazyrecall browse`, which falls back to a plain listing when stdout is not a terminal.

```
lazyrecall                  open the interactive browser
lazyrecall list      [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--group=NAME] [--since=DAYS] [--all] [--json]
lazyrecall search    QUERY [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--group=NAME] [--all] [--json]
lazyrecall review    [--group=NAME] [--all] [--json]
lazyrecall resume    [SESSION_ID|PREFIX|.] [--last] [--repo=PATH] [--agent=NAME] [--client=NAME] [--tag=NAME] [--group=NAME]
lazyrecall comment   add SESSION_ID TEXT... | list SESSION_ID | rm COMMENT_ID
lazyrecall tag       add SESSION_ID TAG | rm SESSION_ID TAG | list
lazyrecall archive   SESSION_ID | list
lazyrecall unarchive SESSION_ID
lazyrecall group     SESSION_ID NAME | SESSION_ID archive | SESSION_ID --auto
lazyrecall name      SESSION_ID TEXT... | SESSION_ID --clear
lazyrecall refresh   [--full]
lazyrecall browse    [QUERY] [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--group=NAME] [--all]
lazyrecall groups    [--json]
lazyrecall config    path|init|show
lazyrecall version, --version, -v
```

A session held somewhere other than the agent's own terminal - a Neovim CodeCompanion chat, say, which reaches Claude Code over ACP - is listed as `[claude·acp]` and selected by `--client=acp`.

`--group` narrows to one view of a session's group (see [Groups](#groups) below, and `lazyrecall groups` for the configured names on this machine): a configured group's name, `archive` for every archived session, or `unknown` for sessions no group rule or manual choice has claimed. With no `--group`, a listing shows every non-archived session regardless of group - the same as before groups existed, and the same as it looks with no `[groups.*]` configured at all.

`lazyrecall resume` takes a session two ways beyond the interactive picker. A `SESSION_ID` is a short handle (`3`), a full composite identifier, or an unambiguous prefix of at least 4 characters of either - `9f2a` resumes the same session `claude:cc:9f2a1b3c-...` does, as long as no other session's id or prefix starts the same way; a prefix that matches more than one session prints a short list of candidates instead of guessing, and this applies everywhere a session id is accepted (`comment`, `tag`, `archive`, `unarchive`, `group`, `resume`), not just here. `.` resumes the most recently active session in the current directory's own repository directly, with no picker - matched the same literal way `--repo=PATH` is, against a session's own recorded repository or working directory, never a path lazyrecall resolves or expands itself. `--last` resumes the most recently active session of the current listing directly, with no picker either; combined with `--repo=PATH` (and `--agent`/`--client`/`--tag`/`--group`), it resumes the newest session matching those filters. With no `SESSION_ID` and no `--last`, `lazyrecall resume` opens the numbered picker, narrowed by whichever of those filters were given.

`lazyrecall name` sets lazyrecall's own name for a session, held separately from anything a source tool records - Claude Code's own rename (`sessions.name`) is untouched and still shown alongside it when the two differ. It is an annotation like a tag or a comment: it lives on the durable lineage, so it survives an index rebuild and moves with a session that continues under a new identifier. Once set, it takes over the row and Detail tab's name slot, ahead of the source-recorded name and the topic. `lazyrecall name SESSION_ID --clear` removes it, and the browser's `r` key does the same thing interactively (see [Key bindings](#key-bindings) below).

A session carrying comments shows a small `✎N` marker on its row (e.g. `✎2` for two comments) - a hint that there is more to read on the Detail or Comments tab. On a row too narrow for everything, the topic is shortened first to keep it; the marker only gives way once keeping it would leave too little of the topic to recognize the session by.

## Key bindings

| Key | Action |
| --- | --- |
| `1`-`4`, `0` | Jump to panel (`1`-`4` are Groups, Agents, Repos, Tags; `0` is Sessions) |
| `H` / `J` / `K` / `L` | Move focus to the panel in that screen direction |
| `tab` / `shift-tab` | Focus the next / previous panel |
| `j`/`k`, `↑`/`↓` | Move within the focused panel |
| `Ctrl-D` / `Ctrl-U` | Move by half a panel |
| `g` / `G` | First / last row |
| `enter` | Resume the selected session (Sessions); filter by the selected value (Groups, Agents, Repos, Tags) |
| `esc` | Clear what this panel is filtering by |
| `[` / `]` | Previous / next tab in the detail pane (Detail, Prompts, Transcript, Comments) |
| `n` / `N` | On the Transcript tab, scroll to the next / previous occurrence of the search phrase |
| `t` | On the Transcript tab, toggle between the clean question/answer view and the full technical transcript |
| `d` | Take the tag under the cursor off the selected session (Tags panel) |
| `/` | Keep only the focused panel's rows containing what you type |
| `s` | Full-text search over your own prompts |
| `x` | Action menu for the focused panel (`j`/`k` or `↑`/`↓` move, `enter` applies, `esc` closes, `/` narrows by typing) |
| `p` | File the selected session into a group, archive it, or return it to automatic (same `j`/`k`, `enter`, `esc`, `/` as the action menu) |
| `X` | Clear all filters, including the selected group (back to All) |
| `R` | Refresh the index |
| `m` / `M` | Add / remove a tag on the selected session (`d` in the Tags panel is usually easier) |
| `c` / `C` | Add / remove a comment on the selected session |
| `r` | Set or clear lazyrecall's own name for the selected session (blank input clears it) |
| `a` | Archive / unarchive the selected session |
| `.` | Toggle showing sessions the hide rules and the archive flag suppress |
| `?` | Show this list |
| `q`, `Ctrl-C` | Quit |

The Sessions panel groups its rows under dim separators - `── Today ──`, `── Yesterday ──`, `── Last 7 days ──`, `── Last 30 days ──`, `── Older ──`, and `── Unknown date ──` for a session with no recorded activity time - based on each session's last activity in local time, with a separator only for a bucket that actually has a session in it. They only ever appear while the list is sorted newest-first, which is every ordinary view (the plain listing, the `/` row filter, and `s` full-text search all keep that order); moving, resuming, and everything else about the list is unaffected either way, since the separators are not rows you can select. Turn them off with `browse.date_headers = false` in the config file (see [Configuration](#configuration)).

## The detail pane

The right-hand pane has four tabs, reached with `[` and `]`.

**Detail** is the session's metadata: its identifier and handle, the agent and the client it was driven through, the install it ran under, working directory and branch, end state, last activity, name, topic, group, tags, and a preview of the most recent comments. The install line names the account, e.g. `install: ccp (~/.claude-personal)`. The group line appears only when groups are configured (see [Groups](#groups) below) and says why the session landed where it did: `group: work (set manually)`, `group: work (path ~/code)`, or `group: unknown`. The name line shows the effective name - lazyrecall's own (`r`, or `lazyrecall name`) when one is set, otherwise the name a source tool recorded; when both exist and differ, the source-recorded one gets its own `agent name:` line underneath so you can see what a session was renamed away from. The comments section shows the 3 most recent comments in the same format as the Comments tab, `(none)` when there are none, and `(+N more on the Comments tab)` when there are more than 3 - the Comments tab itself always shows every one of them.

**Prompts** is what you actually typed in that session, which is usually the only part of it you remember.

**Transcript** is the conversation itself - your turns, the agent's replies, the tools it called, and any compaction boundaries - read when you select the session. It exists so you can tell whether a session is the one you meant *before* resuming it, since resuming takes over the terminal and moves you into the session's working directory. The end of the conversation is what it keeps: how a session started is already answered by Prompts, and what you need before resuming is where it was left. When there is a search phrase in play, every occurrence is highlighted and `n` / `N` step through them. The tab opens in **clean** mode by default (`browse.transcript` below): tool calls and compaction boundaries are hidden, and the replies left adjacent once they are gone are merged into one block, so a session reads as question/answer instead of interleaved with everything it did along the way. Press `t` to switch to **full** mode, which shows every turn exactly as it always has; the tab's top border always names the active mode and the key that changes it. The mode you land on persists for as long as the browser stays open, across every session you look at.

It works for every source but one, from whichever place that source keeps its conversation. `claude`, `pi` and `omp` write transcript files, which are read directly. `hermes`, `goose`, `opencode` and `kilo` write no files at all, but the conversation is in the same database lazyrecall already reads for the session list, so it is read back from there - no agent is ever invoked to fetch it. `antigravity` is the exception: its per-conversation detail is protobuf with no available schema, so the tab says the format cannot be decoded rather than reporting an error. A transcript file the agent has since cleaned up is reported in the same spirit.

**Comments** is your own freeform notes on the session, with the ids `comment rm` takes.

## Supported sources

| Agent | Where it stores sessions | Status |
| --- | --- | --- |
| `claude` (Claude Code) | `$CLAUDE_CONFIG_DIR/history.jsonl` plus transcripts under `projects/` | primary |
| `pi` | Transcripts under `~/.pi/agent/sessions/` | niche |
| `omp` | SQLite index at `~/.omp/agent/history.db`, transcripts under `~/.omp/agent/sessions/` | niche |
| `hermes` | SQLite at `~/.hermes/state.db` | niche |
| `goose` (Block's Goose) | SQLite at `~/.local/share/goose/sessions/sessions.db` | niche |
| `opencode` | SQLite at `~/.local/share/opencode/opencode.db` | niche |
| `kilo` | SQLite at `~/.local/share/kilo/kilo.db` | niche |
| `antigravity` (Antigravity CLI, `agy`) | SQLite at `~/.gemini/antigravity-cli/conversation_summaries.db`; listing only, not full-text searchable - see note below | niche |

Plainly: if you use Claude Code, `claude` is the one you will actually have; the rest are niche tools most people will not have installed at all. Each source is a set of roots to scan plus an adapter; new sources are added via the `[sources]` config table (see [Configuration](#configuration)) and [an adapter in the code](docs/adding-a-source.md).

`goose`, `opencode`, `kilo`, and `antigravity` keep everything in one SQLite database rather than per-session transcript files, the same shape `hermes` already used - so they need no transcript parser, just a query. `kilo` ships `opencode`'s schema and `--session` flag verbatim under its own name, so one adapter serves both. `antigravity` is the exception worth knowing about: its per-conversation detail is stored as protobuf with no available schema to decode, so its sessions show up with a topic, working directory, and end time like everything else, but `s` (full-text prompt search) will never find anything inside them - only their title/preview, which `/` (the row filter) already covers.

## Configuration

Configuration is optional and lives at `~/.config/lazyrecall/config.toml` (`$LAZYRECALL_CONFIG` overrides the location; otherwise `$XDG_CONFIG_HOME/lazyrecall/config.toml` is used). With no file, lazyrecall runs on the defaults below.

| Key | Default |
| --- | --- |
| `sources.<agent>.roots` | `claude`: `["~/.claude-personal", "~/.claude"]`, `pi`: `["~/.pi"]`, `omp`: `["~/.omp"]`, `hermes`: `["~/.hermes"]`, `goose`: `["~/.local/share/goose/sessions"]`, `opencode`: `["~/.local/share/opencode"]`, `kilo`: `["~/.local/share/kilo"]`, `antigravity`: `["~/.gemini/antigravity-cli"]` |
| `sources.<agent>.resume` | `claude --resume {id}`, `pi --session {id}`, `omp --resume {id}`, `hermes --resume {id}`, `goose session --resume --session-id {id}`, `opencode --session {id}`, `kilo --session {id}`, `agy --conversation {id}` |
| `sources.<agent>.env_var` | `claude`: `CLAUDE_CONFIG_DIR`; the rest: none |
| `sources.<agent>.single_install` | everything but `claude`: `true` |
| `labels` | (none; an install's label falls back to its own name) |
| `groups.<name>.paths` | (none configured; see [Groups](#groups) below) |
| `groups.<name>.color` | (none configured; see [Groups](#groups) below) |
| `groups.archive.color` | (none configured; see [Groups](#groups) below) |
| `groups.unknown.color` | (none configured; see [Groups](#groups) below) |
| `hide.non_interactive` | `true` |
| `hide.min_messages` | `0` |
| `hide.paths` | `[]` |
| `browse.show_archived` | `false` |
| `browse.default_group` | `""` (All); also accepts `all`, `archive`, `unknown`, or a configured group's name - anything else is a config error naming the file and the bad value |
| `browse.transcript` | `"clean"`; also accepts `"full"` - anything else is a config error naming the file and the bad value |
| `browse.date_headers` | `true` |

`resume` is an argv template in which `{id}` is replaced with the session id; `env_var` names the environment variable set to the install's root when resuming; `single_install` marks a source that can only ever have one install, which is then named after the source itself (`omp`, `pi`, ...), while a source that can have several installs (`claude`) names each one after its root directory (`claude`, `claude-personal`).

`lazyrecall config init` writes a commented-out copy of these defaults, and `lazyrecall config show` prints the effective config with each value's provenance (default, file, env, or flag).

Environment variables: `LAZYRECALL_CONFIG` (config file path) and `LAZYRECALL_HOME` (data directory, default `~/.lazyrecall`).

## Groups

lazyrecall keeps one local index, `~/.lazyrecall/index.db`, over every install it discovers - there is no more one database per profile. Every session still belongs to exactly one **install** (the config root that produced it - `~/.claude`, `~/.claude-personal`, `~/.omp`, and so on), and which install ran a session stays visible and filterable, but it no longer decides where that session's data lives or which sessions you see together.

Instead, a session belongs to a **group**, a view computed each time you query, the same way the existing hide rules are: editing the config regroups every session instantly, with no refresh needed. A session's group is decided in this order:

1. a manual override you set on the session (`p` in the browser, or `lazyrecall group SESSION NAME`) - this sticks even across an index rebuild, and moves with a session when it continues under a new identifier
2. otherwise, the configured group whose path is the longest prefix of the session's working directory
3. otherwise, **Unknown**

**Archive** is unchanged: archiving a session (`a`, or `lazyrecall archive`) hides it from every group's listing until you ask to see everything, and it remembers which group to return to if you unarchive it.

`browse.default_group` sets which group the browser opens on when `--group` is not given on the command line. It is validated the same way `--group` is: `""` or `all` (both mean All), `archive`, `unknown`, or a name declared under `[groups.*]` in the same file - anything else fails to load with a config error naming the file and the offending value, rather than silently opening on an empty view.

The Groups panel's counts are filter-aware, the same way the Agents/Repos/Tags panels already are: they reflect whatever agent, repo, tag, or text (`/` or `s`) filter is currently applied, not the whole index, and a group with no matching sessions under the current filter is not shown at all - unless it is the group you have selected, which always stays visible (at its true, possibly zero, count) so you can see and clear it.

With no `[groups.*]` configured at all, lazyrecall looks almost exactly as it always has: no Groups panel beyond All (plus Archive when something is archived), no group line in the detail pane. The one difference is the handle's color: it is white by default now, rather than the plain cyan every handle used before groups had colors of their own.

Labels are the replacement for what profiles used to do: giving an account a short, filterable name without deciding anything about grouping. Configure them in a top-level `[labels]` table, keyed by config root:

```toml
[groups.work]
paths = ["~/code", "~/work"]
color = "blue"

[groups.personal]
paths = ["~/dotfiles", "~/personal"]
color = "#22aa88"

[labels]
"~/.claude" = "cc"
"~/.claude-personal" = "ccp"
```

With that config, a work-account Claude session that happens to run under `~/personal/scratch` still shows up under the `personal` group (and can be moved with `p` if that's wrong), while `--agent=cc` or `--agent=ccp` narrows to one account regardless of which group its sessions are filed under. `--agent=claude` still means every Claude account, as it always has; a label only adds a way to name one of them.

`--agent` (and the browser's `--agent` seed) resolves a value in this order, first match wins: a name actually configured in `[labels]` narrows to that one install; otherwise a configured source name (`claude`, `pi`, ...) narrows to every install of that source; otherwise a discovered install's own name narrows to that one install; otherwise the value matches nothing. This holds with or without `[labels]` configured - in particular, `--agent=claude` always means every Claude install, even when one of your installs happens to be named exactly `claude` (the multi-install default when its root is `~/.claude`) and no labels are set up at all.

A group's `color` is optional. It accepts an ANSI color name (`black`, `red`, `green`, `yellow`, `blue`, `magenta`, `cyan`, `white`), a `bright-`-prefixed variant of one of those (e.g. `bright-blue`), or a 24-bit hex triplet like `#3355ff` - case-insensitive either way. It is drawn on the group's own name in the Groups panel, and on the handle (`#904`) of every session filed under it, in the browser and in `lazyrecall list`'s plain-text output alike. A session's handle is white by default; a session with no group at all uses this white default too, unless `unknown`'s own color (below) applies.

Archive and Unknown - the two built-in views alongside a session's real group - can be colored the same way, even though `archive` and `unknown` stay reserved and can never be a real group's name: a `[groups.archive]` or `[groups.unknown]` table may set `color` and nothing else (`paths` there is a config error, since neither is a place a session is ever filed by working directory). Archived colors the handle of every archived session and the Archive row's own label in the Groups panel; unknown does the same for a session with no group and for the Unknown row. When a session is both archived and would otherwise carry a group's color, archive wins; a session that does have a configured group but that group has no color of its own stays white rather than picking up the unknown color, since it isn't actually groupless.

```toml
[groups.archive]
color = "red"

[groups.unknown]
color = "white"
```

`[labels]` has to be its own top-level table, not nested under `[sources.claude]`: a `[sources.X]` table in the file replaces that source's whole configuration, roots included, so writing labels there would silently drop every claude root from discovery the moment you configured one.

### Strict isolation

If you need a hard guarantee that two sets of sessions can never appear together, even by mistake, run lazyrecall twice with entirely separate config and data - but each config file has to actually narrow which sources it reads, since a `[sources.X]` table replaces that source's defaults, not just adds to them, and any source a config does not narrow or switch off is discovered and read by both indexes. A work config, for example:

```toml
[sources.claude]
roots = ["~/.claude"]
resume = ["claude", "--resume", "{id}"]
env_var = "CLAUDE_CONFIG_DIR"

[sources.omp]
roots = []
```

restricts discovery to the `~/.claude` install and switches `omp` off entirely (an empty `roots` list is how a source is turned off); do the same for every other source you don't want this config to see. A personal config mirrors it, with `roots = ["~/.claude-personal"]` under `[sources.claude]` and whichever other sources it should read left in. Then point each invocation at its own config file and its own data directory:

```sh
LAZYRECALL_CONFIG=~/.config/lazyrecall-work/config.toml LAZYRECALL_HOME=~/.lazyrecall-work lazyrecall
LAZYRECALL_CONFIG=~/.config/lazyrecall-personal/config.toml LAZYRECALL_HOME=~/.lazyrecall-personal lazyrecall
```

Each pair of environment variables gives that invocation its own `index.db`, so nothing either one indexes is visible to the other - but only for the sources each config file actually narrowed or switched off.

## License

MIT — see [LICENSE](LICENSE).
