// Package profile resolves which agent config roots exist on the machine,
// groups them into isolated profiles, and selects exactly one active
// profile per invocation (spec session-index, "Profile isolation";
// design.md decision 1).
//
// Where the roots come from is the config file (internal/config), whose
// built-in defaults reproduce the program's historical hardcoded roots.
// Each configured source is probed in the order its roots are listed and
// the ones that exist are kept. A source with more than one configured
// root produces one profile per root that exists - that is how claude and
// claude-personal become two profiles. A source with exactly one root is
// attached to exactly one "primary" profile, never duplicated across
// profiles and never silently attached to a work profile, so a session
// from a single-install source is never presented under two different
// profile identities (design note recorded in devdocs/fyi.md). The primary
// profile defaults to whichever discovered profile's name contains
// "personal" and can be overridden explicitly; if no such profile exists
// at all, a synthetic "default" profile carries the single-install
// sources alone.
package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"lazyrecall/internal/config"
)

// Profile is one isolated identity: a bundle of config roots, at most one
// per source, that are indexed and queried together and never mixed with
// another profile's data.
type Profile struct {
	// Name identifies the profile in output and in the database filename.
	Name string

	// Roots maps a source name ("claude", "pi", ...) to that source's
	// config root for this profile. A key with a non-empty value means
	// this profile has data for that source; an absent key means it does
	// not. The named fields this used to be (ClaudeRoot, PiRoot, ...) are
	// gone because the set of sources is no longer fixed: it comes from
	// the config, and a map is the shape that cannot drift out of sync
	// with it (change sources-become-data).
	Roots map[string]string
}

// Sources returns the (source name, root) pairs this profile has data for.
func (p Profile) Sources() map[string]string { return p.Roots }

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

// profileName derives a profile's name from its config root, so
// ~/.claude-personal and ~/.claude become the profiles "claude-personal"
// and "claude".
func profileName(root string) string {
	base := filepath.Base(root)
	return strings.TrimPrefix(base, ".")
}

// Discover finds every profile present on the machine, driven by the
// config's source roots (internal/config). It never fails because a source
// is absent - an absent source simply contributes nothing (spec
// session-index, "Source discovery") - but a config file that cannot be
// parsed is a hard error, surfaced here rather than swallowed: the user
// wrote that file and expects it to be in effect.
func Discover() ([]Profile, error) {
	cfg, err := config.Load()
	if err != nil {
		return nil, err
	}

	// Iterate source names sorted so profile creation order is
	// deterministic regardless of map iteration order.
	names := make([]string, 0, len(cfg.Sources))
	for name := range cfg.Sources {
		names = append(names, name)
	}
	sort.Strings(names)

	var profiles []Profile
	type singleRoot struct {
		source string
		root   string
	}
	var singles []singleRoot

	for _, name := range names {
		roots := sourceRoots(cfg, name)
		var existing []string
		for _, root := range roots {
			if sourceRootExists(name, root) {
				existing = append(existing, root)
			}
		}
		// A source that can have more than one installation names a
		// profile per root that exists; a single-install source is
		// attached to the primary profile below rather than naming one of
		// its own. Which it is comes from the config, never from counting
		// the roots the user happened to list: a profile's name is its
		// database filename, so inferring it would let someone change
		// which database they are using - orphaning every handle,
		// comment, and tag in the old one - just by narrowing a roots
		// list (change sources-become-data).
		if !cfg.Sources[name].SingleInstall {
			for _, root := range existing {
				profiles = append(profiles, Profile{Name: profileName(root), Roots: map[string]string{name: root}})
			}
		} else {
			for _, root := range existing {
				singles = append(singles, singleRoot{name, root})
			}
		}
	}

	// Attach the single-install roots to exactly one profile: the primary.
	// Prefer a profile whose name suggests "personal"; fall back to the
	// first discovered profile; if there is no profile at all, synthesize
	// a "default" one to carry them.
	if len(singles) > 0 {
		primaryIdx := -1
		for i, p := range profiles {
			if strings.Contains(p.Name, "personal") {
				primaryIdx = i
				break
			}
		}
		if primaryIdx == -1 && len(profiles) > 0 {
			primaryIdx = 0
		}
		if primaryIdx >= 0 {
			roots := profiles[primaryIdx].Roots
			if roots == nil {
				roots = map[string]string{}
				profiles[primaryIdx].Roots = roots
			}
			for _, s := range singles {
				roots[s.source] = s.root
			}
		} else {
			roots := map[string]string{}
			for _, s := range singles {
				roots[s.source] = s.root
			}
			profiles = append(profiles, Profile{Name: "default", Roots: roots})
		}
	}

	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles, nil
}

