package session

import (
	"crypto/sha256"
	"encoding/hex"
)

// LineageID deterministically derives a lineage identifier for a session
// given its source, profile, and the root of its continuation chain.
//
// Determinism matters: a full index rebuild must re-derive the same lineage
// id for the same underlying data so that annotations (which key off
// lineage id) survive re-indexing (spec session-annotations, "Session is
// re-indexed"). The id is therefore a hash of stable inputs rather than a
// freshly minted random value.
//
// rootSourceSessionID is the earliest ancestor in this session's
// continuation/fork chain, as recorded by the source. When a source records
// no continuation relationship for a session, callers pass that session's
// own SourceSessionID, giving it a lineage of its own (design.md decision 8,
// "Where a source records no such relationship, each session is its own
// lineage").
func LineageID(source, profile, rootSourceSessionID string) string {
	h := sha256.New()
	h.Write([]byte(source))
	h.Write([]byte{0})
	h.Write([]byte(profile))
	h.Write([]byte{0})
	h.Write([]byte(rootSourceSessionID))
	return "lin_" + hex.EncodeToString(h.Sum(nil))[:32]
}
