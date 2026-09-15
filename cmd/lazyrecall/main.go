// Command lazyrecall is a cross-agent index over coding-agent sessions already
// written to disk by Claude Code, pi, omp, hermes, Goose, OpenCode, Kilo,
// and the Antigravity CLI. It is read-only with respect to every source; the only
// file it writes is its own single index database (see
// internal/profile.DataDir, internal/profile.DBPath).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rfist/lazyrecall/internal/annotate"
	"github.com/rfist/lazyrecall/internal/cli"
	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/profile"
	"github.com/rfist/lazyrecall/internal/refresh"
	"github.com/rfist/lazyrecall/internal/resume"
	"github.com/rfist/lazyrecall/internal/review"
	"github.com/rfist/lazyrecall/internal/search"
	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// version and buildTime are set at build time via
// `-ldflags "-X main.version=... -X main.buildTime=..."`. Left at their
// zero-value defaults for a plain `go build`/`go run` - which is exactly
// the situation this flag exists to make diagnosable (change
// fix-herdr-resume-delegation: this episode's misdiagnosis turned in part
// on not being able to tell which build of lazyrecall was actually running).
var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lazyrecall: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	global := flag.NewFlagSet("lazyrecall", flag.ContinueOnError)
	jsonFlag := global.Bool("json", false, "machine-readable JSON output")
	noRefresh := global.Bool("no-refresh", false, "skip the automatic refresh before answering")
	allFlag := global.Bool("all", false, "apply no hide rule and show archived sessions")

	// Bare `lazyrecall` is the browser, not a usage dump: the interactive
	// interface is this tool's front door, the same way `lazygit` and
	// `lazydocker` open on theirs. The subcommands below remain the
	// scriptable surface, and `--help` still prints them. cmdBrowse already
	// falls back to a plain listing when output is not a terminal, so
	// `lazyrecall | head` keeps working.
	if len(args) == 0 {
		return cmdBrowse(global, allFlag, noRefresh, nil)
	}
	cmd, rest := args[0], args[1:]
	if cmd == "-h" || cmd == "--help" || cmd == "help" {
		printUsage()
		return nil
	}
	if cmd == "-v" || cmd == "--version" || cmd == "version" {
		fmt.Println(versionString())
		return nil
	}

	// Subcommand-specific flags are parsed after pulling out any global
	// ones interleaved by the user; for simplicity every subcommand parses
	// the same global flag set plus its own.
	switch cmd {
	case "list":
		return cmdList(global, jsonFlag, allFlag, noRefresh, rest)
	case "search":
		return cmdSearch(global, jsonFlag, allFlag, noRefresh, rest)
	case "review":
		return cmdReview(global, jsonFlag, allFlag, noRefresh, rest)
	case "resume":
		return cmdResume(global, jsonFlag, noRefresh, rest)
	case "browse":
		return cmdBrowse(global, allFlag, noRefresh, rest)
	case "comment":
		return cmdComment(global, noRefresh, rest)
	case "tag":
		return cmdTag(global, noRefresh, rest)
	case "archive":
		return cmdArchive(global, noRefresh, rest)
	case "unarchive":
		return cmdUnarchive(global, noRefresh, rest)
	case "group":
		return cmdGroup(global, noRefresh, rest)
	case "refresh":
		return cmdRefresh(global, rest)
	case "groups":
		return cmdGroups(global, jsonFlag, noRefresh, rest)
	case "config":
		return cmdConfig(global, rest)
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// versionString is the one-line identifier `lazyrecall --version`/`-v`/
// `version` prints - enough to tell which build is actually running
// (name, version, build time), the diagnostic this change adds because its
// own debugging episode needed it and didn't have it.
func versionString() string {
	return fmt.Sprintf("lazyrecall %s (built %s)", version, buildTime)
}

func printUsage() {
	fmt.Fprint(os.Stderr, `lazyrecall - a cross-agent index over coding-agent sessions

Usage:
  lazyrecall                  open the interactive browser
  lazyrecall list      [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--group=NAME] [--since=DAYS] [--all] [--json]
  lazyrecall search    QUERY [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--group=NAME] [--all] [--json]
  lazyrecall review    [--group=NAME] [--all] [--json]
  lazyrecall resume    [SESSION_ID]
  lazyrecall comment   add SESSION_ID TEXT... | list SESSION_ID | rm COMMENT_ID
  lazyrecall tag       add SESSION_ID TAG | rm SESSION_ID TAG | list
  lazyrecall archive   SESSION_ID | list
  lazyrecall unarchive SESSION_ID
  lazyrecall group     SESSION_ID NAME | SESSION_ID archive | SESSION_ID --auto
  lazyrecall refresh   [--full]
  lazyrecall browse    [QUERY] [--agent=NAME] [--client=NAME] [--repo=PATH] [--tag=NAME] [--group=NAME] [--all]
  lazyrecall groups    [--json]
  lazyrecall config    path|init|show
  lazyrecall version, --version, -v

With no SESSION_ID, "lazyrecall resume" opens a numbered picker to choose from.
Resuming runs the agent directly in this terminal - nothing else needs to be
installed.

The browser - "lazyrecall" with no arguments, or "lazyrecall browse" - opens on
the most recent sessions from every install and stays open: narrow by agent,
repository, tag, or group from the side panels, read and edit comments/tags,
and resume a session without leaving it.

SESSION_ID accepts either a session's short handle (e.g. "3") or its full
composite identifier.

--group narrows to one view of a session's group (see "lazyrecall groups" for
the configured names): a configured group's name, "archive" for every
archived session, or "unknown" for sessions no group rule or manual choice
has claimed. With no --group, a listing shows every non-archived session
regardless of group - the same as before groups existed.
`)
}

// openDB discovers every install on this machine, refreshes them all
// (unless skipped) into the single index, and returns a runner over it
// (design.md decision 6: every operation refreshes first; change
// group-sessions-in-one-index: there is one index for every install, not
// one active profile to pick). The database program path is gone with the
// preflight check that supplied it (change
// replace-sqlite-subprocess-with-driver): the driver is compiled in, so
// refresh.New receives an empty path it ignores.
func openDB(skipRefresh bool, full bool) (*sqlitex.Runner, error) {
	installs, err := profile.Discover()
	if err != nil {
		return nil, err
	}
	r, err := refresh.New(installs, "")
	if err != nil {
		return nil, err
	}
	if !skipRefresh {
		if _, err := r.Refresh(refresh.Options{FullRebuild: full}); err != nil {
			return nil, fmt.Errorf("refreshing index: %w", err)
		}
	}
	return r.DB, nil
}

// parseInterleaved parses fs against args, accepting flags before, after,
// and between positional arguments (spec session-search, "Argument order
// does not change a command's meaning"; design.md decision 1). The
// standard library's flag.Parse stops at the first non-flag argument by
// design, so a command's documented usage - flags after a positional, e.g.
// "lazyrecall search buddy --profile=X" - would otherwise be swallowed whole
// into the positional. This is the standard re-parse idiom: parse, peel off
// one positional when flag.Parse stops on one, and parse whatever remains,
// repeating until nothing is left.
//
// The end-of-options marker "--" is honoured explicitly rather than relying
// on flag.Parse's own handling of it, because flag.Parse consumes "--"
// itself and forgets it happened - a second Parse call on what's left after
// "--" would go right back to interpreting a leading "-" as a flag, which
// is exactly the "text that resembles an option" scenario the spec requires
// to stay text. Splitting on the first literal "--" before any parsing
// begins, and never feeding what comes after it back through flag.Parse at
// all, keeps that guarantee regardless of how many re-parse iterations run
// before it.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	before, after, hasDoubleDash := splitDoubleDash(args)

	var positionals []string
	remaining := before
	for {
		if err := fs.Parse(remaining); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		remaining = rest[1:]
	}
	if hasDoubleDash {
		positionals = append(positionals, after...)
	}
	return positionals, nil
}