// configuredSourceNames lists the sources the config actually defines, sorted.
// The "nothing was found" error names them because the set is no longer fixed:
// telling a user with a codex-only config that no "Claude, pi, omp, or hermes"
// directory was found would name four sources they never configured and omit
// the one they did.
func configuredSourceNames() []string {
	cfg, err := config.Load()
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(cfg.Sources))
	for name := range cfg.Sources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Resolve picks exactly one active profile (spec session-index, "Profile
// isolation": "Exactly one profile SHALL be active for any operation").
// Resolution order: the explicitly requested name, then the
// LAZYRECALL_PROFILE/RECALL_PROFILE environment, then the config file's
// default_profile, then the primary profile (the one bundling the
// single-install sources), and finally - where nothing gave a reason to
// prefer any one profile - the first profile by name. That last step is
// what makes a machine with two Claude installs and none of the others
// usable: before it existed, such a machine was a hard error with no way
// out from inside the tool. A requested name that matches nothing is still
// a hard error listing the real choices, never a silent fallback.
func Resolve(profiles []Profile, requested string) (Profile, error) {
	if len(profiles) == 0 {
		return Profile{}, fmt.Errorf("no session sources found on this machine: none of the configured sources (%s) has a config directory here", strings.Join(configuredSourceNames(), ", "))
	}

	if requested == "" {
		requested = envProfile()
	}
	if requested != "" {
		for _, p := range profiles {
			if p.Name == requested {
				return p, nil
			}
		}
		names := make([]string, len(profiles))
		for i, p := range profiles {
			names[i] = p.Name
		}
		return Profile{}, fmt.Errorf("no such profile %q; available profiles: %s", requested, strings.Join(names, ", "))
	}

	cfg, err := config.Load()
	if err != nil {
		return Profile{}, err
	}
	if cfg.DefaultProfile != "" {
		for _, p := range profiles {
			if p.Name == cfg.DefaultProfile {
				return p, nil
			}
		}
		// A default_profile that names a profile absent from this machine
		// falls through to the built-in rule: the setting is a preference,
		// and a stale one must not make the tool refuse to run.
	}

	for _, p := range profiles {
		if len(p.Roots) > 1 {
			return p, nil
		}
	}

	return profiles[0], nil
}

// env reads one of this program's environment overrides, preferring the
// current LAZYRECALL_-prefixed name and falling back to the pre-rename
// RECALL_ one (change rename-to-lazyrecall). Every override goes through
// here, so "the old name still works" is one rule in one place rather than
// a fallback that some variables got and others quietly did not.
func env(name string) string {
	if v := os.Getenv("LAZYRECALL_" + name); v != "" {
		return v
	}
	return os.Getenv("RECALL_" + name)
}

// envProfile reads the profile name from the environment.
func envProfile() string { return env("PROFILE") }

// DataDir is the directory LazyRecall's own per-profile databases live in.
// Overridable via LAZYRECALL_HOME; defaults to ~/.lazyrecall. This is the
// one accepted data location LazyRecall writes to (proposal.md - Impact).
//
// RECALL_HOME is still honoured when LAZYRECALL_HOME is unset (change
// rename-to-lazyrecall): the variable predates the rename, and an
// environment that still sets it would otherwise silently start indexing
// into a second, empty database instead of the one it has been pointing at.
func DataDir() string {
	if v := env("HOME"); v != "" {
		return v
	}
	return filepath.Join(homeDir(), ".lazyrecall")
}

// DataDirExplicit reports whether the data directory was chosen by the
// environment rather than defaulted. The one-time migration in
// internal/refresh needs to know: a caller that has named its data
// directory has said where its data is, and moving something else on top of
// that would be the opposite of what it asked for.
func DataDirExplicit() bool { return env("HOME") != "" }

// LegacyDataDir is the pre-rename default data directory, ~/.recall. It is
// consulted for exactly one purpose - the one-time move in refresh.New -
// and is deliberately not part of DataDir's resolution order: an existing
// ~/.recall is migrated, never read in place, so there is only ever one
// live database location.
func LegacyDataDir() string {
	return filepath.Join(homeDir(), ".recall")
}

// DBPath returns the single database file for a profile. One file per
// profile, not one shared file with a profile column (design.md decision
// 1) - resolved here, in exactly one place (task 2.5).
func DBPath(p Profile) string {
	return filepath.Join(DataDir(), p.Name+".db")
}
