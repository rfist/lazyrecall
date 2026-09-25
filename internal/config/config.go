// Package config loads LazyRecall's TOML configuration and remembers where
// every value came from.
//
// Configuration is optional: the built-in defaults reproduce exactly what the
// program does with no file present, so no caller has to branch on absence.
// A file that exists but cannot be parsed is a hard error, never a silent
// fallback to defaults - running a configuration the user did not write is
// worse than refusing to start.
//
// Precedence is flag > env > file > default, maintained by call order: Load
// applies the file layer and Set the flag layer, so callers that apply flag
// overrides must call Set last.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Origin records where a configuration value came from so a later package can
// print the effective config and attribute each value to the layer that set
// it, instead of forcing the reader to reverse-engineer the precedence chain.
type Origin string

const (
	OriginDefault Origin = "default"
	OriginFile    Origin = "file"
	OriginEnv     Origin = "env"
	OriginFlag    Origin = "flag"
)

// Source configures one agent LazyRecall knows: where its config roots live,
// how a session in it is resumed, and which env var to point at the active
// root.
type Source struct {
	Roots  []string `toml:"roots"`
	Resume []string `toml:"resume"`  // argv template; "{id}" is replaced with the session id
	EnvVar string   `toml:"env_var"` // env var to set to the profile's root when resuming

	// SingleInstall marks a source that has exactly one installation per
	// machine and so cannot be split work/personal. Such a source is
	// bundled into one primary profile instead of naming a profile of its
	// own (see internal/profile.Discover).
	//
	// It is stated rather than inferred from the number of configured
	// roots, because a profile's name is its database filename: inferring
	// it would let a user change which database they are using - and
	// silently orphan every handle, comment, and tag in the old one - just
	// by narrowing a roots list.
	SingleInstall bool `toml:"single_install"`
}

// Hide is the standing rules that keep noise out of a listing. They are
// applied at query time, never at index time: a hidden session is still
// indexed, still searchable with --all, and stops being hidden the moment
// the rule changes. Nothing is discarded on the strength of a rule.
type Hide struct {
	NonInteractive bool     `toml:"non_interactive"`
	MinMessages    int      `toml:"min_messages"`
	Paths          []string `toml:"paths"`
}

// Browse configures the interactive browse command.
type Browse struct {
	ShowArchived bool `toml:"show_archived"`

	// DefaultGroup is the group shown when browse starts with nothing else
	// requested. Empty means All. It names a group by its configured name,
	// not a path - the paths behind a name can change without this value
	// going stale.
	DefaultGroup string `toml:"default_group"`

	// Transcript is which rendering the Transcript tab opens in: "clean"
	// (the default) hides KindToolUse and KindCompactionBoundary turns and
	// merges the runs of KindAssistantText turns that removing them leaves
	// adjacent into one block, so a session reads as question/answer
	// instead of interleaved with every tool call; "full" renders every
	// turn Conversation kept, exactly as the tab has always looked (change
	// clean-transcript-mode). The `t` key flips between them for the
	// running browser - this only decides what a freshly opened one starts
	// on.
	Transcript string `toml:"transcript"`
	// DateHeaders turns the Sessions panel's date separator rows ("── Today
	// ──", "── Yesterday ──", ...) on or off. It defaults to true, unlike
	// every other Browse field, which is why Load checks md.IsDefined rather
	// than trusting the decoded bool: a file that never mentions
	// `date_headers` must still end up true, and a decoded zero value (false)
	// cannot be told apart from an explicit `date_headers = false` on its
	// own.
	DateHeaders bool `toml:"date_headers"`
}

// Group is one configured way of sorting sessions by working directory,
// e.g. "work" claiming every session under ~/code and ~/work. Groups are
// resolved at query time, the same way hide.paths is (internal/search):
// editing the config regroups every session immediately, with no refresh
// and no rewrite of anything already indexed.
//
// A session's group is, in order: a manual override recorded on its
// lineage (so it survives a rebuild and moves with a continuation, like
// archived_at does), else the configured group whose path is the longest
// prefix of the session's cwd, else Unknown. With no groups configured at
// all, every session is Unknown and the tool looks exactly as it did
// before groups existed - no group column, no Groups panel beyond All.
type Group struct {
	Name  string
	Paths []string

	// Color is this group's optional display color, normalized (lowercased
	// name, or lowercased "#rrggbb" hex) by validateGroupColor at Load -
	// never the raw ANSI escape sequence, so config show and any other
	// consumer of Config prints something a human wrote, not terminal
	// control bytes. "" means no color configured: the group's name and any
	// session filed under it keep today's styling. Turning this into an
	// actual escape sequence is internal/cli's job (GroupColors), not
	// config's - this package has no notion of terminal rendering.
	Color string
}