// splitDoubleDash splits args on the first literal "--" element, reporting
// whether one was found. The marker itself is not included in either half.
func splitDoubleDash(args []string) (before, after []string, found bool) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:], true
		}
	}
	return args, nil, false
}

type filterFlags struct {
	agent  *string
	client *string
	repo   *string
	tag    *string
	group  *string
	since  *int
}

func parseFilterFlags(fs *flag.FlagSet) filterFlags {
	return filterFlags{
		agent:  fs.String("agent", "", "filter by agent (claude, pi, omp, hermes, goose, opencode, kilo, antigravity) or install/label"),
		client: fs.String("client", "", "filter by the program the session was driven through (acp, sdk, cli)"),
		repo:   fs.String("repo", "", "filter by repository root or working directory"),
		tag:    fs.String("tag", "", "filter by tag"),
		group:  fs.String("group", "", "filter by group name, 'archive', or 'unknown' (see 'lazyrecall groups')"),
		since:  fs.Int("since", 0, "only sessions active in the last N days"),
	}
}

// buildFilter turns the parsed filter flags into a search.Filter, resolving
// --agent to Filter.Agent and/or Filter.Install (resolveAgentFilter) and
// validating --group against cfg's configured groups (search.ValidateGroup)
// - the CLI's one gate for a bad --group value, since every caller below
// goes through this rather than setting Filter.Group directly.
func buildFilter(cfg config.Config, ff filterFlags) (search.Filter, error) {
	if err := search.ValidateGroup(cfg, *ff.group); err != nil {
		return search.Filter{}, err
	}
	agent, install := resolveAgentFilter(cfg, *ff.agent)
	f := search.Filter{
		Agent:   agent,
		Install: install,
		Client:  *ff.client,
		Repo:    *ff.repo,
		Tag:     *ff.tag,
		Group:   *ff.group,
		Groups:  cfg.Groups,
	}
	if *ff.since > 0 {
		t := time.Now().Add(-time.Duration(*ff.since) * 24 * time.Hour)
		f.Since = &t
	}
	return f, nil
}

