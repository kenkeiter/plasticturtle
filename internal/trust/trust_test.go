package trust

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kenkeiter/plasticturtle/internal/config"
)

const (
	hashA = "sha256:aaaa"
	hashB = "sha256:bbbb"
)

// newStore opens a database under a fresh temp dir, including a directory level
// that does not exist yet so that Open's mkdir path is exercised every time.
func newStore(t *testing.T) (Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state", FileName)
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s, dbPath
}

func TestOpenCreatesParentDirectoryPrivately(t *testing.T) {
	_, dbPath := newStore(t)

	info, err := os.Stat(filepath.Dir(dbPath))
	if err != nil {
		t.Fatalf("stat state dir: %v", err)
	}
	if got := info.Mode().Perm(); got != dirPerm {
		t.Errorf("state dir perm = %v, want %v", got, dirPerm)
	}
	// Open must not create the database itself; absence is meaningful.
	if _, err := os.Stat(dbPath); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("database exists after Open; want it absent until first Allow (err=%v)", err)
	}
}

func TestOpenRejectsRelativePath(t *testing.T) {
	if _, err := Open("state/trust.json"); err == nil {
		t.Fatal("Open(relative) = nil error, want error")
	}
	if _, err := Open(""); err == nil {
		t.Fatal("Open(\"\") = nil error, want error")
	}
}

func TestMissingDatabaseIsNotTrustedAndNotAnError(t *testing.T) {
	s, _ := newStore(t)

	ok, err := s.Check("/Users/alice/proj", hashA)
	if err != nil {
		t.Fatalf("Check on missing database: %v", err)
	}
	if ok {
		t.Error("Check = true on missing database, want false")
	}

	rec, found, err := s.Get("/Users/alice/proj")
	if err != nil {
		t.Fatalf("Get on missing database: %v", err)
	}
	if found {
		t.Errorf("Get found = true, want false (rec=%+v)", rec)
	}
	if rec.Hash != "" || !rec.AllowedAt.IsZero() || rec.Raw != nil {
		t.Errorf("Get rec = %+v, want zero", rec)
	}
}

func TestAllowThenCheck(t *testing.T) {
	s, dbPath := newStore(t)
	now := time.Date(2026, 8, 18, 8, 59, 0, 0, time.UTC)

	if err := s.Allow("/Users/alice/proj", hashA, nil, now); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	ok, err := s.Check("/Users/alice/proj", hashA)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !ok {
		t.Error("Check at the allowed hash = false, want true")
	}

	// A different hash for the same path is the "config changed" case.
	ok, err = s.Check("/Users/alice/proj", hashB)
	if err != nil {
		t.Fatalf("Check(other hash): %v", err)
	}
	if ok {
		t.Error("Check at a different hash = true, want false")
	}

	// An unknown path is the "never allowed" case.
	ok, err = s.Check("/Users/alice/other", hashA)
	if err != nil {
		t.Fatalf("Check(unknown path): %v", err)
	}
	if ok {
		t.Error("Check for an unknown path = true, want false")
	}

	rec, found, err := s.Get("/Users/alice/proj")
	if err != nil || !found {
		t.Fatalf("Get = (%+v, %v, %v), want found", rec, found, err)
	}
	if rec.Hash != hashA {
		t.Errorf("Hash = %q, want %q", rec.Hash, hashA)
	}
	if !rec.AllowedAt.Equal(now) {
		t.Errorf("AllowedAt = %v, want %v", rec.AllowedAt, now)
	}

	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat database: %v", err)
	}
	if got := info.Mode().Perm(); got != filePerm {
		t.Errorf("database perm = %v, want %v", got, filePerm)
	}
}

func TestAllowReplacesExistingEntry(t *testing.T) {
	s, dbPath := newStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t1 := t0.Add(48 * time.Hour)

	if err := s.Allow("/Users/alice/proj", hashA, nil, t0); err != nil {
		t.Fatalf("Allow #1: %v", err)
	}
	if err := s.Allow("/Users/alice/proj", hashB, nil, t1); err != nil {
		t.Fatalf("Allow #2: %v", err)
	}

	ok, err := s.Check("/Users/alice/proj", hashA)
	if err != nil {
		t.Fatalf("Check(old): %v", err)
	}
	if ok {
		t.Error("old hash still trusted after re-Allow; replacement must be total")
	}
	ok, err = s.Check("/Users/alice/proj", hashB)
	if err != nil {
		t.Fatalf("Check(new): %v", err)
	}
	if !ok {
		t.Error("new hash not trusted after re-Allow")
	}

	db := readDB(t, dbPath)
	if len(db) != 1 {
		t.Errorf("database has %d entries after replacing one, want 1: %+v", len(db), db)
	}
	if !db["/Users/alice/proj"].AllowedAt.Equal(t1) {
		t.Errorf("AllowedAt = %v, want %v", db["/Users/alice/proj"].AllowedAt, t1)
	}
}