// Config is the effective configuration: defaults overlaid by the file, then
// by environment, then by flags.
type Config struct {
	Sources map[string]Source `toml:"sources"`
	Hide    Hide              `toml:"hide"`
	Browse  Browse            `toml:"browse"`

	// Groups is ordered, not a map: a session that matches more than one
	// group's paths is resolved by longest-prefix match regardless of
	// order (internal/search), but the Groups panel and the `p` popup list
	// groups in the order they were declared in the config file, because
	// that is the only order a user actually chose. The order is recovered
	// from toml.MetaData.Keys() in Load, since Go map iteration order
	// would otherwise be random.
	Groups []Group

	// Labels names the display label for a discovered install's config
	// root, keyed by the root path (expanded and filepath.Clean-ed, like
	// Groups' paths): {"~/.claude": "cc", "~/.claude-personal": "ccp"}.
	// This is what lets an account stay visible and filterable (change
	// group-sessions-in-one-index) without it deciding a session's group
	// the way the old one-database-per-profile split did - a root still
	// identifies which install ran a session, but grouping is a separate,
	// query-time decision (see Group).
	//
	// It is a top-level [labels] table, not nested under [sources.<name>]
	// (follow-up fix to the change above): a [sources.X] table in the file
	// replaces that source's whole configuration, roots included - so a
	// user writing `[sources.claude]\nlabels = {...}` to label their claude
	// installs, the obvious place to reach for, would silently replace
	// Roots with the zero value and make every claude install vanish from
	// discovery. A root with no configured label falls back to the
	// install's own name (internal/profile.Profile.Label).
	Labels map[string]string

	// ArchiveColor and UnknownColor are the optional display colors for the
	// two built-in session views that are not real groups (change
	// archive-unknown-colors): the sessions a user archived, and the
	// sessions that match no configured group's paths. "archive" and
	// "unknown" stay reserved and unusable as a real [groups.<name>] name
	// (see reservedGroupNames) - a session's actual group is never one of
	// these - but their [groups.archive] / [groups.unknown] tables are
	// still allowed, restricted to setting only color, because the Archive
	// and Unknown rows in the Groups panel and the sessions filed under
	// them need the same per-view coloring a real group gets. Validated and
	// normalized by validateGroupColor exactly like Group.Color; "" means
	// no color configured.
	ArchiveColor string
	UnknownColor string

	// Origins reports where each value came from, keyed by dotted path
	// ("sources.claude.roots", "hide.non_interactive", ...).
	// Every key that can be configured has an entry, so printing code never
	// has to guess at absent keys.
	Origins map[string]Origin
}

// fileConfig mirrors Config without the Origins field, which is derived from
// the file, not read from it. Groups doesn't mirror Config's field either:
// TOML has no notion of table order, so the file is decoded into a map here
// and turned into Config's ordered slice by consulting md.Keys() in Load.
type fileConfig struct {
	Sources map[string]Source    `toml:"sources"`
	Hide    Hide                 `toml:"hide"`
	Browse  Browse               `toml:"browse"`
	Groups  map[string]groupFile `toml:"groups"`
	Labels  map[string]string    `toml:"labels"`
}

// groupFile is one [groups.<name>] table as TOML decodes it, before its
// path patterns are expanded and it is placed into Config.Groups in file
// order.
type groupFile struct {
	Paths []string `toml:"paths"`
	Color string   `toml:"color"`
}

