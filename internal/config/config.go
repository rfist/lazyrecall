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
// applies the file layer, ApplyEnv the env layer, and Set the flag layer, so
// callers that apply flag overrides must call Set last.
package config

import (
	"fmt"
	"os"
	"path/filepath"
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
}

// Config is the effective configuration: defaults overlaid by the file, then
// by environment, then by flags.
type Config struct {
	DefaultProfile string            `toml:"default_profile"`
	Sources        map[string]Source `toml:"sources"`
	Hide           Hide              `toml:"hide"`
	Browse         Browse            `toml:"browse"`

	// Origins reports where each value came from, keyed by dotted path
	// ("default_profile", "sources.claude.roots", "hide.non_interactive", ...).
	// Every key that can be configured has an entry, so printing code never
	// has to guess at absent keys.
	Origins map[string]Origin
}

// fileConfig mirrors Config without the Origins field, which is derived from
// the file, not read from it.
type fileConfig struct {
	DefaultProfile string            `toml:"default_profile"`
	Sources        map[string]Source `toml:"sources"`
	Hide           Hide              `toml:"hide"`
	Browse         Browse            `toml:"browse"`
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
			Roots:  []string{"~/.pi"},
			Resume: []string{"pi", "--session", "{id}"},
		},
		"omp": {
			Roots:  []string{"~/.omp"},
			Resume: []string{"omp", "--resume", "{id}"},
		},
		"hermes": {
			Roots:  []string{"~/.hermes"},
			Resume: []string{"hermes", "--resume", "{id}"},
		},
	}
}

// defaultConfig is what the program runs with when no file, env var, or flag
// overrides anything. An empty DefaultProfile is a signal, not a value: the
// program applies its own profile rule.
func defaultConfig() Config {
	return Config{
		Sources: defaultSources(),
		Hide: Hide{
			NonInteractive: true,
			MinMessages:    0,
			Paths:          []string{},
		},
		Browse: Browse{ShowArchived: false},
	}
}

// defaultOrigins seeds every configurable key with OriginDefault, so a config
// that never touches a key still has an answer for it.
func defaultOrigins(sources map[string]Source) map[string]Origin {
	m := map[string]Origin{
		"default_profile":      OriginDefault,
		"hide.non_interactive": OriginDefault,
		"hide.min_messages":    OriginDefault,
		"hide.paths":           OriginDefault,
		"browse.show_archived": OriginDefault,
	}
	for name := range sources {
		m["sources."+name+".roots"] = OriginDefault
		m["sources."+name+".resume"] = OriginDefault
		m["sources."+name+".env_var"] = OriginDefault
	}
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
	cfg.Hide.Paths = expandPaths(cfg.Hide.Paths)
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

	if md.IsDefined("default_profile") {
		cfg.DefaultProfile = file.DefaultProfile
		cfg.Origins["default_profile"] = OriginFile
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
		cfg.Hide.Paths = expandPaths(file.Hide.Paths)
		cfg.Origins["hide.paths"] = OriginFile
	}
	if md.IsDefined("browse", "show_archived") {
		cfg.Browse.ShowArchived = file.Browse.ShowArchived
		cfg.Origins["browse.show_archived"] = OriginFile
	}

	for name, s := range file.Sources {
		cfg.Sources[name] = Source{
			Roots:  expandPaths(s.Roots),
			Resume: s.Resume,
			EnvVar: s.EnvVar,
		}
		// Once a source's table appears, the whole source is file-owned -
		// even the fields the table omits, which are zero because the table
		// replaced the defaults, not because a default survives.
		cfg.Origins["sources."+name+".roots"] = OriginFile
		cfg.Origins["sources."+name+".resume"] = OriginFile
		cfg.Origins["sources."+name+".env_var"] = OriginFile
	}

	return cfg, nil
}

// ApplyEnv overlays LAZYRECALL_* overrides and records them as OriginEnv.
// LAZYRECALL_PROFILE is the only override that maps onto a config field
// today; others arrive later and slot in here. It must be called after Load
// and before Set, or an env value would stomp a flag value.
func (c *Config) ApplyEnv() {
	if v := profileEnv(); v != "" {
		c.DefaultProfile = v
		c.Origins["default_profile"] = OriginEnv
	}
}

// Set records that the caller overrode one dotted key and marks where that
// value came from. The caller applies the value itself first - Set takes no
// value because it exists so provenance survives after the value is long
// gone. Flag overrides are the highest-precedence layer, so callers must
// call Set last.
func (c *Config) Set(key string, origin Origin) {
	c.Origins[key] = origin
}

// profileEnv follows the house convention from internal/profile's env(): the
// LAZYRECALL_ name wins and the pre-rename RECALL_ name still works, so one
// rule covers every override rather than a fallback some variables got and
// others quietly did not.
func profileEnv() string {
	if v := os.Getenv("LAZYRECALL_PROFILE"); v != "" {
		return v
	}
	return os.Getenv("RECALL_PROFILE")
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

// expandSources applies the same one-time expansion to every source's roots.
// Defaults are expanded too, so the config Load returns is already in its
// final form whether a value came from the file or from a default.
func expandSources(sources map[string]Source) map[string]Source {
	out := make(map[string]Source, len(sources))
	for name, s := range sources {
		s.Roots = expandPaths(s.Roots)
		out[name] = s
	}
	return out
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