// resolveAgentFilter decides what a --agent value names and returns the
// search.Filter field to set it on (change group-sessions-in-one-index,
// follow-up fix): Filter.Agent ("every install of this source") and
// Filter.Install ("this one install specifically") can no longer be tried
// interchangeably the way a single combined field once was, because an
// install's name can equal its own source's name - one claude install is
// literally named "claude" - which made a label over *that* install
// (e.g. "cc") indistinguishable from "every claude session": the OR'd
// source-match alone already covered it.
//
// Resolution order, first match wins:
//  1. arg is a configured label of a discovered install -> Filter.Install
//     (a label was written specifically to narrow to one account). This
//     step uses Profile.ConfiguredLabel, not Profile.Label: Label falls back
//     to the install's own name when no label is configured, and matching
//     against that fallback was a P1 bug - with no [labels] table at all,
//     the install named "claude" was mistaken for a label of itself, so
//     --agent=claude resolved to Filter.Install="claude" (one install) in
//     step 1, never reaching step 2's Filter.Agent="claude" (every claude
//     install) - silently narrower than the README promises, and invisible
//     in any test that always configured labels.
//  2. arg is a configured source name -> Filter.Agent, so --agent=claude
//     keeps meaning every claude account, exactly as before labels existed.
//  3. arg is a discovered install's own name -> Filter.Install.
//  4. otherwise -> Filter.Agent, unchanged: an unrecognised value behaves
//     exactly as it always has (matching nothing).
//
// A discovery failure at steps 1/3 is not fatal - those steps simply
// resolve nothing, and arg falls through toward step 4, the same fallback
// openDB's own discovery failure would surface more loudly a moment later
// anyway.
func resolveAgentFilter(cfg config.Config, arg string) (agent, install string) {
	if arg == "" {
		return "", ""
	}
	installs, discErr := profile.Discover()
	if discErr == nil {
		for _, p := range installs {
			if label, ok := p.ConfiguredLabel(cfg); ok && label == arg {
				return "", p.Name
			}
		}
	}
	if _, ok := cfg.Sources[arg]; ok {
		return arg, ""
	}
	if discErr == nil {
		for _, p := range installs {
			if p.Name == arg {
				return "", p.Name
			}
		}
	}
	return arg, ""
}

// groupLabel is what a listing's header note and JSON envelope call the
// scope it ran under: the group name it was filtered to, or "all" for no
// filter - the group-sessions-in-one-index replacement for the active
// profile name those same places used to show.
func groupLabel(f search.Filter) string {
	if f.Group == "" {
		return "all"
	}
	return f.Group
}

func cmdList(global *flag.FlagSet, jsonFlag, allFlag, noRefresh *bool, args []string) error {
	ff := parseFilterFlags(global)
	if _, err := parseInterleaved(global, args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	f, err := buildFilter(cfg, ff)
	if err != nil {
		return err
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}
	f.Hide = cfg.Hide
	f.ShowAll = *allFlag
	items, hidden, err := search.ListWithHidden(db, f)
	if err != nil {
		return err
	}
	return outputItems(items, *jsonFlag, hidden, search.EmptyMessage(f, ""), groupLabel(f), installLabelsForCLI(cfg), cfg.Groups, cfg.ArchiveColor, cfg.UnknownColor)
}

func cmdSearch(global *flag.FlagSet, jsonFlag, allFlag, noRefresh *bool, args []string) error {
	ff := parseFilterFlags(global)
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: lazyrecall search QUERY")
	}
	query := strings.Join(rest, " ")

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	f, err := buildFilter(cfg, ff)
	if err != nil {
		return err
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}
	f.Hide = cfg.Hide
	f.ShowAll = *allFlag
	items, hidden, err := search.SearchWithHidden(db, query, f)
	if err != nil {
		return err
	}
	return outputItems(items, *jsonFlag, hidden, search.EmptyMessage(f, query), groupLabel(f), installLabelsForCLI(cfg), cfg.Groups, cfg.ArchiveColor, cfg.UnknownColor)
}

func cmdReview(global *flag.FlagSet, jsonFlag, allFlag, noRefresh *bool, args []string) error {
	groupFlag := global.String("group", "", "filter by group name, 'archive', or 'unknown' (see 'lazyrecall groups')")
	if err := global.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := search.ValidateGroup(cfg, *groupFlag); err != nil {
		return err
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}
	f := search.Filter{Hide: cfg.Hide, ShowAll: *allFlag, Group: *groupFlag, Groups: cfg.Groups}
	entries, hidden, err := review.ReportWithHidden(db, f)
	if err != nil {
		return err
	}
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		items := make([]search.Item, len(entries))
		for i, e := range entries {
			items[i] = e.Item
		}
		type envelope struct {
			Group string `json:"group"`
			Items []any  `json:"items"`
		}
		return enc.Encode(envelope{Group: groupLabel(f), Items: cli.ToJSONItems(items)})
	}
	printHiddenNote(hidden)
	opts := cli.DetermineOptions(os.Stdout)
	opts.InstallLabels = installLabelsForCLI(cfg)
	opts.GroupColors = cli.GroupColors(cfg.Groups, cfg.ArchiveColor, cfg.UnknownColor)
	cli.WriteReviewHuman(os.Stdout, entries, review.EmptyMessage(groupLabel(f)), opts)
	return nil
}

func cmdResume(global *flag.FlagSet, jsonFlag, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}

	var target search.Item
	if len(rest) > 0 {
		// Accepts either the short handle or the fully-qualified session
		// identifier (spec session-search, "Short session handle";
		// design.md decision 4) - disambiguated purely by shape, so a
		// handle that resolves to nothing is reported rather than
		// silently falling through to act on some other session.
		item, found, err := search.ItemForIdentifier(db, rest[0], cfg.Groups)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%q does not resolve to any session", rest[0])
		}
		target = item
	} else {
		items, err := search.List(db, search.Filter{Groups: cfg.Groups})
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println(search.EmptyMessage(search.Filter{}, ""))
			return nil
		}
		chosen, ok, err := cli.Pick(items, os.Stdin, os.Stdout)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		target = chosen
	}

	// Resume replaces this process with the agent on success (change
	// resume-in-current-terminal, design.md decision 1), so anything below
	// this call only ever runs on a failure path - there is no LazyRecall left
	// to report success from.
	tpl, profileEnv, profileOK, profileReason := resumeSource(target.Source, session.InstallFromID(target.SessionID))
	out := resume.Resume(resume.Target{
		Source: target.Source, SourceSessionID: sourceSessionIDFrom(target.SessionID),
		CWD: target.CWD, GitRepoRoot: target.GitRepoRoot, DirExists: target.DirExists,
		Resumable:               target.Resumable,
		Resume:                  tpl,
		ProfileEnv:              profileEnv,
		ProfileUnresolved:       !profileOK,
		ProfileUnresolvedReason: profileReason,
	})
	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Println(out.Message)
	return nil
}

