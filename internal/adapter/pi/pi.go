// Package pi adapts pi's on-disk session data: transcripts only, no index
// of any kind, at ~/.pi/agent/sessions/<encoded-cwd>/<ts>_<uuid>.jsonl
// (design.md context). Tier 2 (prompts) has no source index to query, so
// refresh always falls back to extracting prompts from the transcript scan
// itself for every pi session (task 5.4).
package pi

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"recall/internal/adapter"
	"recall/internal/profile"
	"recall/internal/session"
)

type Adapter struct{}

func New() *Adapter { return &Adapter{} }

func (*Adapter) Name() string { return "pi" }

func (a *Adapter) Discover(p profile.Profile) ([]adapter.Discovered, error) {
	root := p.PiRoot
	if root == "" {
		return nil, &adapter.Unavailable{Reason: "no pi root for this profile"}
	}
	sessionsDir := filepath.Join(root, "agent", "sessions")
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &adapter.Unavailable{Reason: "no pi session data on this machine"}
		}
		return nil, fmt.Errorf("pi: listing %s: %w", sessionsDir, err)
	}

	var out []adapter.Discovered
	for _, dirEntry := range entries {
		if !dirEntry.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir, dirEntry.Name())
		files, err := os.ReadDir(dir)
		if err != nil {
			continue // unreadable subdir: skip, keep going
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".jsonl") {
				continue
			}
			// Filename convention: <RFC3339-ish timestamp>_<uuid>.jsonl.
			// The id used for identity is the file's own basename minus
			// extension - unique and stable regardless of what it encodes.
			id := strings.TrimSuffix(f.Name(), ".jsonl")
			fi, err := f.Info()
			if err != nil {
				continue
			}
			s := session.Session{
				ID:              "pi:" + p.Name + ":" + id,
				Source:          "pi",
				Profile:         p.Name,
				SourceSessionID: id,
				EndState:        session.EndStateUnknown,
				Resumable:       true,
			}
			out = append(out, adapter.Discovered{
				Session:        s,
				TranscriptPath: filepath.Join(dir, f.Name()),
				FileSize:       fi.Size(),
			})
		}
	}
	return out, nil
}