// defaultSources are the agents LazyRecall knows how to launch, with the
// roots the program probes today (see candidateClaudeRoots and Discover in
// internal/profile). The claude order matters: it must mirror
// candidateClaudeRoots, which probes ~/.claude-personal before ~/.claude, so
// the two defaults can never disagree about which install is which.
func defaultSources() map[string]Source {
	return map[string]Source{
		"claude": {
			Roots:  []string{"~/.claude-personal", "~/.claude"},
			Resume: []string{"claude", "--resume", "{id}"},
			EnvVar: "CLAUDE_CONFIG_DIR",
		},
		"pi": {
			Roots:         []string{"~/.pi"},
			Resume:        []string{"pi", "--session", "{id}"},
			SingleInstall: true,
		},
		"omp": {
			Roots:         []string{"~/.omp"},
			Resume:        []string{"omp", "--resume", "{id}"},
			SingleInstall: true,
		},
		"hermes": {
			Roots:         []string{"~/.hermes"},
			Resume:        []string{"hermes", "--resume", "{id}"},
			SingleInstall: true,
		},
		"goose": {
			Roots: []string{"~/.local/share/goose/sessions"},
			// --session-id "Requires --resume" (confirmed from `goose
			// session --help` before writing this) - the two must be
			// given together, not --resume alone (which would resume
			// the most recent session instead of the one requested).
			Resume:        []string{"goose", "session", "--resume", "--session-id", "{id}"},
			SingleInstall: true,
		},
		"opencode": {
			Roots: []string{"~/.local/share/opencode"},
			// -s/--session at the top level, with no subcommand, is
			// opencode's own default command ("start opencode tui") -
			// confirmed from `opencode --help` before writing this.
			Resume:        []string{"opencode", "--session", "{id}"},
			SingleInstall: true,
		},
		"kilo": {
			// Kilo is OpenCode's schema and CLI under another name, so the
			// resume flag is the same one - "-s, --session  session id to
			// continue", confirmed from `kilo --help` before writing this.
			Roots:         []string{"~/.local/share/kilo"},
			Resume:        []string{"kilo", "--session", "{id}"},
			SingleInstall: true,
		},
		"antigravity": {
			Roots: []string{"~/.gemini/antigravity-cli"},
			// --conversation "Resume a previous conversation by ID" -
			// confirmed unambiguous from `agy --help` before writing
			// this, unlike goose/opencode's flag combinations above.
			Resume:        []string{"agy", "--conversation", "{id}"},
			SingleInstall: true,
		},
	}
}

// defaultConfig is what the program runs with when no file, env var, or flag
// overrides anything.
func defaultConfig() Config {
	return Config{
		Sources: defaultSources(),
		Hide: Hide{
			NonInteractive: true,
			MinMessages:    0,
			Paths:          []string{},
		},
		Browse: Browse{ShowArchived: false, DefaultGroup: "", Transcript: "clean", DateHeaders: true},
		// Groups is nil by default: no config means no groups, and every
		// session resolves to Unknown (which the browser and CLI treat as
		// "today's behavior", not as a new state to display).
		Groups: nil,
		// Labels is nil by default: every install falls back to its own
		// name (internal/profile.Profile.Label).
		Labels: nil,
	}
}

// defaultOrigins seeds every configurable key with OriginDefault, so a config
// that never touches a key still has an answer for it.
func defaultOrigins(sources map[string]Source) map[string]Origin {
	m := map[string]Origin{
		"hide.non_interactive": OriginDefault,
		"hide.min_messages":    OriginDefault,
		"hide.paths":           OriginDefault,
		"browse.show_archived": OriginDefault,
		"browse.default_group": OriginDefault,
		"browse.transcript":    OriginDefault,
		"browse.date_headers":  OriginDefault,
		"labels":               OriginDefault,
	}
	for name := range sources {
		m["sources."+name+".roots"] = OriginDefault
		m["sources."+name+".resume"] = OriginDefault
		m["sources."+name+".env_var"] = OriginDefault
		m["sources."+name+".single_install"] = OriginDefault
	}
	// Groups has no defaults: with nothing configured, there is nothing to
	// seed an origin for. A group's origin key only comes into existence
	// once its [groups.<name>] table is read from the file, in Load.
	return m
}