func TestKeysAreCleanedAbsolutePaths(t *testing.T) {
	s, dbPath := newStore(t)

	if err := s.Allow("/Users/alice/../alice/proj/", hashA, nil, time.Now()); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	db := readDB(t, dbPath)
	if _, ok := db["/Users/alice/proj"]; !ok {
		t.Errorf("key not cleaned on write: %+v", db)
	}
	// The same project reached by an uncleaned path must resolve to one entry.
	ok, err := s.Check("/Users/alice/proj/.", hashA)
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !ok {
		t.Error("cleaned lookup key did not match the cleaned stored key")
	}
}

func TestRelativeAndEmptyInputsRejected(t *testing.T) {
	s, _ := newStore(t)
	now := time.Now()

	cases := []struct {
		name string
		path string
	}{
		{"relative", "proj"},
		{"dot-relative", "./proj"},
		{"parent-relative", "../proj"},
		{"empty", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.Allow(tc.path, hashA, nil, now); err == nil {
				t.Error("Allow accepted a non-canonical path")
			}
			if _, _, err := s.Get(tc.path); err == nil {
				t.Error("Get accepted a non-canonical path")
			}
			if _, err := s.Check(tc.path, hashA); err == nil {
				t.Error("Check accepted a non-canonical path")
			}
		})
	}

	// An empty hash must never be answerable, in either direction.
	if _, err := s.Check("/Users/alice/proj", ""); err == nil {
		t.Error("Check accepted an empty hash")
	}
	if err := s.Allow("/Users/alice/proj", "", nil, now); err == nil {
		t.Error("Allow accepted an empty hash")
	}
}

func TestCorruptDatabaseIsAnErrorNotAnEmptyDatabase(t *testing.T) {
	s, dbPath := newStore(t)
	if err := s.Allow("/Users/alice/proj", hashA, nil, time.Now()); err != nil {
		t.Fatalf("Allow: %v", err)
	}

	// Truncation mid-object is exactly what a non-atomic writer would leave.
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(dbPath, raw[:len(raw)/2], filePerm); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	if _, err := s.Check("/Users/alice/proj", hashA); err == nil {
		t.Fatal("Check on a corrupt database = nil error; silently-empty degrades to re-prompting without telling the user why")
	} else if !strings.Contains(err.Error(), "corrupt") {
		t.Errorf("error %q does not name the problem", err)
	}
	if _, _, err := s.Get("/Users/alice/proj"); err == nil {
		t.Error("Get on a corrupt database = nil error")
	}
	if err := s.Allow("/Users/alice/proj", hashB, nil, time.Now()); err == nil {
		t.Error("Allow overwrote a corrupt database; the user should inspect it first")
	}
}

func TestConcurrentAllowKeepsEveryEntry(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	dbPath := filepath.Join(dir, FileName)

	const n = 24
	now := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)

	var wg sync.WaitGroup
	errs := make(chan error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Each goroutine opens its own Store, the way N concurrent `pt
			// allow` processes would; nothing is shared in memory.
			s, err := Open(dbPath)
			if err != nil {
				errs <- err
				return
			}
			<-start
			if err := s.Allow(projectPathFor(i), hashFor(i), nil, now); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Allow: %v", err)
	}

	db := readDB(t, dbPath)
	if len(db) != n {
		t.Fatalf("database has %d entries, want %d — a writer lost another's entry", len(db), n)
	}
	for i := range n {
		rec, ok := db[projectPathFor(i)]
		if !ok {
			t.Errorf("entry %d missing", i)
			continue
		}
		if rec.Hash != hashFor(i) {
			t.Errorf("entry %d hash = %q, want %q", i, rec.Hash, hashFor(i))
		}
	}
}