// sourceSessionIDFrom recovers the source's own session id from LazyRecall's
// composite id ("source:profile:sourceSessionID").
func sourceSessionIDFrom(compositeID string) string {
	parts := strings.SplitN(compositeID, ":", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return compositeID
}

// resumeSource resolves everything a session's source needs to be resumed
// from a single config load: the argv template the resume command is built
// from (cfg.Sources[source].Resume, passed through as Target.Resume) and
// the environment override for the session's own install (profileEnvFor).
// The two come from the same config file, so resolving them together keeps
// the resume command from reading and parsing it twice. An empty template
// means the source has no configured resume command; resume reports that
// source as one it does not know how to resume.
func resumeSource(source, installName string) (tpl []string, env map[string]string, ok bool, reason string) {
	cfg, err := config.Load()
	if err != nil {
		return nil, nil, false, fmt.Sprintf("loading config: %v", err)
	}
	tpl = cfg.Sources[source].Resume
	env, ok, reason = profileEnvFor(cfg, source, installName)
	return tpl, env, ok, reason
}

// profileEnvFor resolves the environment overrides needed to start source's
// agent against the installation installName's session belongs to
// (design.md decision 3: apply the session's install configuration to the
// environment). Which env var points at the install's root is configured
// per source (internal/config: a source's EnvVar). A source with no EnvVar
// never varies by install on this machine - each single_install source has
// exactly one (internal/profile.Discover) - so it returns ok=true with a
// nil env unconditionally, leaving other sources unaffected (task 2.2).
// ok=false means the install's configuration could not be determined; the
// caller must not start the agent in that case (task 2.3). installName is
// read off the session itself (session.InstallFromID), not assumed from any
// command-line flag, because a session shown in one browsing session can
// belong to any install once one index holds them all together (change
// group-sessions-in-one-index, carrying forward fix-resume-session-identity,
// design.md decision 3).
func profileEnvFor(cfg config.Config, source, installName string) (env map[string]string, ok bool, reason string) {
	envVar := cfg.Sources[source].EnvVar
	if envVar == "" {
		return nil, true, ""
	}
	discovered, err := profile.Discover()
	if err != nil {
		return nil, false, fmt.Sprintf("discovering installs: %v", err)
	}
	for _, p := range discovered {
		if p.Name != installName {
			continue
		}
		root := p.Roots[source]
		if root == "" {
			return nil, false, fmt.Sprintf("install %q has no %s configuration root", installName, source)
		}
		return map[string]string{envVar: root}, true, ""
	}
	return nil, false, fmt.Sprintf("no discovered install named %q", installName)
}

func cmdComment(global *flag.FlagSet, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: lazyrecall comment add|list|rm ...")
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}

	switch rest[0] {
	case "add":
		if len(rest) < 3 {
			return fmt.Errorf("usage: lazyrecall comment add SESSION_ID TEXT...")
		}
		lineage, err := annotate.LineageForIdentifier(db, rest[1])
		if err != nil {
			return err
		}
		return annotate.AddComment(db, lineage, strings.Join(rest[2:], " "))
	case "list":
		if len(rest) < 2 {
			return fmt.Errorf("usage: lazyrecall comment list SESSION_ID")
		}
		lineage, err := annotate.LineageForIdentifier(db, rest[1])
		if err != nil {
			return err
		}
		comments, err := annotate.CommentsForLineage(db, lineage)
		if err != nil {
			return err
		}
		if len(comments) == 0 {
			fmt.Println("No comments.")
		}
		for _, c := range comments {
			fmt.Printf("[%d] %s: %s\n", c.ID, c.CreatedAt.Format(time.RFC3339), c.Body)
		}
		return nil
	case "rm":
		if len(rest) < 2 {
			return fmt.Errorf("usage: lazyrecall comment rm COMMENT_ID")
		}
		id, err := strconv.ParseInt(rest[1], 10, 64)
		if err != nil {
			return fmt.Errorf("invalid comment id %q", rest[1])
		}
		return annotate.RemoveComment(db, id)
	default:
		return fmt.Errorf("unknown comment subcommand %q", rest[0])
	}
}