// Path returns the config file location, resolved in this order:
// $LAZYRECALL_CONFIG, then $XDG_CONFIG_HOME/lazyrecall/config.toml, then
// ~/.config/lazyrecall/config.toml. The result is ~/$VAR-expanded like any
// other path in the config, so a $LAZYRECALL_CONFIG pointing at "~/" works
// the same way the built-in default does.
func Path() string {
	p := ""
	switch {
	case os.Getenv("LAZYRECALL_CONFIG") != "":
		p = os.Getenv("LAZYRECALL_CONFIG")
	case os.Getenv("XDG_CONFIG_HOME") != "":
		p = filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "lazyrecall", "config.toml")
	default:
		p = filepath.Join(homeDir(), ".config", "lazyrecall", "config.toml")
	}
	return expandPath(p)
}

// Load reads the config file at Path() and returns the effective
// configuration. A missing file is normal first-run state and yields the
// defaults; a file that exists but cannot be parsed is an error that names
// the file and the offending line.
func Load() (Config, error) {
	cfg := defaultConfig()
	cfg.Sources = expandSources(cfg.Sources)
	cfg.Hide.Paths = globPaths(cfg.Hide.Paths)
	cfg.Origins = defaultOrigins(cfg.Sources)

	path := Path()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}

	// Decode into a zeroed struct, not over the defaults: a [sources.X]
	// table in the file replaces that source's defaults entirely, it does not
	// merge field by field. Keeping the file and the defaults separate is
	// what makes "the file is the whole source" true for a table that lists
	// only some fields.
	var file fileConfig
	md, err := toml.Decode(string(data), &file)
	if err != nil {
		// toml.ParseError carries the line number; %w keeps it in the
		// message so the failure points at the exact spot to fix.
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}

	if md.IsDefined("hide", "non_interactive") {
		cfg.Hide.NonInteractive = file.Hide.NonInteractive
		cfg.Origins["hide.non_interactive"] = OriginFile
	}
	if md.IsDefined("hide", "min_messages") {
		cfg.Hide.MinMessages = file.Hide.MinMessages
		cfg.Origins["hide.min_messages"] = OriginFile
	}
	if md.IsDefined("hide", "paths") {
		cfg.Hide.Paths = globPaths(file.Hide.Paths)
		cfg.Origins["hide.paths"] = OriginFile
	}
	if md.IsDefined("browse", "show_archived") {
		cfg.Browse.ShowArchived = file.Browse.ShowArchived
		cfg.Origins["browse.show_archived"] = OriginFile
	}
	if md.IsDefined("browse", "default_group") {
		cfg.Browse.DefaultGroup = file.Browse.DefaultGroup
		cfg.Origins["browse.default_group"] = OriginFile
	}
	if md.IsDefined("browse", "transcript") {
		cfg.Browse.Transcript = file.Browse.Transcript
		cfg.Origins["browse.transcript"] = OriginFile
	}
	if md.IsDefined("browse", "date_headers") {
		cfg.Browse.DateHeaders = file.Browse.DateHeaders
		cfg.Origins["browse.date_headers"] = OriginFile
	}

	for name, s := range file.Sources {
		cfg.Sources[name] = Source{
			Roots:         expandPaths(s.Roots),
			Resume:        s.Resume,
			EnvVar:        s.EnvVar,
			SingleInstall: s.SingleInstall,
		}
		// Once a source's table appears, the whole source is file-owned -
		// even the fields the table omits, which are zero because the table
		// replaced the defaults, not because a default survives.
		cfg.Origins["sources."+name+".roots"] = OriginFile
		cfg.Origins["sources."+name+".resume"] = OriginFile
		cfg.Origins["sources."+name+".env_var"] = OriginFile
		cfg.Origins["sources."+name+".single_install"] = OriginFile
	}

	// [labels] is a top-level table, deliberately not nested under
	// [sources.<name>] - see Config.Labels' doc comment for why writing
	// labels there would silently drop the source's roots and make its
	// installs vanish from discovery.
	if md.IsDefined("labels") {
		labels := expandLabelKeys(file.Labels)
		if err := validateLabels(labels); err != nil {
			return Config{}, fmt.Errorf("config %s: %w", path, err)
		}
		cfg.Labels = labels
		cfg.Origins["labels"] = OriginFile
	}

	// Groups have no default to overlay - the file is the only source of
	// them - but TOML tables decode into a Go map, which has no memory of
	// the order they were written in. md.Keys() lists every key in file
	// order, including the two entries `[groups.<name>]` produces (the
	// table itself, then "paths" beneath it); filtering to the table keys
	// recovers exactly the declaration order, once each.
	if md.IsDefined("groups") {
		seen := make(map[string]bool, len(file.Groups))
		for _, key := range md.Keys() {
			if len(key) != 2 || key[0] != "groups" {
				continue
			}
			name := key[1]
			if seen[name] {
				continue
			}
			seen[name] = true

			// [groups.archive] and [groups.unknown] (any case) are not real
			// groups - "archive" and "unknown" stay reserved, so they never
			// reach validateGroupName or Config.Groups below - but they are
			// allowed as color-only tables for the two built-in views (change
			// archive-unknown-colors). "all" has no such exception and falls
			// through to the ordinary reserved-name rejection.
			switch strings.ToLower(name) {
			case "archive", "unknown":
				if err := validateOnlyColorKey(md, name); err != nil {
					return Config{}, fmt.Errorf("config %s: %w", path, err)
				}
				color, err := validateGroupColor(name, file.Groups[name].Color)
				if err != nil {
					return Config{}, fmt.Errorf("config %s: %w", path, err)
				}
				if strings.ToLower(name) == "archive" {
					cfg.ArchiveColor = color
				} else {
					cfg.UnknownColor = color
				}
				cfg.Origins["groups."+strings.ToLower(name)+".color"] = OriginFile
				continue
			}

			if err := validateGroupName(name); err != nil {
				return Config{}, fmt.Errorf("config %s: %w", path, err)
			}
			color, err := validateGroupColor(name, file.Groups[name].Color)
			if err != nil {
				return Config{}, fmt.Errorf("config %s: %w", path, err)
			}
			cfg.Groups = append(cfg.Groups, Group{
				Name:  name,
				Paths: expandGroupPaths(file.Groups[name].Paths),
				Color: color,
			})
			cfg.Origins["groups."+name+".paths"] = OriginFile
			cfg.Origins["groups."+name+".color"] = OriginFile
		}
	}

	// browse.default_group must name something a --group flag could
	// actually select, checked here - after cfg.Groups is fully built, so a
	// configured group's own name is in scope - rather than left for each
	// command to discover on its own (P1 fix, group-sessions-in-one-index
	// review): only --group ever went through search.ValidateGroup before
	// this, so a typo'd default_group ("typo" for "work") reached the
	// browser unvalidated, exited 0, and opened on a view with nothing in
	// it and no error explaining why. Validating in Load, next to the
	// groups validation just above, is what makes every command that reads
	// config report the mistake, not just the ones that happen to call
	// ValidateGroup themselves.
	if err := validateDefaultGroup(cfg.Browse.DefaultGroup, cfg.Groups); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	// browse.transcript gets the same treatment as browse.default_group
	// just above: checked once here, right after it is set, so every
	// caller that loads config sees the mistake instead of the browser
	// silently falling back to whichever mode a switch statement's default
	// case happens to pick.
	if err := validateTranscriptMode(cfg.Browse.Transcript); err != nil {
		return Config{}, fmt.Errorf("config %s: %w", path, err)
	}
	// "all" and "" both mean "no group filter" - Filter.Group's zero value
	// already carries that meaning everywhere it is consumed (search.Filter,
	// cmd/lazyrecall's groupLabel), and a config author writing the explicit
	// spelling should not have to know the zero value is what they want.
	// Normalizing here, rather than teaching every consumer of
	// Browse.DefaultGroup a second spelling for "All", is what keeps that
	// true without cmdBrowse (or some future caller) having to remember it.
	if cfg.Browse.DefaultGroup == "all" {
		cfg.Browse.DefaultGroup = ""
	}

	return cfg, nil
}

