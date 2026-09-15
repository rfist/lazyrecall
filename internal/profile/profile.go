// Package profile discovers which agent config roots exist on this machine
// and names each one an "install" (spec session-index; change
// group-sessions-in-one-index, "Model": "Install is a discovered config
// root").
//
// Where the roots come from is the config file (internal/config), whose
// built-in defaults reproduce the program's historical hardcoded roots.
// Each configured source is probed in the order its roots are listed and
// the ones that exist are kept. Every existing root becomes its own
// install - there is no more bundling of every single-install source into
// one "primary" profile the way there was under one-database-per-profile
// (design.md decision 1, reversed by this change): with one index for
// every install, there is nothing left for bundling to protect.
package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/rfist/lazyrecall/internal/config"
)

// Profile is one discovered install: a bundle of config roots, at most one
// per source. Every install Discover returns has exactly one entry in
// Roots - the type keeps the map shape (rather than a single source/root
// pair) so adapter.Discover(p profile.Profile), which every adapter already
// implements, needs no change.
type Profile struct {
	// Name identifies the install in output and (change
	// group-sessions-in-one-index) as the middle segment of every composite
	// session id it produces. The named fields this used to be (ClaudeRoot,
	// PiRoot, ...) are gone because the set of sources is no longer fixed:
	// it comes from the config, and a map is the shape that cannot drift out
	// of sync with it (change sources-become-data).
	Name string

	// Roots maps a source name ("claude", "pi", ...) to that source's
	// config root for this install. Every install Discover returns has
	// exactly one key.
	Roots map[string]string
}

// Sources returns the (source name, root) pairs this install has data for.
func (p Profile) Sources() map[string]string { return p.Roots }

// Source returns the name of the one source this install carries data for,
// or "" for a zero-value Profile that names none. Discover never returns an
// install with more than one entry in Roots, so there is no ambiguity to
// resolve here.
func (p Profile) Source() string {
	for name := range p.Roots {
		return name
	}
	return ""
}

// Root returns this install's one config root, or "" for a zero-value
// Profile.
func (p Profile) Root() string {
	for _, root := range p.Roots {
		return root
	}
	return ""
}

// ConfiguredLabel returns this install's label as actually written in
// cfg.Labels, and whether one is configured at all - as opposed to Label's
// fallback to the install's own name, which a caller resolving "does arg
// name a configured label" must not mistake for one (change
// group-sessions-in-one-index, P1 fix: cmd/lazyrecall's resolveAgentFilter
// used to call Label and compare its fallback value against arg, so with no
// [labels] configured, an install named the same as its own source - one
// claude install is literally named "claude" - was mistaken for a label of
// itself and --agent=claude matched only that one install instead of every
// claude install). Root paths are compared after filepath.Clean on both
// sides - cfg.Labels' keys are already expanded and cleaned when it comes
// from config.Load (config.expandLabelKeys), but this doesn't assume that: a
// caller building a Config by hand (as this package's own tests do) gets the
// same match-a-trailing-slash forgiveness.
func (p Profile) ConfiguredLabel(cfg config.Config) (label string, ok bool) {
	if len(cfg.Labels) == 0 {
		return "", false
	}
	root := filepath.Clean(p.Root())
	for r, l := range cfg.Labels {
		if filepath.Clean(r) == root {
			return l, true
		}
	}
	return "", false
}

// Label returns this install's display label: the label configured for its
// root in the top-level cfg.Labels table (ConfiguredLabel), else the
// install's own name (change group-sessions-in-one-index, follow-up fix:
// labels moved out of [sources.<name>] into their own [labels] table, since
// a [sources.X] table in the file replaces that source's roots too, and the
// obvious way to write labels must not silently drop an agent from
// discovery).
func (p Profile) Label(cfg config.Config) string {
	if label, ok := p.ConfiguredLabel(cfg); ok {
		return label
	}
	return p.Name
}

func homeDir() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	h, _ := os.UserHomeDir()
	return h
}

// looksLikeClaudeRoot reports whether dir has the shape of a Claude Code
// config directory, so a stray empty directory named ".claude*" is not
// mistaken for a real one.
func looksLikeClaudeRoot(dir string) bool {
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, "history.jsonl")); err == nil {
		return true
	}
	if fi, err := os.Stat(filepath.Join(dir, "projects")); err == nil && fi.IsDir() {
		return true
	}
	return false
}

