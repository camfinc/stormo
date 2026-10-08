package loop

import (
	"database/sql"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/camfinc/stormo/pkg/build"
	"github.com/camfinc/stormo/pkg/engine"
	_ "modernc.org/sqlite" // pure Go: the sidecar is a static binary
)

// SnapshotFile is one captured runtime file.
type SnapshotFile struct {
	Path   string // relative to the engine home
	Class  engine.FileClass
	Sha256 string
	Size   int
	Body   []byte
}

// SQLiteSnapshot is a consistent copy of a live SQLite DB (works while the engine holds it open in
// WAL mode): VACUUM INTO a temp file.
func SQLiteSnapshot(dbPath string) ([]byte, error) {
	dir, err := os.MkdirTemp("", "stormo-sqlite-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, "snap.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if _, err := db.Exec(fmt.Sprintf("VACUUM INTO '%s'", strings.ReplaceAll(out, "'", "''"))); err != nil {
		return nil, err
	}
	return os.ReadFile(out)
}

// TakeSnapshot walks home and captures every file a rule claims. Unclaimed files (logs, caches)
// are skipped.
func TakeSnapshot(home string, rules []engine.SnapshotRule) ([]SnapshotFile, error) {
	files := []SnapshotFile{}
	if _, err := os.Stat(home); os.IsNotExist(err) {
		return files, nil
	}
	err := filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(home, p)
		rel = filepath.ToSlash(rel)
		if engine.IsTempSibling(rel) {
			return nil
		}
		rule := engine.Classify(rel, rules)
		if rule == nil {
			return nil
		}
		var body []byte
		if rule.SQLite {
			body, err = SQLiteSnapshot(p)
		} else {
			body, err = os.ReadFile(p)
		}
		if err != nil {
			// A file can vanish between walk and read (atomic rewrites); the next nap picks it up.
			log.Printf("[nap] skip %s: %v", rel, err)
			return nil
		}
		files = append(files, SnapshotFile{Path: rel, Class: rule.Class, Sha256: build.Sha256(body), Size: len(body), Body: body})
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, err
}