// validateDefaultGroup checks browse.default_group against exactly the
// values a session's effective group can be viewed by: "" or its explicit
// spelling "all" (no group filter), "archive", "unknown", or the name of a
// group declared in groups. It is the config-file sibling of
// search.ValidateGroup, which checks the same set of names for --group -
// duplicated rather than shared, because internal/config cannot import
// internal/search (search already imports config) and the check is a
// handful of string comparisons, not worth restructuring an import
// direction over.
func validateDefaultGroup(name string, groups []Group) error {
	switch name {
	case "", "all", "archive", "unknown":
		return nil
	}
	for _, g := range groups {
		if g.Name == name {
			return nil
		}
	}
	valid := make([]string, 0, len(groups)+3)
	valid = append(valid, "all", "archive", "unknown")
	for _, g := range groups {
		valid = append(valid, g.Name)
	}
	return fmt.Errorf("browse.default_group %q: valid values are: %s", name, strings.Join(valid, ", "))
}

// validateTranscriptMode checks browse.transcript against the only two
// renderings the Transcript tab has (see Browse.Transcript): anything else
// is a typo, and a typo silently falling back to one of the two would be a
// harder mistake to notice than a hard error naming the file and the value,
// the same choice validateDefaultGroup makes for browse.default_group.
func validateTranscriptMode(value string) error {
	switch value {
	case "clean", "full":
		return nil
	}
	return fmt.Errorf("browse.transcript %q: valid values are: clean, full", value)
}

