package annotate

import (
	"fmt"
	"strconv"

	"lazyrecall/internal/session"
	"lazyrecall/internal/sqlitex"
)

// LineageForIdentifier resolves either form of a session identifier (spec
// session-annotations, "Annotation commands accept the short handle" /
// design.md decision 4) to the lineage id annotations attach to: an
// all-digit argument is resolved directly against the lineages table by
// (profile, handle); anything else is treated as the fully-qualified
// session identifier and resolved via LineageForSession, exactly as
// before.
func LineageForIdentifier(db *sqlitex.Runner, profileName, arg string) (string, error) {
	if session.IsHandle(arg) {
		return lineageForHandle(db, profileName, arg)
	}
	return LineageForSession(db, arg)
}

func lineageForHandle(db *sqlitex.Runner, profileName, arg string) (string, error) {
	handle, err := strconv.ParseInt(arg, 10, 64)
	if err != nil {
		return "", fmt.Errorf("annotate: handle %q does not resolve to any session in profile %q", arg, profileName)
	}
	pf, err := db.WriteParams(map[string]any{"profile": profileName, "handle": handle})
	if err != nil {
		return "", err
	}
	defer pf.Close()

	var rows []struct {
		ID string `json:"id"`
	}
	q := "SELECT id FROM lineages WHERE profile = " + pf.Ref("profile") + " AND handle = " + pf.Ref("handle") + ";"
	if err := db.Query(q, &rows); err != nil {
		return "", fmt.Errorf("annotate: resolving handle %s: %w", arg, err)
	}
	if len(rows) == 0 {
		return "", fmt.Errorf("annotate: handle %s does not resolve to any session in profile %q", arg, profileName)
	}
	return rows[0].ID, nil
}