func cmdTag(global *flag.FlagSet, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: lazyrecall tag add|rm|list ...")
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}

	switch rest[0] {
	case "add":
		if len(rest) < 3 {
			return fmt.Errorf("usage: lazyrecall tag add SESSION_ID TAG")
		}
		lineage, err := annotate.LineageForIdentifier(db, rest[1])
		if err != nil {
			return err
		}
		return annotate.AddTag(db, lineage, rest[2])
	case "rm":
		if len(rest) < 3 {
			return fmt.Errorf("usage: lazyrecall tag rm SESSION_ID TAG")
		}
		lineage, err := annotate.LineageForIdentifier(db, rest[1])
		if err != nil {
			return err
		}
		return annotate.RemoveTag(db, lineage, rest[2])
	case "list":
		tags, err := annotate.AllTags(db)
		if err != nil {
			return err
		}
		for _, t := range tags {
			fmt.Println(t)
		}
		return nil
	default:
		return fmt.Errorf("unknown tag subcommand %q", rest[0])
	}
}

// cmdArchive implements `lazyrecall archive SESSION_ID` and `lazyrecall
// archive list` (change add-archive-facility). Archiving a session is a
// decision the user made, recorded on the lineage (annotate.Archive) so a
// full index rebuild cannot destroy it. `list` renders the archived
// sessions through the normal row path rather than a bespoke format.
func cmdArchive(global *flag.FlagSet, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: lazyrecall archive SESSION_ID|list")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}

	if rest[0] == "list" {
		ids, err := annotate.AllArchived(db)
		if err != nil {
			return err
		}
		items, err := search.ListByLineageIDs(db, ids, cfg.Groups)
		if err != nil {
			return err
		}
		return outputItems(items, false, 0, "No archived sessions.", "archive", installLabelsForCLI(cfg), cfg.Groups, cfg.ArchiveColor, cfg.UnknownColor)
	}

	// SESSION_ID accepts the short handle exactly like every other
	// command (annotate.LineageForIdentifier), so "archive 3" names the
	// same session "comment add 3 ..." does.
	lineage, err := annotate.LineageForIdentifier(db, rest[0])
	if err != nil {
		return err
	}
	return annotate.Archive(db, lineage)
}

// cmdUnarchive implements `lazyrecall unarchive SESSION_ID`, returning an
// archived session to normal listings.
func cmdUnarchive(global *flag.FlagSet, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: lazyrecall unarchive SESSION_ID")
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}
	lineage, err := annotate.LineageForIdentifier(db, rest[0])
	if err != nil {
		return err
	}
	return annotate.Unarchive(db, lineage)
}

// cmdGroup implements `lazyrecall group SESSION_ID NAME`, `lazyrecall group
// SESSION_ID archive`, and `lazyrecall group SESSION_ID --auto` (change
// group-sessions-in-one-index) - the CLI surface for annotate.SetGroup,
// which also backs the browser's `p` popup in Stage E. SESSION_ID accepts
// the short handle exactly like every other annotation command.
func cmdGroup(global *flag.FlagSet, noRefresh *bool, args []string) error {
	auto := global.Bool("auto", false, "clear the manual override, returning to the automatic path-based group")
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 || (!*auto && len(rest) < 2) {
		return fmt.Errorf("usage: lazyrecall group SESSION_ID NAME|archive|--auto")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}
	lineage, err := annotate.LineageForIdentifier(db, rest[0])
	if err != nil {
		return err
	}

	choice := ""
	label := "automatic"
	if !*auto {
		choice = rest[1]
		label = choice
	}
	if err := annotate.SetGroup(db, cfg, lineage, choice); err != nil {
		return err
	}
	fmt.Printf("lin %s -> %s\n", rest[0], label)
	return nil
}

func cmdRefresh(global *flag.FlagSet, args []string) error {
	full := global.Bool("full", false, "discard the index and rebuild it from scratch")
	if err := global.Parse(args); err != nil {
		return err
	}
	installs, err := profile.Discover()
	if err != nil {
		return err
	}
	r, err := refresh.New(installs, "")
	if err != nil {
		return err
	}
	sum, err := r.Refresh(refresh.Options{FullRebuild: *full})
	if err != nil {
		return err
	}
	for _, s := range sum.Sources {
		label := s.Source
		if s.Install != "" {
			label = s.Source + " (" + s.Install + ")"
		}
		if s.Available {
			fmt.Printf("  %s: %d sessions\n", label, s.Sessions)
		} else {
			fmt.Printf("  %s: unavailable (%v)\n", label, s.Err)
		}
	}
	return nil
}

// cmdGroups implements `lazyrecall groups` (change
// group-sessions-in-one-index), replacing the old `profiles` command now
// that a session's group and the install that produced it are two separate
// questions: each configured group in config order with its paths and
// non-archived session count, then the archive and unknown counts, then an
// installs section naming each discovered install by its display label so
// "which account" is still answerable without a group of its own. --json
// carries the same data as a minimal object; counts and layout polish is
// Phase 2 (see the plan).
func cmdGroups(global *flag.FlagSet, jsonFlag, noRefresh *bool, args []string) error {
	if err := global.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}
	counts, err := search.Counts(db, cfg.Hide, cfg.Groups)
	if err != nil {
		return err
	}
	installs, err := profile.Discover()
	if err != nil {
		return err
	}

	if *jsonFlag {
		return writeGroupsJSON(cfg, counts, installs)
	}
	writeGroupsHuman(cfg, counts, installs)
	return nil
}

func writeGroupsHuman(cfg config.Config, counts search.GroupCounts, installs []profile.Profile) {
	for _, g := range cfg.Groups {
		fmt.Printf("%s (%s): %d\n", g.Name, strings.Join(g.Paths, ", "), counts.ByGroup[g.Name])
	}
	fmt.Printf("archive %d\n", counts.Archive)
	if counts.Unknown > 0 {
		fmt.Printf("unknown %d\n", counts.Unknown)
	}
	if len(installs) == 0 {
		return
	}
	fmt.Println()
	fmt.Println("installs:")
	for _, p := range installs {
		fmt.Printf("  %s: %s (%s, %s)\n", p.Label(cfg), p.Name, p.Source(), p.Root())
	}
}

