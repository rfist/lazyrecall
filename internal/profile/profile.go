// Package profile resolves which agent config roots exist on the machine,
// groups them into isolated profiles, and selects exactly one active
// profile per invocation (spec session-index, "Profile isolation";
// design.md decision 1).
//
// Design note on profile granularity (not dictated verbatim by the spec,
// recorded here and in devdocs/fyi.md):
//
// Of the four sources, only Claude Code is known to have more than one
// config root on the author's machine ($CLAUDE_CONFIG_DIR-selected
// ~/.claude for work, ~/.claude-personal for personal). pi, omp, and hermes
// each have exactly one canonical root and no work/personal split anywhere
// in the planning artifacts. So: every discovered Claude config root is its
// own profile (task 5.2). pi/omp/hermes are bundled into exactly one
// "primary" profile - never duplicated across profiles, never silently
// attached to a work profile - so that a session from a single-install
// source is never presented under two different profile identities. The
// primary profile defaults to whichever Claude root is personal-looking
// (name contains "personal"), and can be overridden explicitly. If no
// Claude root exists at all, a synthetic "default" profile carries
// pi/omp/hermes alone.
package profile

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Profile is one isolated identity: a bundle of config roots, at most one
// per source, that are indexed and queried together and never mixed with
// another profile's data.
type Profile struct {
	// Name identifies the profile in output and in the database filename.
	Name string

	// ClaudeRoot is this profile's Claude config root ($CLAUDE_CONFIG_DIR
	// equivalent), or "" if this profile has no Claude data.
	ClaudeRoot string

	// PiRoot, OmpRoot, HermesRoot are "" unless this is the primary profile
	// that bundles the machine's single-instance sources.
	PiRoot     string
	OmpRoot    string
	HermesRoot string
}

// Sources returns the (source name, root) pairs this profile has data for.
func (p Profile) Sources() map[string]string {
	m := map[string]string{}
	if p.ClaudeRoot != "" {
		m["claude"] = p.ClaudeRoot
	}
	if p.PiRoot != "" {
		m["pi"] = p.PiRoot
	}
	if p.OmpRoot != "" {
		m["omp"] = p.OmpRoot
	}
	if p.HermesRoot != "" {
		m["hermes"] = p.HermesRoot
	}
	return m
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

// candidateClaudeRoots returns the Claude config roots to probe: an
// explicit override list (LAZYRECALL_CLAUDE_CONFIG_DIRS, colon-separated)
// if set, else the two conventional defaults.
func candidateClaudeRoots() []string {
	if v := env("CLAUDE_CONFIG_DIRS"); v != "" {
		var out []string
		for _, p := range strings.Split(v, ":") {
			if p != "" {
				out = append(out, p)
			}
		}
		return out
	}
	home := homeDir()
	return []string{
		filepath.Join(home, ".claude-personal"),
		filepath.Join(home, ".claude"),
	}
}

func claudeProfileName(root string) string {
	base := filepath.Base(root)
	return strings.TrimPrefix(base, ".")
}

// Discover finds every profile present on the machine. It never returns an
// error for an absent source - an absent source simply contributes nothing
// (spec session-index, "Source discovery").
func Discover() []Profile {
	home := homeDir()

	var claudeRoots []string
	for _, c := range candidateClaudeRoots() {
		if looksLikeClaudeRoot(c) {
			claudeRoots = append(claudeRoots, c)
		}
	}

	piRoot := env("PI_HOME")
	if piRoot == "" {
		piRoot = filepath.Join(home, ".pi")
	}
	if !exists(piRoot) {
		piRoot = ""
	}

	ompRoot := env("OMP_HOME")
	if ompRoot == "" {
		ompRoot = filepath.Join(home, ".omp")
	}
	if !exists(ompRoot) {
		ompRoot = ""
	}

	hermesRoot := env("HERMES_HOME")
	if hermesRoot == "" {
		hermesRoot = filepath.Join(home, ".hermes")
	}
	if !exists(hermesRoot) {
		hermesRoot = ""
	}

	var profiles []Profile
	for _, root := range claudeRoots {
		profiles = append(profiles, Profile{Name: claudeProfileName(root), ClaudeRoot: root})
	}

	// Attach the single-instance sources to exactly one profile: the
	// primary. Prefer a Claude root whose name suggests "personal"; fall
	// back to the first discovered Claude root; if there is no Claude root
	// at all, synthesize a "default" profile.
	if piRoot != "" || ompRoot != "" || hermesRoot != "" {
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
			profiles[primaryIdx].PiRoot = piRoot
			profiles[primaryIdx].OmpRoot = ompRoot
			profiles[primaryIdx].HermesRoot = hermesRoot
		} else {
			profiles = append(profiles, Profile{
				Name:       "default",
				PiRoot:     piRoot,
				OmpRoot:    ompRoot,
				HermesRoot: hermesRoot,
			})
		}
	}

	sort.Slice(profiles, func(i, j int) bool { return profiles[i].Name < profiles[j].Name })
	return profiles
}

// Resolve picks exactly one active profile (spec session-index, "Profile
// isolation": "Exactly one profile SHALL be active for any operation").
// requested, when non-empty, must name a discovered profile exactly.
// Otherwise LAZYRECALL_PROFILE is consulted, and failing that the primary
// profile (the one bundling pi/omp/hermes, if any) is used, and failing
// that, if exactly one profile was discovered, that one is used.
func Resolve(profiles []Profile, requested string) (Profile, error) {
	if len(profiles) == 0 {
		return Profile{}, fmt.Errorf("no session sources found on this machine: no Claude, pi, omp, or hermes config directories were discovered")
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

	for _, p := range profiles {
		if p.PiRoot != "" || p.OmpRoot != "" || p.HermesRoot != "" {
			return p, nil
		}
	}
	if len(profiles) == 1 {
		return profiles[0], nil
	}

	names := make([]string, len(profiles))
	for i, p := range profiles {
		names[i] = p.Name
	}
	return Profile{}, fmt.Errorf("multiple profiles found (%s) and none is the default; pass --profile or set LAZYRECALL_PROFILE", strings.Join(names, ", "))
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