// Set records that the caller overrode one dotted key and marks where that
// value came from. The caller applies the value itself first - Set takes no
// value because it exists so provenance survives after the value is long
// gone. Flag overrides are the highest-precedence layer, so callers must
// call Set last.
func (c *Config) Set(key string, origin Origin) {
	c.Origins[key] = origin
}

// expandPaths expands ~ and $VAR in every path once, at load, so nothing
// downstream has to remember to do it - and nothing can forget.
func expandPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = expandPath(p)
	}
	return out
}

// expandSources applies the same one-time expansion to every source's
// roots. Defaults are expanded too, so the config Load returns is already
// in its final form whether a value came from the file or from a default.
func expandSources(sources map[string]Source) map[string]Source {
	out := make(map[string]Source, len(sources))
	for name, s := range sources {
		s.Roots = expandPaths(s.Roots)
		out[name] = s
	}
	return out
}

// expandLabelKeys expands ~ and $VAR and filepath.Clean's each key of the
// top-level [labels] table - install roots, matched the same way
// expandGroupPaths treats a group's paths, so a trailing slash or
// non-canonical spelling in the config doesn't silently fail to match at
// lookup (internal/profile.Profile.Label). The label values themselves are
// opaque display strings and are left untouched.
func expandLabelKeys(labels map[string]string) map[string]string {
	if labels == nil {
		return nil
	}
	out := make(map[string]string, len(labels))
	for root, label := range labels {
		out[filepath.Clean(expandPath(root))] = label
	}
	return out
}

// validateLabels rejects two configured-labels mistakes before they reach
// anything that uses them: a label with no value (silently falling back to
// the install's own name is a confusing way to learn the value was empty),
// and two roots that would end up sharing the same label. The latter
// matters because a label doubles as a valid --agent value
// (cmd/lazyrecall's resolveAgentLabel) - two installs answering to the same
// label would make that value ambiguous between them. Iterates roots in
// sorted order so the error is deterministic, not dependent on Go's
// randomized map iteration.
func validateLabels(labels map[string]string) error {
	roots := make([]string, 0, len(labels))
	for root := range labels {
		roots = append(roots, root)
	}
	sort.Strings(roots)

	seenBy := map[string]string{} // label -> first root that claimed it
	for _, root := range roots {
		label := labels[root]
		if label == "" {
			return fmt.Errorf("label for %q must not be empty", root)
		}
		if prior, ok := seenBy[label]; ok {
			return fmt.Errorf("label %q is used by both %q and %q", label, prior, root)
		}
		seenBy[label] = root
	}
	return nil
}

// expandGroupPaths expands and cleans each configured group path. Group
// paths are matched against a session's cwd as directory prefixes at query
// time (internal/search), never as SQLite GLOB patterns the way hide.paths
// is, so they get filepath.Clean rather than hide's '**' collapsing.
func expandGroupPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = filepath.Clean(expandPath(p))
	}
	return out
}

// globPaths expands and normalises hide path patterns at load. SQLite's GLOB
// '*' already crosses '/', so '**' in a configured hide path would be a
// meaningless doubling; collapsing it to '*' makes a '**' pattern behave
// identically to the equivalent '*' one (and, unlike '**' in glob(3), the
// two are not different pattern languages here).
func globPaths(paths []string) []string {
	out := make([]string, len(paths))
	for i, p := range paths {
		out[i] = strings.ReplaceAll(expandPath(p), "**", "*")
	}
	return out
}

