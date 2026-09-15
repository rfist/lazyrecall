package annotate

import (
	"fmt"
	"strconv"

	"github.com/rfist/lazyrecall/internal/session"
	"github.com/rfist/lazyrecall/internal/sqlitex"
)

// LineageForIdentifier resolves either form of a session identifier (spec
// session-annotations, "Annotation commands accept the short handle" /
// design.md decision 4) to the lineage id annotations attach to: an
// all-digit argument is resolved directly against the lineages table by
// handle; anything else is treated as the fully-qualified session
// identifier and resolved via LineageForSession, exactly as before. Handle
// lookup is global, not scoped to a profile (change
// group-sessions-in-one-index: schema v8 makes a handle unique across every
// install sharing the one index, not just within one profile's database).
func LineageForIdentifier(db *sqlitex.Runner, arg string) (string, error) {
	if session.IsHandle(arg) {
		return lineageForHandle(db, arg)
	}
	return LineageForSession(db, arg)
}

func lineageForHandle(db *sqlitex.Runner, arg string) (string, error) {
	handle, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return "", fmt.Errorf("annotate: handle %q does not resolve to any session", arg)
	}
	pf, err := db.WriteParams(map[string]any{"handle": handle})
	if err != nil {
		return "", err
	}
	defer pf.Close()

	var rows []struct {
		ID string `json:"id"`
	}
	q := "SELECT id FROM lineages WHERE handle = " + pf.Ref("handle") + ";"
	if err := db.Query(q, &rows); err != nil {
		return "", fmt.Errorf("annotate: resolving handle %s: %w", arg, err)
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("annotate: handle %s does not resolve to any session", arg)
	}
	return rows[0].ID, nil
}
