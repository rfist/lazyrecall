// Command recall is a cross-agent index over coding-agent sessions already
// written to disk by Claude Code, pi, omp, and hermes. It is read-only with
// respect to every source; the only file it writes is its own per-profile
// database (see internal/profile.DataDir).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"recall/internal/annotate"
	"recall/internal/cli"
	"recall/internal/profile"
	"recall/internal/refresh"
	"recall/internal/resume"
	"recall/internal/review"
	"recall/internal/search"
	"recall/internal/sqlitex"
)

// version and buildTime are set at build time via
// `-ldflags "-X main.version=... -X main.buildTime=..."`. Left at their
// zero-value defaults for a plain `go build`/`go run` - which is exactly
// the situation this flag exists to make diagnosable (change
// fix-herdr-resume-delegation: this episode's misdiagnosis turned in part
// on not being able to tell which build of recall was actually running).
var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "recall: "+err.Error())
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		printUsage()
		return nil
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

	global := flag.NewFlagSet("recall", flag.ContinueOnError)
	profileFlag := global.String("profile", "", "profile to operate under (default: the primary profile)")
	jsonFlag := global.Bool("json", false, "machine-readable JSON output")
	noRefresh := global.Bool("no-refresh", false, "skip the automatic refresh before answering")

	// Subcommand-specific flags are parsed after pulling out any global
	// ones interleaved by the user; for simplicity every subcommand parses
	// the same global flag set plus its own.
	switch cmd {
	case "list":
		return cmdList(global, profileFlag, jsonFlag, noRefresh, rest)
	case "search":
		return cmdSearch(global, profileFlag, jsonFlag, noRefresh, rest)
	case "review":
		return cmdReview(global, profileFlag, jsonFlag, noRefresh, rest)
	case "resume":
		return cmdResume(global, profileFlag, jsonFlag, noRefresh, rest)
	case "browse":
		return cmdBrowse(global, profileFlag, noRefresh, rest)
	case "comment":
		return cmdComment(global, profileFlag, noRefresh, rest)
	case "tag":
		return cmdTag(global, profileFlag, noRefresh, rest)
	case "refresh":
		return cmdRefresh(global, profileFlag, rest)
	case "profiles":
		return cmdProfiles(*jsonFlag)
	default:
		printUsage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

// versionString is the one-line identifier `recall --version`/`-v`/
// `version` prints - enough to tell which build is actually running
// (name, version, build time), the diagnostic this change adds because its
// own debugging episode needed it and didn't have it.
func versionString() string {
	return fmt.Sprintf("recall %s (built %s)", version, buildTime)
}

func printUsage() {
	fmt.Fprint(os.Stderr, `recall - a cross-agent index over coding-agent sessions

Usage:
  recall list      [--agent=NAME] [--repo=PATH] [--tag=NAME] [--since=DAYS] [--json] [--profile=NAME]
  recall search    QUERY [--agent=NAME] [--repo=PATH] [--tag=NAME] [--json] [--profile=NAME]
  recall review    [--json] [--profile=NAME]
  recall resume    [SESSION_ID] [--profile=NAME]
  recall comment   add SESSION_ID TEXT... | list SESSION_ID | rm COMMENT_ID
  recall tag       add SESSION_ID TAG | rm SESSION_ID TAG | list
  recall refresh   [--full] [--profile=NAME]
  recall browse    [QUERY] [--agent=NAME] [--repo=PATH] [--tag=NAME] [--profile=NAME]
  recall profiles  [--json]
  recall version, --version, -v

With no SESSION_ID, "recall resume" opens a numbered picker to choose from.
Resuming runs the agent directly in this terminal - nothing else needs to be
installed.

"recall browse" opens on the most recent sessions and stays open: change
filters, read and edit comments/tags, and resume a session without leaving
it. SESSION_ID accepts either a session's short handle (e.g. "3") or its
full composite identifier.
`)
}

// resolveProfile discovers profiles and picks the active one, printing it
// is left to callers (spec session-index, "Active profile is visible").
func resolveProfile(requested string) (profile.Profile, error) {
	profiles := profile.Discover()
	return profile.Resolve(profiles, requested)
}

// openDB refreshes (unless skipped) and returns a runner over the active
// profile's database (design.md decision 6: every operation refreshes
// first). The database program path is gone with the preflight check that
// supplied it (change replace-sqlite-subprocess-with-driver): the driver
// is compiled in, so refresh.New receives an empty path it ignores.
func openDB(p profile.Profile, skipRefresh bool, full bool) (*sqlitex.Runner, error) {
	r, err := refresh.New(p, "")
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
// "recall search buddy --profile=X" - would otherwise be swallowed whole
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

func parseFilterFlags(fs *flag.FlagSet) (*string, *string, *string, *int) {
	agent := fs.String("agent", "", "filter by agent (claude, pi, omp, hermes)")
	repo := fs.String("repo", "", "filter by repository root or working directory")
	tag := fs.String("tag", "", "filter by tag")
	since := fs.Int("since", 0, "only sessions active in the last N days")
	return agent, repo, tag, since
}

func buildFilter(agent, repo, tag *string, sinceDays *int) search.Filter {
	f := search.Filter{Agent: *agent, Repo: *repo, Tag: *tag}
	if *sinceDays > 0 {
		t := time.Now().Add(-time.Duration(*sinceDays) * 24 * time.Hour)
		f.Since = &t
	}
	return f
}

func cmdList(global *flag.FlagSet, profileFlag *string, jsonFlag, noRefresh *bool, args []string) error {
	agent, repoF, tag, since := parseFilterFlags(global)
	if _, err := parseInterleaved(global, args); err != nil {
		return err
	}
	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}
	db, err := openDB(p, *noRefresh, false)
	if err != nil {
		return err
	}
	f := buildFilter(agent, repoF, tag, since)
	items, err := search.List(db, f)
	if err != nil {
		return err
	}
	return outputItems(p, items, *jsonFlag, search.EmptyMessage(p.Name, f, ""))
}

func cmdSearch(global *flag.FlagSet, profileFlag *string, jsonFlag, noRefresh *bool, args []string) error {
	agent, repoF, tag, since := parseFilterFlags(global)
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: recall search QUERY")
	}
	query := strings.Join(rest, " ")

	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}
	db, err := openDB(p, *noRefresh, false)
	if err != nil {
		return err
	}
	f := buildFilter(agent, repoF, tag, since)
	items, err := search.Search(db, query, f)
	if err != nil {
		return err
	}
	return outputItems(p, items, *jsonFlag, search.EmptyMessage(p.Name, f, query))
}

