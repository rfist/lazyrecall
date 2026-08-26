// Package claude adapts Claude Code's on-disk session data: a flat
// append-only history index at $CLAUDE_CONFIG_DIR/history.jsonl plus one
// transcript per session under projects/<encoded-cwd>/<uuid>.jsonl. Each
// discovered Claude config root is its own profile (design.md; task 5.2).
//
// This adapter only enumerates. It locates each session's transcript file
// by its uuid basename - never by decoding the encoded directory name the
// files happen to live under (spec session-index, "Authoritative working
// directory"). Transcript content is read by internal/transcript, driven by
// internal/refresh.
package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lazyrecall/internal/adapter"
	"lazyrecall/internal/profile"
	"lazyrecall/internal/session"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return "claude" }

func (a *Adapter) Discover(p profile.Profile) ([]adapter.Discovered, error) {
	root := p.ClaudeRoot
	if root == "" {
		return nil, &adapter.Unavailable{Reason: "no Claude config root for this profile"}
	}

	transcripts, err := findTranscripts(root)
	if err != nil {
		return nil, fmt.Errorf("claude: listing transcripts under %s: %w", root, err)
	}

	// history.jsonl is a cheap, best-effort enumeration hint (last activity
	// time). It is never treated as authoritative for cwd/branch - that
	// always comes from the transcript itself, read later by refresh.
	lastActivity, _ := scanHistoryLastActivity(filepath.Join(root, "history.jsonl"))

	out := make([]adapter.Discovered, 0, len(transcripts))
	for id, info := range transcripts {
		s := session.Session{
			ID:              "claude:" + p.Name + ":" + id,
			Source:          "claude",
			Profile:         p.Name,
			SourceSessionID: id,
			EndState:        session.EndStateUnknown,
			Resumable:       true,
		}
		if la, ok := lastActivity[id]; ok {
			s.LastActivityAt = la
		}
		out = append(out, adapter.Discovered{
			Session:        s,
			TranscriptPath: info.path,
			FileSize:       info.size,
		})
	}
	return out, nil
}

type transcriptInfo struct {
	path string
	size int64
}

// findTranscripts walks root/projects/*/*.jsonl and indexes each transcript
// by its uuid basename - the session id. Subdirectory names (the encoded
// cwd) are used only to find files, never decoded into a path.
func findTranscripts(root string) (map[string]transcriptInfo, error) {
	projectsDir := filepath.Join(root, "projects")
	entries, err := os.ReadDir(projectsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]transcriptInfo{}, nil
		}
		return nil, err
	}

	out := map[string]transcriptInfo{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(projectsDir, e.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue // unreadable subdir: skip it, keep going
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			fi, err := f.Info()
			if err != nil {
				continue
			}
			out[id] = transcriptInfo{path: filepath.Join(dir, f.Name()), size: fi.Size()}
		}
	}
	return out, nil
}

// scanHistoryLastActivity reads history.jsonl fully (it is small and
// append-only) and returns, per session id, the latest timestamp seen.
func scanHistoryLastActivity(path string) (map[string]*time.Time, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	out := map[string]*time.Time{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var rec struct {
			SessionID string `json:"sessionId"`
			Timestamp int64  `json:"timestamp"`
		}
		if err := json.Unmarshal(sc.Bytes(), &rec); err != nil {
			continue
		}
		if rec.SessionID == "" {
			continue
		}
		t := time.UnixMilli(rec.Timestamp)
		if prev, ok := out[rec.SessionID]; !ok || t.After(*prev) {
			out[rec.SessionID] = &t
		}
	}
	return out, sc.Err()
}

// HistoryPrompt is one tier-2 prompt entry read from history.jsonl.
type HistoryPrompt struct {
	SessionID string
	Text      string
	At        *time.Time
}

// PromptsSince reads history.jsonl from byte offset fromOffset to EOF -
// the same incremental, cursor-driven pattern transcript.Scan uses - and
// returns every prompt recorded in that range plus the new offset (task
// 6.4: tier 2 ingestion from the source's own index). history.jsonl's
// "display" field is literally what the user typed or the slash command
// they ran; pastedContents (large pasted blocks) are deliberately not
// unpacked here - decision 4 scopes the index to what the user typed, and
// "display" already stands in for a paste with a placeholder the user did
// in fact see and send.
func PromptsSince(root string, fromOffset int64) ([]HistoryPrompt, int64, error) {
	path := filepath.Join(root, "history.jsonl")
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fromOffset, nil
		}
		return nil, fromOffset, err
	}
	defer f.Close()

	if fromOffset > 0 {
		if _, err := f.Seek(fromOffset, 0); err != nil {
			return nil, fromOffset, err
		}
	}

	var out []HistoryPrompt
	offset := fromOffset
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		offset += int64(len(line)) + 1
		var rec struct {
			Display   string `json:"display"`
			Timestamp int64  `json:"timestamp"`
			SessionID string `json:"sessionId"`
		}
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec.Display == "" || rec.SessionID == "" {
			continue
		}
		t := time.UnixMilli(rec.Timestamp)
		out = append(out, HistoryPrompt{SessionID: rec.SessionID, Text: rec.Display, At: &t})
	}
	if err := sc.Err(); err != nil {
		return out, offset, err
	}
	return out, offset, nil
}