// reservedGroupNames are the filter values Filter.Group already gives a
// fixed meaning (internal/search): "all" is the zero value, and "archive"
// and "unknown" are the two synthetic views alongside a session's real
// group. A configured [groups.<name>] table using one of these would be
// unreachable - --group=archive could never mean the config's group named
// "archive", only the reserved view - so it is rejected at load rather than
// silently shadowed. Compared case-insensitively: --group=Archive typed by
// a user would still hit the reserved view, so a group spelled differently
// only in case is just as unreachable. validateGroupName is only ever
// consulted for "archive"/"unknown" the way it is for any other reserved or
// empty name - Load intercepts both before this check, since their
// color-only tables (change archive-unknown-colors) are a narrower
// exception, not a lifting of the reservation.
var reservedGroupNames = map[string]bool{"all": true, "archive": true, "unknown": true}

// validateGroupName rejects a configured group name that could never be
// selected: empty, or one of the reserved filter values above.
func validateGroupName(name string) error {
	if name == "" {
		return fmt.Errorf("group name must not be empty")
	}
	if reservedGroupNames[strings.ToLower(name)] {
		return fmt.Errorf("group name %q is reserved for the built-in %q filter view", name, strings.ToLower(name))
	}
	return nil
}

// ansiColorNames are the color values a [groups.<name>] table's color key
// accepts by name: the 8 standard ANSI foreground colors and their
// high-intensity "bright-" variants. This is the same set internal/cli's
// groupAnsiCode recognizes - duplicated, not shared, because config has no
// business knowing about ANSI escape sequences (see Group.Color's doc
// comment) and the set is eight names long and effectively frozen, so the
// duplication is not a maintenance risk worth an import-cycle-avoiding
// abstraction over.
var ansiColorNames = map[string]bool{
	"black": true, "red": true, "green": true, "yellow": true,
	"blue": true, "magenta": true, "cyan": true, "white": true,
	"bright-black": true, "bright-red": true, "bright-green": true, "bright-yellow": true,
	"bright-blue": true, "bright-magenta": true, "bright-cyan": true, "bright-white": true,
}

// hexColorPattern matches a 24-bit truecolor triplet, e.g. "#3355ff".
var hexColorPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// validateGroupColor checks a configured [groups.<name>] color value: empty
// (no color configured) is always fine, an ANSI name (case-insensitive) or
// a "#rrggbb" hex triplet normalizes to its lowercased form, and anything
// else is a hard error naming the file (via the "config %s: %w" wrap at the
// call site), the group, and the offending value - the same shape
// validateGroupName's reserved-name error takes, so an invalid color reads
// like every other config mistake instead of a stack trace.
func validateGroupColor(name, value string) (string, error) {
	if value == "" {
		return "", nil
	}
	norm := strings.ToLower(strings.TrimSpace(value))
	if ansiColorNames[norm] || hexColorPattern.MatchString(norm) {
		return norm, nil
	}
	return "", fmt.Errorf("group %q color %q: must be black, red, green, yellow, blue, magenta, cyan, white, a bright- variant of one of those, or a hex color like #3355ff", name, value)
}

// validateOnlyColorKey rejects any key under a [groups.archive] or
// [groups.unknown] table other than color - most importantly paths, which
// would imply "archive"/"unknown" claim sessions by working directory the
// way a real group does, when they are query-time views, not something a
// session is ever filed under by path. md.Keys() lists every key the TOML
// source actually contained under that table, in file order, regardless of
// whether it maps onto a field of groupFile - so an unrecognized key (not
// just "paths") is caught here too, not silently dropped.
func validateOnlyColorKey(md toml.MetaData, name string) error {
	for _, key := range md.Keys() {
		if len(key) == 3 && key[0] == "groups" && key[1] == name && key[2] != "color" {
			return fmt.Errorf("group %q: table may only set color, found %q", name, key[2])
		}
	}
	return nil
}

func expandPath(p string) string {
	switch {
	case p == "~":
		p = homeDir()
	case strings.HasPrefix(p, "~/"):
		p = filepath.Join(homeDir(), p[2:])
	}
	return os.ExpandEnv(p)
}

// homeDir prefers $HOME and falls back to os.UserHomeDir, matching
// internal/profile so tests can pin the home with HOME.
func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}
