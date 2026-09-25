package annotate

import (
	"strings"

	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// SetName assigns lazyrecall's own name to a lineage - the name the user
// sets from inside lazyrecall itself (the browser's rename action, or
// `lazyrecall name`), as distinct from sessions.name, the name a source
// tool records (Claude Code's rename) and which lives on the disposable
// index instead (change show-session-names' own non-goal: "a Recall-
// assigned name would live on the durable lineage, not on the disposable
// index" - this is that follow-up). It lives on the durable lineages
// table, exactly like group_name and archived_at, so it survives an index
// rebuild and moves with a continuation the way a lineage's other
// annotations do.
//
// An empty or whitespace-only name clears it (NULL, "no custom name") -
// mirroring SetGroup's "" choice - so submitting a blank rename prompt is
// how a user removes a name they set earlier, not how they set one that is
// blank.
func SetName(db *sqlitex.Runner, lineageID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return clearCustomName(db, lineageID)
	}
	return setCustomName(db, lineageID, name)
}

func setCustomName(db *sqlitex.Runner, lineageID, name string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID, "name": name})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET custom_name = " + pf.Ref("name") + " WHERE id = " + pf.Ref("lineage_id") + ";")
}

func clearCustomName(db *sqlitex.Runner, lineageID string) error {
	pf, err := db.WriteParams(map[string]any{"lineage_id": lineageID})
	if err != nil {
		return err
	}
	defer pf.Close()
	return db.Exec("UPDATE lineages SET custom_name = NULL WHERE id = " + pf.Ref("lineage_id") + ";")
}