func writeGroupsJSON(cfg config.Config, counts search.GroupCounts, installs []profile.Profile) error {
	type groupOut struct {
		Name  string   `json:"name"`
		Paths []string `json:"paths"`
		Count int      `json:"count"`
	}
	type installOut struct {
		Label   string `json:"label"`
		Install string `json:"install"`
		Source  string `json:"source"`
		Root    string `json:"root"`
	}
	groups := make([]groupOut, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		groups = append(groups, groupOut{Name: g.Name, Paths: g.Paths, Count: counts.ByGroup[g.Name]})
	}
	outInstalls := make([]installOut, 0, len(installs))
	for _, p := range installs {
		outInstalls = append(outInstalls, installOut{Label: p.Label(cfg), Install: p.Name, Source: p.Source(), Root: p.Root()})
	}
	out := struct {
		Groups   []groupOut   `json:"groups"`
		Archive  int          `json:"archive"`
		Unknown  int          `json:"unknown"`
		Installs []installOut `json:"installs"`
	}{Groups: groups, Archive: counts.Archive, Unknown: counts.Unknown, Installs: outInstalls}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// discoverInstallsForBrowser feeds the browser's Transcript tab, which
// needs to resolve a database-backed session's own install (session.
// InstallFromID) rather than any single "active profile" - a notion that no
// longer exists once one browsing session covers every install's data
// together (change group-sessions-in-one-index). It takes a plain slice
// with no error slot: a discovery failure here is best-effort (the
// Transcript tab simply can't resolve the install), while every command
// path surfaces the same failure through openDB.
func discoverInstallsForBrowser() []profile.Profile {
	installs, _ := profile.Discover()
	return installs
}

// installLabelsForCLI computes the install-name -> display-label map every
// row renderer needs for its badge (cli.InstallLabels) - the CLI's own text
// output (list/search/review/archive list/browse's non-terminal fallback)
// and the interactive browser both go through the same map, built once per
// command rather than once per row. A discovery failure here is not fatal:
// the caller already surfaces the same failure more loudly through openDB,
// and a listing with no label map simply falls back to showing bare source
// names, exactly as it always has.
func installLabelsForCLI(cfg config.Config) map[string]string {
	installs, _ := profile.Discover()
	return cli.InstallLabels(installs, cfg)
}

// ---------------------------------------------------------------------
// lazyrecall config (change sources-become-data)
// ---------------------------------------------------------------------

// cmdConfig implements `lazyrecall config path|init|show`: path prints
// where the config file resolves to and whether it exists, init writes a
// commented-out default config there (refusing to clobber an existing
// one), and show prints the effective config with each value's provenance.
// The provenance display exists because the precedence chain (flag > env >
// file > default) is otherwise something every reader has to reconstruct
// by hand; the Origins map lets this command say where each value came
// from instead (internal/config).
func cmdConfig(global *flag.FlagSet, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: lazyrecall config path|init|show")
	}
	switch rest[0] {
	case "path":
		path := config.Path()
		fmt.Println(path)
		if _, err := os.Stat(path); err == nil {
			fmt.Println("exists")
		} else if os.IsNotExist(err) {
			fmt.Println("does not exist")
		} else {
			return err
		}
		return nil
	case "init":
		path := config.Path()
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config file already exists at %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(defaultConfigFile), 0o644); err != nil {
			return err
		}
		fmt.Printf("wrote %s\n", path)
		return nil
	case "show":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return showConfig(cfg)
	default:
		return fmt.Errorf("unknown config subcommand %q", rest[0])
	}
}

// showConfig prints every configurable key with its effective value and
// provenance, in sorted-key order, so the whole effective config is visible
// at once with each value attributed to the layer that set it.
func showConfig(cfg config.Config) error {
	values := map[string]any{
		"hide.non_interactive": cfg.Hide.NonInteractive,
		"hide.min_messages":    cfg.Hide.MinMessages,
		"hide.paths":           cfg.Hide.Paths,
		"browse.show_archived": cfg.Browse.ShowArchived,
		"browse.default_group": cfg.Browse.DefaultGroup,
		"labels":               cfg.Labels,
	}
	names := make([]string, 0, len(cfg.Sources))
	for name := range cfg.Sources {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values["sources."+name+".roots"] = cfg.Sources[name].Roots
		values["sources."+name+".resume"] = cfg.Sources[name].Resume
		values["sources."+name+".env_var"] = cfg.Sources[name].EnvVar
	}
	// Groups have no default entry to fall back on - only a config file
	// puts one there - so with none configured, no "groups.*" key appears
	// at all, rather than printing an empty placeholder.
	for _, g := range cfg.Groups {
		values["groups."+g.Name+".paths"] = g.Paths
		values["groups."+g.Name+".color"] = g.Color
	}
	// ArchiveColor and UnknownColor follow the same rule as a group's own
	// color: no default, so only a table actually declared in the file
	// gives one of these keys an entry at all - checked via Origins, like
	// cfg.Groups above is implicitly, rather than by the color being
	// non-empty, since `[groups.archive]` with no color key is still
	// file-owned and still worth showing as "" (change
	// archive-unknown-colors).
	if _, ok := cfg.Origins["groups.archive.color"]; ok {
		values["groups.archive.color"] = cfg.ArchiveColor
	}
	if _, ok := cfg.Origins["groups.unknown.color"]; ok {
		values["groups.unknown.color"] = cfg.UnknownColor
	}

	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Printf("%s = %v    (%s)\n", k, values[k], cfg.Origins[k])
	}
	return nil
}

