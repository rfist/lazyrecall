// Package kilo adapts Kilo's on-disk session data. Kilo ships OpenCode's
// schema verbatim - the same session/message/part tables, the same JSON
// `data` blob columns, the same `--session <id>` resume flag - under its own
// name, at ~/.local/share/kilo/kilo.db. Verified by comparing the two
// databases table-for-table on a machine carrying both, and against
// `kilo --help`, which documents "-s, --session  session id to continue".
//
// So this package is a name and a file, not an implementation: the queries
// live in internal/adapter/opencode, which takes both as parameters. A
// second copy of them would be a second thing to keep in step with a schema
// neither project controls.
package kilo

import (
	"github.com/rfist/lazyrecall/internal/adapter/opencode"
)

// New builds the Kilo adapter over OpenCode's implementation.
func New(sqlite3Path string) *opencode.Adapter {
	return opencode.NewFor("kilo", "kilo.db", sqlite3Path)
}