func exists(dir string) bool {
	fi, err := os.Stat(dir)
	return err == nil && fi.IsDir()
}

// sourceRoots returns the roots to probe for a configured source: the
// pre-config environment override when one is set, else the source's own
// configured roots. The env overrides (LAZYRECALL_CLAUDE_CONFIG_DIRS,
// LAZYRECALL_PI_HOME, ...) predate the config file and remain the only way
// to point discovery at synthetic roots in tests and scripts, so a value
// they set must keep meaning exactly what it always meant.
func sourceRoots(cfg config.Config, name string) []string {
	if name == "claude" {
		if v := env("CLAUDE_CONFIG_DIRS"); v != "" {
			var out []string
			for _, p := range strings.Split(v, ":") {
				if p != "" {
					out = append(out, p)
				}
			}
			return out
		}
		return cfg.Sources[name].Roots
	}
	if v := env(strings.ToUpper(name) + "_HOME"); v != "" {
		return []string{v}
	}
	return cfg.Sources[name].Roots
}

// sourceRootExists is the existence test a source's roots must pass to be
// discovered. claude alone needs the stricter shape check: a stray empty
// directory named ".claude*" must still not count as a real install.
func sourceRootExists(name, root string) bool {
	if name == "claude" {
		return looksLikeClaudeRoot(root)
	}
	return exists(root)
}

// profileName derives a non-single-install source's install name from its
// config root, so ~/.claude-personal and ~/.claude become the installs
// "claude-personal" and "claude".
func profileName(root string) string {
	base := filepath.Base(root)
	return strings.TrimPrefix(base, ".")
}

// installName is the name Discover gives an install for one of source's
// existing roots: a single_install source (spec session-index) always uses
// the source's own name, since it can only ever have one install; any other
// source uses its root's basename (profileName).
func installName(cfg config.Config, source, root string) string {
	if cfg.Sources[source].SingleInstall {
		return source
	}
	return profileName(root)
}

// Discover finds every install present on the machine, driven by the
// config's source roots (internal/config): every existing root of every
// configured source becomes its own install (change
// group-sessions-in-one-index). It never fails because a source is absent -
// an absent source simply contributes nothing (spec session-index, "Source
// discovery") - but a config file that cannot be parsed is a hard error,
// surfaced here rather than swallowed: the user wrote that file and expects
// it to be in effect. Two installs that would end up with the same name
// (e.g. two single_install sources sharing a source name, which cannot
// legitimately happen, or two claude roots with the same basename) is a
// hard error too: silently dropping one would orphan its handles, comments,
// and tags exactly as silently renaming a profile used to (see profileName's
// old doc comment).
func Discover() ([]Profile, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	// Iterate source names sorted so install creation order is
	// deterministic regardless of map iteration order.
	names := make([]string, 0, len(cfg.Sources))
	for name := range cfg.Sources {
		names = append(names, name)
	}
	sort.Strings(names)

	var profiles []Profile
	claimedBy := map[string]string{} // install name -> "source:root" that claimed it, for the collision error

	for _, name := range names {
		roots := sourceRoots(cfg, name)
		for _, root := range roots {
			if !sourceRootExists(name, root) {
				continue
			}
			iname := installName(cfg, name, root)
			claim := name + ":" + root
			if prior, ok := claimedBy[iname]; ok {
				return nil, fmt.Errorf("profile: %s and %s would both be named install %q; configure distinct roots to tell them apart", prior, claim, iname)
			}
			claimedBy[iname] = claim
			profiles = append(profiles, Profile{Name: iname, Roots: map[string]string{name: root}})
		}
	}

	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// env reads one of this program's LAZYRECALL_-prefixed environment
// overrides. Every override goes through here, so the prefix is one rule in
// one place rather than repeated at each call site.
func env(name string) string {
	return os.Getenv("LAZYRECALL_" + name)
}

// DataDir is the directory LazyRecall's own database lives in. Overridable
// via LAZYRECALL_HOME; defaults to ~/.lazyrecall. This is the one accepted
// data location LazyRecall writes to (proposal.md - Impact).
func DataDir() string {
	if v := env("HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".lazyrecall")
}

// DBPath returns the single database file every install's data lives in:
// DataDir()/index.db. One index, not one file per install (change
// group-sessions-in-one-index, reversing design.md decision 1 - see that
// change's proposal for why the account an install belongs to no longer
// decides where its sessions are stored).
func DBPath() string {
	return filepath.Join(DataDir(), "index.db")
}