// defaultConfigFile is what `lazyrecall config init` writes: the built-in
// defaults as a commented template, so the file the user ends up with is
// exactly the configuration they would otherwise have had implicitly, with
// every knob visible. The table headers are commented out too, not just
// the values: a bare [sources.X] header would make that source file-owned
// and zero every field it omits (internal/config: a source table replaces
// the defaults entirely), so an untouched file must not change behavior -
// it is a starting point for editing, not a new effective config.
const defaultConfigFile = `# LazyRecall configuration (lazyrecall config).
# Every setting here is optional: with no config file, LazyRecall runs on
# exactly these defaults. Uncomment a line to change it.

# [sources.claude]
# roots = ["~/.claude-personal", "~/.claude"]
# resume = ["claude", "--resume", "{id}"]
# env_var = "CLAUDE_CONFIG_DIR"

# [sources.pi]
# roots = ["~/.pi"]
# resume = ["pi", "--session", "{id}"]

# [sources.omp]
# roots = ["~/.omp"]
# resume = ["omp", "--resume", "{id}"]

# [sources.hermes]
# roots = ["~/.hermes"]
# resume = ["hermes", "--resume", "{id}"]

# [sources.goose]
# roots = ["~/.local/share/goose/sessions"]
# resume = ["goose", "session", "--resume", "--session-id", "{id}"]

# [sources.opencode]
# roots = ["~/.local/share/opencode"]
# resume = ["opencode", "--session", "{id}"]

# [sources.kilo]
# roots = ["~/.local/share/kilo"]
# resume = ["kilo", "--session", "{id}"]

# [sources.antigravity]
# roots = ["~/.gemini/antigravity-cli"]
# resume = ["agy", "--conversation", "{id}"]

# Labels give an install a short display name and a matching --agent value,
# without deciding its group (see [groups.*] below) - keyed by config root,
# a top-level table on purpose: a [sources.X] table above replaces that
# source's whole configuration, roots included, so labels cannot live there
# without risking silently dropping an agent from discovery the moment its
# labels are configured.
# [labels]
# "~/.claude" = "cc"
# "~/.claude-personal" = "ccp"

# [hide]
# non_interactive = true
# min_messages = 0
# paths = []

# NOTE: raising hide.min_messages above 0 can hide a genuinely dangling
# session. A dangling session is by definition a short one - the exchange
# stopped mid-turn - so a threshold that hides short sessions also hides
# real problems from the review listing.

# Groups sort sessions by working directory, at query time - editing this
# section regroups every session immediately, with no refresh needed. A
# session under none of these paths is Unknown; one under more than one is
# claimed by whichever path is the longest match. With no [groups.*] tables
# at all, LazyRecall looks exactly as it does with this file absent.
# A group's color is optional and applies to its name in the Groups panel
# and to the handle of every session filed under it (browse and the list
# command alike): an ANSI name (black, red, green, yellow, blue, magenta, cyan,
# white), a bright-prefixed variant of one (e.g. bright-blue), or a 24-bit
# hex triplet like #3355ff. Case-insensitive; with none set, a group's rows
# keep today's default styling.
# [groups.work]
# paths = ["~/code", "~/work"]
# color = "blue"

# [groups.personal]
# paths = ["~/dotfiles", "~/personal"]
# color = "#22aa88"

# "archive" and "unknown" are reserved names - they cannot be real groups -
# but [groups.archive] and [groups.unknown] may still set a color for the
# two built-in views: every archived session, and every session no group
# rule or manual choice has claimed. Only color is allowed here; a session
# handle with no color from any of these rules is white.
# [groups.archive]
# color = "red"

# [groups.unknown]
# color = "white"

# [browse]
# show_archived = false
# default_group = ""
`

// outputItems writes a listing either as JSON - an envelope naming the
// group it was filtered to (change group-sessions-in-one-index: restores
// the shape `{"profile": ..., "items": [...]}` had before Stage B dropped
// it for a bare array, renamed to "group" now that there is no more single
// active profile) - or as the human-readable listing, preceded by the
// hidden-count note.
func outputItems(items []search.Item, jsonOut bool, hidden int, emptyMessage string, group string, labels map[string]string, groups []config.Group, archiveColor, unknownColor string) error {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		type envelope struct {
			Group string `json:"group"`
			Items []any  `json:"items"`
		}
		return enc.Encode(envelope{Group: group, Items: cli.ToJSONItems(items)})
	}
	printHiddenNote(hidden)
	opts := cli.DetermineOptions(os.Stdout)
	opts.InstallLabels = labels
	// GroupColors is computed once here, not per row - the same discipline
	// InstallLabels already follows (see cli.RenderOptions.GroupColors).
	opts.GroupColors = cli.GroupColors(groups, archiveColor, unknownColor)
	cli.WriteItemsHuman(os.Stdout, items, emptyMessage, opts)
	return nil
}

