// Package trust implements the pt allow database.
//
// A .plasticturtle is inert until its exact bytes have been approved for its
// exact canonical path. This package is the whole enforcement mechanism, so it
// stays small enough to audit in one sitting.
package trust

import "time"

// FileName is the trust database, stored in the state root.
const FileName = "trust.json"

// Record is one approved config.
type Record struct {
	// Hash is config.HashBytes of the approved file contents.
	Hash string `json:"hash"`
	// AllowedAt is when the user approved it.
	AllowedAt time.Time `json:"allowedAt"`
}

// Store is the trust database. It is keyed by canonical absolute project path;
// moving a project therefore requires re-allowing, which is intentional.
type Store interface {
	// Get returns the record for projectPath. A missing entry is (zero, false,
	// nil) — absence is not an error.
	Get(projectPath string) (Record, bool, error)

	// Check reports whether projectPath is allowed at exactly hash. A missing
	// entry and a stale entry are both simply false; callers phrase the error.
	Check(projectPath, hash string) (bool, error)

	// Allow records approval of hash for projectPath, replacing any prior
	// entry. Writes are atomic (temp file + rename) under an exclusive lock on
	// the database, so concurrent pt allow runs cannot truncate it.
	Allow(projectPath, hash string, now time.Time) error
}

// Open returns the Store backed by the trust database at path, creating the
// parent directory if needed. A nonexistent database is an empty one.
func Open(path string) (Store, error) {
	panic("TODO(wave1): trust.Open")
}
