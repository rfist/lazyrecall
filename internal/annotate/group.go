package annotate

import (
	"fmt"
	"strings"

	"github.com/rfist/lazyrecall/internal/config"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// SetGroup applies a group choice to a lineage - the one function the CLI's
// `group` command and (Stage E) the browser's `p` popup both call, so the
// two surfaces can never disagree about what a choice does (change
// group-sessions-in-one-index):
//
//   - a configured group's name sets the manual override (lineages.
//     group_name) and clears any archive flag - filing a session under a
//     group is a decision that supersedes an earlier archive, so the
//     session reappears in its new group's listing rather than staying
//     hidden under Archive.
//   - "archive" archives the lineage (annotate.Archive's usual, idempotent
//     rule) without touching group_name, so an archived session remembers
//     which group to return to if it is ever unarchived.
//   - "" (Automatic) clears both, returning the lineage to the fully
//     automatic, path-derived group.
//
// A name that is neither "" nor "archive" nor a group in cfg.Groups is
// rejected - config.Load already refuses to let a real group be named
// "archive" or "unknown" (or left empty), so there is no ambiguity between
// those two reserved words and an actual configured group.
func SetGroup(db *sqlitex.Runner, cfg config.Config, lineageID, choice string) error {
	switch choice {
	case "":
		return clearGroup(db, lineageID)
	case "archive":
		return Archive(db, lineageID)
	default:
		if !isConfiguredGroup(cfg, choice) {
			return fmt.Errorf("annotate: unknown group %q; valid choices are: %s", choice, validChoicesList(cfg))
		}
		return setNamedGroup(db, lineageID, choice)
	}
}

func isConfiguredGroup(cfg config.Config, name string) bool {
	for _, g := range cfg.Groups {
		if g.Name == name {
			return true
		}
	}
	return false
}

func validChoicesList(cfg config.Config) string {
	choices := make([]string, 0, len(cfg.Groups)+2)
	for _, g := range cfg.Groups {
		choices = append(choices, g.Name)
	}
	choices = append(choices, "archive", "(empty for automatic)")
	return strings.Join(choices, ", ")
}

func setNamedGroup(db *sqlitex.Runner, lineageID, name string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID, "name": name})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET group_name = " + pf.Ref("name") + ", archived_at = NULL WHERE id = " + pf.Ref("lineage_id") + ";")
}

func clearGroup(db *sqlitex.Runner, lineageID string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET group_name = NULL, archived_at = NULL WHERE id = " + pf.Ref("lineage_id") + ";")
}