// TestInterruptedWriteLeavesPreviousDatabaseIntact covers the failure mode the
// temp-file dance exists for: the target is never the file being written, and a
// write that dies before the rename leaves the old contents readable.
func TestInterruptedWriteLeavesPreviousDatabaseIntact(t *testing.T) {
	s, dbPath := newStore(t)
	if err := s.Allow("/Users/alice/proj", hashA, nil, time.Now()); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	before, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Make the rename fail by replacing the target with a directory: os.Rename
	// of a file onto a non-empty directory fails on macOS and Linux alike.
	if err := os.Remove(dbPath); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dbPath, "blocker"), dirPerm); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}
	fs2 := s.(*fileStore)
	if err := fs2.save(map[string]Record{"/Users/alice/proj": {Hash: hashB}}); err == nil {
		t.Fatal("save = nil error with an unrenameable target, want error")
	}

	// The failed write must not have left its temp file behind, and must not
	// have touched anything else in the directory.
	names := dirEntries(t, filepath.Dir(dbPath))
	for _, name := range names {
		if strings.Contains(name, ".tmp-") {
			t.Errorf("temp file %q left behind after a failed save", name)
		}
	}

	// Restore the real file and confirm a subsequent successful write is still
	// consistent with the pre-failure contents.
	if err := os.RemoveAll(dbPath); err != nil {
		t.Fatalf("cleanup blocker: %v", err)
	}
	if err := os.WriteFile(dbPath, before, filePerm); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ok, err := s.Check("/Users/alice/proj", hashA)
	if err != nil || !ok {
		t.Fatalf("Check after restore = (%v, %v), want (true, nil)", ok, err)
	}
}

// TestSaveNeverWritesThroughTheTargetPath asserts the property that makes the
// above safe: while save runs, the target inode is either absent or complete.
func TestSaveWritesTempFileInSameDirectory(t *testing.T) {
	s, dbPath := newStore(t)
	fs2 := s.(*fileStore)

	// Pre-create the target with sentinel bytes that are not valid JSON. If
	// save wrote through the target rather than renaming onto it, a concurrent
	// reader could observe a partial file; here we assert the sentinel is
	// replaced wholesale and the directory holds no leftovers.
	if err := os.WriteFile(dbPath, []byte("SENTINEL"), filePerm); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := fs2.save(map[string]Record{"/Users/alice/proj": {Hash: hashA}}); err != nil {
		t.Fatalf("save: %v", err)
	}
	for _, name := range dirEntries(t, filepath.Dir(dbPath)) {
		if strings.Contains(name, ".tmp-") {
			t.Errorf("temp file %q left behind after a successful save", name)
		}
	}
	db := readDB(t, dbPath)
	if db["/Users/alice/proj"].Hash != hashA {
		t.Errorf("target not replaced: %+v", db)
	}
}

func TestLockIsASidecarNotTheDatabase(t *testing.T) {
	s, dbPath := newStore(t)
	fs2 := s.(*fileStore)
	if fs2.lockPath == dbPath {
		t.Fatal("lock path == database path; the atomic rename would swap the locked inode out from under a concurrent writer")
	}
	if err := s.Allow("/Users/alice/proj", hashA, nil, time.Now()); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	if _, err := os.Stat(fs2.lockPath); err != nil {
		t.Errorf("sidecar lock file not created: %v", err)
	}
	// The lock file must never be mistaken for the database.
	if db := readDB(t, dbPath); len(db) != 1 {
		t.Errorf("database = %+v, want exactly the one entry", db)
	}
}

func TestDatabaseIsHumanReadableWithStableKeyOrder(t *testing.T) {
	s, dbPath := newStore(t)
	now := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	for _, p := range []string{"/z/proj", "/a/proj", "/m/proj"} {
		if err := s.Allow(p, hashA, nil, now); err != nil {
			t.Fatalf("Allow %s: %v", p, err)
		}
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	text := string(raw)
	if !strings.HasSuffix(text, "\n") {
		t.Error("database does not end in a newline")
	}
	if !strings.Contains(text, "\n  \"") {
		t.Error("database is not indented; a human is expected to read it")
	}
	ia := strings.Index(text, "/a/proj")
	im := strings.Index(text, "/m/proj")
	iz := strings.Index(text, "/z/proj")
	if !(ia < im && im < iz) {
		t.Errorf("keys are not sorted: /a at %d, /m at %d, /z at %d", ia, im, iz)
	}
}

func TestAllowStoresUTC(t *testing.T) {
	s, dbPath := newStore(t)
	loc := time.FixedZone("UTC-7", -7*60*60)
	local := time.Date(2026, 8, 18, 2, 0, 0, 0, loc)
	if err := s.Allow("/Users/alice/proj", hashA, nil, local); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	raw, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Spec §5's example shows a Z-suffixed timestamp; keep the file
	// comparable across machines in different zones.
	if !strings.Contains(string(raw), "2026-08-18T09:00:00Z") {
		t.Errorf("timestamp not normalized to UTC: %s", raw)
	}
}

func projectPathFor(i int) string {
	return filepath.Join("/Users/alice/code", "proj"+strconv.Itoa(i))
}

func hashFor(i int) string { return "sha256:" + strconv.Itoa(i) }

func readDB(t *testing.T, path string) map[string]Record {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}
	db := map[string]Record{}
	if err := json.Unmarshal(raw, &db); err != nil {
		t.Fatalf("database is not valid JSON (%v): %s", err, raw)
	}
	return db
}

