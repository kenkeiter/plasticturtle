// Package trust implements the pt allow database.
//
// A .plasticturtle is inert until its exact bytes have been approved for its
// exact canonical path. This package is the whole enforcement mechanism, so it
// stays small enough to audit in one sitting.
package trust

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
)

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
	// a sidecar file, so concurrent pt allow runs cannot lose each other's
	// entries. The lock is deliberately not on the database itself; see
	// lockSuffix for why locking an inode that rename replaces is unsound.
	Allow(projectPath, hash string, now time.Time) error
}

const (
	// dirPerm keeps the state root private: the trust database is the only
	// thing standing between a hostile .plasticturtle and a booted VM, so a
	// group- or world-writable parent directory would defeat the whole model.
	dirPerm fs.FileMode = 0o700
	// filePerm matches: readable for `pt _check-trust`, writable only by owner.
	filePerm fs.FileMode = 0o600

	// lockSuffix names a sidecar lock file rather than locking trust.json
	// itself. Allow replaces trust.json by rename, which swaps the inode; a
	// flock held on the old inode guards nothing once a second writer has
	// renamed a new file into place, so both writers would believe they held
	// the database and one set of entries would be lost. The sidecar's inode is
	// stable for the life of the state directory, so it actually serializes.
	lockSuffix = ".lock"
)

// Open returns the Store backed by the trust database at path, creating the
// parent directory if needed. A nonexistent database is an empty one.
func Open(path string) (Store, error) {
	if path == "" {
		return nil, errors.New("trust: empty database path")
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("trust: database path must be absolute, got %q", path)
	}
	// Create the directory eagerly so that Allow's temp file has somewhere to
	// land, and so that a read-only Check on a fresh install still works.
	if err := os.MkdirAll(filepath.Dir(path), dirPerm); err != nil {
		return nil, fmt.Errorf("trust: create state directory: %w", err)
	}
	return &fileStore{path: path, lockPath: path + lockSuffix}, nil
}

// fileStore is a JSON object on disk. It holds no cached state: pt is a
// short-lived CLI, and re-reading a few hundred bytes is cheaper than reasoning
// about a cache that another process can invalidate at any moment.
type fileStore struct {
	path     string
	lockPath string
}

func (s *fileStore) Get(projectPath string) (Record, bool, error) {
	key, err := canonicalKey(projectPath)
	if err != nil {
		return Record{}, false, err
	}
	db, err := s.load()
	if err != nil {
		return Record{}, false, err
	}
	rec, ok := db[key]
	return rec, ok, nil
}

func (s *fileStore) Check(projectPath, hash string) (bool, error) {
	// An empty hash would otherwise match an entry written with an empty hash;
	// refuse rather than invent a trust decision for a caller that clearly has
	// no digest in hand.
	if hash == "" {
		return false, errors.New("trust: empty hash")
	}
	rec, ok, err := s.Get(projectPath)
	if err != nil || !ok {
		return false, err
	}
	// "Never allowed" and "allowed at a different hash" are deliberately
	// indistinguishable here; §5 gives the user one message for both.
	return rec.Hash == hash, nil
}

func (s *fileStore) Allow(projectPath, hash string, now time.Time) error {
	key, err := canonicalKey(projectPath)
	if err != nil {
		return err
	}
	if hash == "" {
		return errors.New("trust: empty hash")
	}

	lock := flock.New(s.lockPath)
	if err := lock.Lock(); err != nil {
		return fmt.Errorf("trust: lock %s: %w", s.lockPath, err)
	}
	defer lock.Unlock() //nolint:errcheck // unlocking is best-effort; the process is exiting either way.

	// Read-modify-write must happen entirely inside the lock, otherwise two
	// concurrent `pt allow` runs both read the old database and the second
	// rename silently drops the first one's entry.
	db, err := s.load()
	if err != nil {
		return err
	}
	db[key] = Record{Hash: hash, AllowedAt: now.UTC()}
	return s.save(db)
}

// load reads the database. A missing file is an empty database, but a file that
// exists and does not parse is an error: silently treating garbage as empty
// would mean "nothing is trusted", which merely re-prompts, while silently
// *succeeding* on garbage could approve something the user never saw. Failing
// loudly points the user at a file they can inspect or delete.
func (s *fileStore) load() (map[string]Record, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("trust: read %s: %w", s.path, err)
	}
	db := map[string]Record{}
	// DisallowUnknownFields is not used: the value type is a struct, and
	// tolerating fields a future pt version adds is friendlier than refusing to
	// start after a downgrade. Unknown *keys* cannot exist — keys are paths.
	if err := json.Unmarshal(raw, &db); err != nil {
		return nil, fmt.Errorf("trust: %s is corrupt (%v); inspect it, or delete it and re-run pt allow", s.path, err)
	}
	return db, nil
}

// save writes the database atomically: a temp file in the same directory (so
// rename cannot cross a filesystem boundary and degrade to a copy), fsync'd
// before the rename so a crash cannot leave a renamed-but-empty file. A
// half-written trust database is a security failure, not a cosmetic one.
func (s *fileStore) save(db map[string]Record) error {
	// encoding/json sorts map keys, so the file diffs cleanly and a human can
	// actually read what they have approved.
	buf, err := json.MarshalIndent(db, "", "  ")
	if err != nil {
		return fmt.Errorf("trust: encode: %w", err)
	}
	buf = append(buf, '\n')

	dir := filepath.Dir(s.path)
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("trust: create temp file: %w", err)
	}
	tmpName := tmp.Name()
	// Any failure past this point must leave the previous database untouched,
	// so clean up the temp file and never rename a partial one over the target.
	defer os.Remove(tmpName)

	if err := tmp.Chmod(filePerm); err != nil {
		tmp.Close()
		return fmt.Errorf("trust: chmod temp file: %w", err)
	}
	if _, err := tmp.Write(buf); err != nil {
		tmp.Close()
		return fmt.Errorf("trust: write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("trust: sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("trust: close temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("trust: rename into place: %w", err)
	}
	// Fsync the directory so the rename itself survives a crash; without it the
	// entry can be lost even though the file contents were durable.
	if d, err := os.Open(dir); err == nil {
		d.Sync() //nolint:errcheck // best effort: some filesystems reject directory fsync.
		d.Close()
	}
	return nil
}

// canonicalKey normalizes a lookup key. Symlink resolution is deliberately NOT
// done here: config.Find already resolves the project directory to its
// canonical form, and doing it twice would let this package silently "fix" a
// path that the caller believed was canonical — masking exactly the kind of
// mismatch that trust keying exists to catch. This package's job is only to
// reject a key that plainly cannot be canonical.
func canonicalKey(projectPath string) (string, error) {
	if projectPath == "" {
		return "", errors.New("trust: empty project path")
	}
	if !filepath.IsAbs(projectPath) {
		return "", fmt.Errorf("trust: project path must be absolute, got %q", projectPath)
	}
	return filepath.Clean(projectPath), nil
}