func cmdReview(global *flag.FlagSet, profileFlag *string, jsonFlag, noRefresh *bool, args []string) error {
	if err := global.Parse(args); err != nil {
		return err
	}
	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}
	db, err := openDB(p, *noRefresh, false)
	if err != nil {
		return err
	}
	entries, err := review.Report(db)
	if err != nil {
		return err
	}
	if *jsonFlag {
		return jsonEnvelope(p.Name, func() error {
			return cli.WriteReviewJSON(os.Stdout, entries)
		})
	}
	fmt.Printf("Profile: %s\n", p.Name)
	cli.WriteReviewHuman(os.Stdout, entries, review.EmptyMessage(p.Name), cli.DetermineOptions(os.Stdout))
	return nil
}

func cmdResume(global *flag.FlagSet, profileFlag *string, jsonFlag, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}

	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}
	db, err := openDB(p, *noRefresh, false)
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
		item, found, err := search.ItemForIdentifier(db, p.Name, rest[0])
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("%q does not resolve to any session in profile %q", rest[0], p.Name)
		}
		target = item
	} else {
		items, err := search.List(db, search.Filter{})
		if err != nil {
			return err
		}
		if len(items) == 0 {
			fmt.Println(search.EmptyMessage(p.Name, search.Filter{}, ""))
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
	// this call only ever runs on a failure path - there is no Recall left
	// to report success from.
	profileEnv, profileOK, profileReason := resumeProfileEnv(target.Source, profileNameFrom(target.SessionID))
	out := resume.Resume(resume.Target{
		Source: target.Source, SourceSessionID: sourceSessionIDFrom(target.SessionID),
		CWD: target.CWD, GitRepoRoot: target.GitRepoRoot, DirExists: target.DirExists,
		Resumable:               target.Resumable,
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

// sourceSessionIDFrom recovers the source's own session id from Recall's
// composite id ("source:profile:sourceSessionID").
func sourceSessionIDFrom(recallID string) string {
	parts := strings.SplitN(recallID, ":", 3)
	if len(parts) == 3 {
		return parts[2]
	}
	return recallID
}

// profileNameFrom recovers the profile name from Recall's composite id
// ("source:profile:sourceSessionID") - the same composite sourceSessionIDFrom
// reads, just the middle segment instead of the last. Needed because a
// session accepted via the browser may belong to a profile other than the
// one this command opened with (the browser switches profiles in-process),
// so the profile a session's own agent must be started under has to be read
// off the session itself, not assumed from the command's own --profile flag
// (change fix-resume-session-identity, design.md decision 3).
func profileNameFrom(recallID string) string {
	parts := strings.SplitN(recallID, ":", 3)
	if len(parts) == 3 {
		return parts[1]
	}
	return ""
}

// resumeProfileEnv resolves the environment overrides needed to start
// source's agent against the installation profileName's session belongs to
// (design.md decision 3: "internal/profile already models each profile's
// Claude configuration root ... apply the session's profile configuration
// to the environment"). Only claude currently varies by profile on this
// machine - every discovered Claude config root is its own profile
// (internal/profile doc comment), while pi/omp/hermes are each bundled into
// exactly one profile and never duplicated - so every other source returns
// ok=true with a nil env unconditionally (task 2.2: "apply only what the
// session's profile requires, leaving other sources unaffected"). ok=false
// means the profile's configuration could not be determined; the caller
// must not start the agent in that case (task 2.3).
func resumeProfileEnv(source, profileName string) (env map[string]string, ok bool, reason string) {
	if source != "claude" {
		return nil, true, ""
	}
	for _, p := range profile.Discover() {
		if p.Name != profileName {
			continue
		}
		if p.ClaudeRoot == "" {
			return nil, false, fmt.Sprintf("profile %q has no Claude configuration root", profileName)
		}
		return map[string]string{"CLAUDE_CONFIG_DIR": p.ClaudeRoot}, true, ""
	}
	return nil, false, fmt.Sprintf("no discovered profile named %q", profileName)
}

func cmdComment(global *flag.FlagSet, profileFlag *string, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: recall comment add|list|rm ...")
	}
	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}
	db, err := openDB(p, *noRefresh, false)
	if err != nil {
		return err
	}

	switch rest[0] {
	case "add":
		if len(rest) < 3 {
			return fmt.Errorf("usage: recall comment add SESSION_ID TEXT...")
		}
		lineage, err := annotate.LineageForIdentifier(db, p.Name, rest[1])
		if err != nil {
			return err
		}
		return annotate.AddComment(db, lineage, strings.Join(rest[2:], " "))
	case "list":
		if len(rest) < 2 {
			return fmt.Errorf("usage: recall comment list SESSION_ID")
		}
		lineage, err := annotate.LineageForIdentifier(db, p.Name, rest[1])
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
			return fmt.Errorf("usage: recall comment rm COMMENT_ID")
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

func cmdTag(global *flag.FlagSet, profileFlag *string, noRefresh *bool, args []string) error {
	rest, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return fmt.Errorf("usage: recall tag add|rm|list ...")
	}
	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}
	db, err := openDB(p, *noRefresh, false)
	if err != nil {
		return err
	}

	switch rest[0] {
	case "add":
		if len(rest) < 3 {
			return fmt.Errorf("usage: recall tag add SESSION_ID TAG")
		}
		lineage, err := annotate.LineageForIdentifier(db, p.Name, rest[1])
		if err != nil {
			return err
		}
		return annotate.AddTag(db, lineage, rest[2])
	case "rm":
		if len(rest) < 3 {
			return fmt.Errorf("usage: recall tag rm SESSION_ID TAG")
		}
		lineage, err := annotate.LineageForIdentifier(db, p.Name, rest[1])
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

func cmdRefresh(global *flag.FlagSet, profileFlag *string, args []string) error {
	full := global.Bool("full", false, "discard the index and rebuild it from scratch")
	if err := global.Parse(args); err != nil {
		return err
	}
	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}
	r, err := refresh.New(p, "")
	if err != nil {
		return err
	}
	sum, err := r.Refresh(refresh.Options{FullRebuild: *full})
	if err != nil {
		return err
	}
	fmt.Printf("Profile: %s\n", p.Name)
	for _, s := range sum.Sources {
		if s.Available {
			fmt.Printf("  %s: %d sessions\n", s.Source, s.Sessions)
		} else {
			fmt.Printf("  %s: unavailable (%v)\n", s.Source, s.Err)
		}
	}
	return nil
}

// cmdProfiles prints each discovered profile by name together with the
// sources it covers (spec session-search, "The profile listing is
// readable"; choose-from-known-values task 4.1/4.2). The prior form printed
// p.Sources() straight through %v, which is Go's own map syntax
// ("claude: map[claude:/Users/...]") - not something intended to be read,
// and not a reliable way to learn a profile's name from the command line.
func cmdProfiles(jsonOut bool) error {
	profiles := profile.Discover()
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(profiles)
	}
	if len(profiles) == 0 {
		fmt.Println("No session sources found on this machine.")
		return nil
	}
	for i, p := range profiles {
		if i > 0 {
			fmt.Println()
		}
		fmt.Println(p.Name)
		sources := p.Sources()
		names := make([]string, 0, len(sources))
		for name := range sources {
			names = append(names, name)
		}
		sort.Strings(names) // deterministic - map iteration order is not
		for _, name := range names {
			fmt.Printf("  %s: %s\n", name, sources[name])
		}
	}
	return nil
}

func outputItems(p profile.Profile, items []search.Item, jsonOut bool, emptyMessage string) error {
	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		w := &jsonProfileWriter{enc: enc, profile: p.Name}
		return w.writeItems(items)
	}
	fmt.Printf("Profile: %s\n", p.Name)
	cli.WriteItemsHuman(os.Stdout, items, emptyMessage, cli.DetermineOptions(os.Stdout))
	return nil
}

// jsonProfileWriter wraps the item list with the active profile, so
// machine-readable output also identifies which profile it was produced
// under (spec session-index, "Active profile is visible").
type jsonProfileWriter struct {
	enc     *json.Encoder
	profile string
}

func (w *jsonProfileWriter) writeItems(items []search.Item) error {
	type out struct {
		Profile string `json:"profile"`
		Items   []any  `json:"items"`
	}
	return w.enc.Encode(out{Profile: w.profile, Items: cli.ToJSONItems(items)})
}

// jsonEnvelope wraps a JSON-array writer with the active profile, so
// review's machine-readable output also identifies which profile it ran
// under (spec session-index, "Active profile is visible"), without
// review's own array-writer needing to know about the envelope.
func jsonEnvelope(profileName string, writeArray func() error) error {
	fmt.Printf(`{"profile": %q, "entries": `, profileName)
	if err := writeArray(); err != nil {
		return err
	}
	fmt.Println("}")
	return nil
}

// ---------------------------------------------------------------------
// recall browse (change add-interactive-browse)
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
func cmdBrowse(global *flag.FlagSet, profileFlag *string, noRefresh *bool, args []string) error {
	agent, repoF, tag, _ := parseFilterFlags(global)
	positionals, err := parseInterleaved(global, args)
	if err != nil {
		return err
	}
	query := strings.Join(positionals, " ")

	p, err := resolveProfile(*profileFlag)
	if err != nil {
		return err
	}

	if !cli.IsTerminal(os.Stdout) {
		fmt.Fprintln(os.Stderr, "browsing requires a terminal; showing a non-interactive listing instead.")
		db, err := openDB(p, *noRefresh, false)
		if err != nil {
			return err
		}
		f := search.Filter{Agent: *agent, Repo: *repoF, Tag: *tag}
		var items []search.Item
		if query != "" {
			items, err = search.Search(db, query, f)
		} else {
			items, err = search.List(db, f)
		}
		if err != nil {
			return err
		}
		return outputItems(p, items, false, search.EmptyMessage(p.Name, f, query))
	}

	db, err := openDB(p, *noRefresh, false)
	if err != nil {
		return err
	}

	selected, ok, err := cli.RunBrowser(cli.BrowserOptions{
		DB:          db,
		ProfileName: p.Name,
		Repo:        *repoF,
		Agent:       *agent,
		Tag:         *tag,
		Query:       query,
		Style:       os.Getenv("NO_COLOR") == "",
		Resolve:     resolveProfile,
		Profiles:    profile.Discover,
	})
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	// The accepted session may belong to a different profile than the one
	// this function opened with - the browser switches profiles in-process
	// (spec session-search, "Switching profile replaces the view") - so
	// resume against the item the browser returned as it was selected; the
	// item carries its profile's data. This is the same resume path the
	// non-interactive `recall resume` uses.
	//
	// cli.RunBrowser has already returned by this point, which is exactly
	// what makes the terminal-restoration ordering correct (change
	// resume-in-current-terminal, design.md decision 3): bubbletea tears
	// down the alternate screen and restores terminal modes as part of
	// p.Run() returning, so that has already happened before Resume can
	// replace this process - there is no later point at which it could
	// still be undone.
	profileEnv, profileOK, profileReason := resumeProfileEnv(selected.Source, profileNameFrom(selected.SessionID))
	out := resume.Resume(resume.Target{
		Source: selected.Source, SourceSessionID: sourceSessionIDFrom(selected.SessionID),
		CWD: selected.CWD, GitRepoRoot: selected.GitRepoRoot, DirExists: selected.DirExists,
		Resumable:               selected.Resumable,
		ProfileEnv:              profileEnv,
		ProfileUnresolved:       !profileOK,
		ProfileUnresolvedReason: profileReason,
	})
	fmt.Println(out.Message)
	return nil
}