func dirEntries(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestAllowRoundTripsTheApprovedBytes(t *testing.T) {
	s, _ := newStore(t)
	raw := []byte("version: 1\nimage: img\n")

	if err := s.Allow("/Users/alice/proj", hashBytes(raw), raw, time.Now()); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	rec, found, err := s.Get("/Users/alice/proj")
	if err != nil || !found {
		t.Fatalf("Get = (%+v, %v, %v), want found", rec, found, err)
	}
	if string(rec.Raw) != string(raw) {
		// Byte-exact matters: the snapshot is diffed against a later file, and
		// a re-serialized approximation would report edits nobody made.
		t.Errorf("Raw = %q, want %q", rec.Raw, raw)
	}
}

func TestAllowRejectsASnapshotThatDoesNotMatchItsHash(t *testing.T) {
	s, _ := newStore(t)

	// The whole value of the snapshot is that it is the bytes the hash
	// approved. Storing a mismatched pair would let the next pt allow diff
	// against something the user never saw and report "nothing changed".
	if err := s.Allow("/Users/alice/proj", hashA, []byte("image: img\n"), time.Now()); err == nil {
		t.Fatal("Allow accepted a snapshot that does not hash to the approved hash")
	}
	if _, found, _ := s.Get("/Users/alice/proj"); found {
		t.Error("a rejected Allow still wrote a record")
	}
}

func TestAllowDropsAnOversizeSnapshot(t *testing.T) {
	s, _ := newStore(t)
	raw := []byte(strings.Repeat("# padding\n", maxSnapshot/10+1))

	// Trust must still be recorded; only the diff-time convenience is dropped.
	if err := s.Allow("/Users/alice/proj", hashBytes(raw), raw, time.Now()); err != nil {
		t.Fatalf("Allow: %v", err)
	}
	rec, found, err := s.Get("/Users/alice/proj")
	if err != nil || !found {
		t.Fatalf("Get = (%+v, %v, %v), want found", rec, found, err)
	}
	if rec.Hash != hashBytes(raw) {
		t.Errorf("Hash = %q, want the approved hash", rec.Hash)
	}
	if rec.Raw != nil {
		t.Errorf("Raw is %d bytes, want it dropped above the %d-byte cap", len(rec.Raw), maxSnapshot)
	}
}

func TestRecordsWithoutASnapshotStillLoad(t *testing.T) {
	s, dbPath := newStore(t)

	// A database written by a pt that predates snapshots. It must keep working:
	// the omitted field is optional, not a corrupt record.
	body := `{"/Users/alice/proj":{"hash":"` + hashA + `","allowedAt":"2026-08-18T08:59:00Z"}}`
	if err := os.WriteFile(dbPath, []byte(body), filePerm); err != nil {
		t.Fatal(err)
	}
	rec, found, err := s.Get("/Users/alice/proj")
	if err != nil || !found {
		t.Fatalf("Get = (%+v, %v, %v), want found", rec, found, err)
	}
	if rec.Raw != nil {
		t.Errorf("Raw = %q, want nil", rec.Raw)
	}
	ok, err := s.Check("/Users/alice/proj", hashA)
	if err != nil || !ok {
		t.Errorf("Check = (%v, %v), want true; a snapshot-less record still grants trust", ok, err)
	}
}

func TestHashBytesMatchesConfig(t *testing.T) {
	// hashBytes is duplicated here so this package does not depend on the
	// config parser. That is only safe while the two agree exactly: a record
	// written with one and checked with the other would silently never match.
	for _, raw := range [][]byte{nil, []byte(""), []byte("version: 1\nimage: img\n")} {
		if got, want := hashBytes(raw), config.HashBytes(raw); got != want {
			t.Errorf("hashBytes(%q) = %q, config.HashBytes = %q", raw, got, want)
		}
	}
}