// printHiddenNote is what is left of the old "Profile: NAME" header once
// there is no longer a single active profile to name (change
// group-sessions-in-one-index): just the part that was never about the
// profile - how many sessions the standing hide rules suppressed, so hiding
// is never silent. Nothing is printed when nothing was hidden, and the note
// never appears in --all mode, where nothing is suppressed.
func printHiddenNote(hidden int) {
	if hidden > 0 {
		fmt.Printf("%d hidden (--all to show)\n", hidden)
	}
}

// ---------------------------------------------------------------------
// lazyrecall browse (change add-interactive-browse)
// ---------------------------------------------------------------------

// cmdBrowse opens the interactive browser (change
// replace-fzf-browser-with-tui): the index is refreshed once here at open
// (design.md decision 5 - refresh once on open, never per interaction),
// then the browser runs entirely in-process, holding all of its state in
// the running program. When output is not a terminal there is nothing for
// an interactive interface to draw on, which is a permanent condition
// rather than an installation gap (design.md decision 7): the system
// reports that browsing requires a terminal and prints the equivalent
// non-interactive listing.
func cmdBrowse(global *flag.FlagSet, allFlag, noRefresh *bool, args []string) error {
	ff := parseFilterFlags(global)
	repoF, tag := ff.repo, ff.tag
	positionals, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	query := strings.Join(positionals, " ")

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := search.ValidateGroup(cfg, *ff.group); err != nil {
		return err
	}
	// "if not given, use cfg.Browse.DefaultGroup" (change
	// group-sessions-in-one-index): --group and the config default share the
	// same empty-string zero value, so an unset flag falls through to the
	// configured default exactly as an explicit --group="" would - the only
	// value that distinction could matter for is already All either way.
	group := *ff.group
	if group == "" {
		group = cfg.Browse.DefaultGroup
	}
	agentResolved, installResolved := resolveAgentFilter(cfg, *ff.agent)
	// Computed once per command, from the same install discovery openDB and
	// resolveAgentFilter each already perform, and handed to both branches
	// below (cli.InstallLabels): the interactive browser's rows and the
	// non-interactive fallback listing must show the same badges (change
	// group-sessions-in-one-index).
	labels := installLabelsForCLI(cfg)

	if !cli.IsTerminal(os.Stdout) {
		fmt.Fprintln(os.Stderr, "browsing requires a terminal; showing a non-interactive listing instead.")
		db, err := openDB(*noRefresh, false)
		if err != nil {
			return err
		}
		f := search.Filter{Agent: agentResolved, Install: installResolved, Client: *ff.client, Repo: *repoF, Tag: *tag, Hide: cfg.Hide, ShowAll: *allFlag, Group: group, Groups: cfg.Groups}
		var items []search.Item
		var hidden int
		if query != "" {
			items, hidden, err = search.SearchWithHidden(db, query, f)
		} else {
			items, hidden, err = search.ListWithHidden(db, f)
		}
		if err != nil {
			return err
		}
		return outputItems(items, false, hidden, search.EmptyMessage(f, query), groupLabel(f), labels, cfg.Groups, cfg.ArchiveColor, cfg.UnknownColor)
	}

	db, err := openDB(*noRefresh, false)
	if err != nil {
		return err
	}

	selected, ok, err := cli.RunBrowser(cli.BrowserOptions{
		DB:      db,
		Repo:    *repoF,
		Agent:   agentResolved,
		Install: installResolved,
		Tag:     *tag,
		Client:  *ff.client,
		Query:   query,
		Style:   os.Getenv("NO_COLOR") == "",
		// The browser opens under the same hide rules the non-interactive
		// commands use, and starts showing everything when the user asked
		// for it either on this command line or in the config file - the
		// browse.show_archived preference, and --all here, are the same
		// "show me everything" choice.
		ShowAll:       *allFlag || cfg.Browse.ShowArchived,
		Hide:          cfg.Hide,
		Group:         group,
		Groups:        cfg.Groups,
		ArchiveColor:  cfg.ArchiveColor,
		UnknownColor:  cfg.UnknownColor,
		Installs:      discoverInstallsForBrowser,
		InstallLabels: labels,
	})
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	// The selected session may belong to any install - the browser lists
	// every install's sessions together now that one index holds them all
	// (change group-sessions-in-one-index) - so resume against the install
	// named in the session's own composite id, not any single "active"
	// one. This is the same resume path the non-interactive `lazyrecall
	// resume` uses.
	//
	// cli.RunBrowser has already returned by this point, which is exactly
	// what makes the terminal-restoration ordering correct (change
	// resume-in-current-terminal, design.md decision 3): bubbletea tears
	// down the alternate screen and restores terminal modes as part of
	// p.Run() returning, so that has already happened before Resume can
	// replace this process - there is no later point at which it could
	// still be undone.
	tpl, profileEnv, profileOK, profileReason := resumeSource(selected.Source, session.InstallFromID(selected.SessionID))
	out := resume.Resume(resume.Target{
		Source: selected.Source, SourceSessionID: sourceSessionIDFrom(selected.SessionID),
		CWD: selected.CWD, GitRepoRoot: selected.GitRepoRoot, DirExists: selected.DirExists,
		Resumable:               selected.Resumable,
		Resume:                  tpl,
		ProfileEnv:              profileEnv,
		ProfileUnresolved:       !profileOK,
		ProfileUnresolvedReason: profileReason,
	})
	fmt.Println(out.Message)
	return nil
}
