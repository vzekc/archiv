package archiv

import (
	"database/sql"
	"os"
	"path"
	"strings"
	"sync"
	"time"
)

// Archive gives read access to the archive's index (data/collection.sqlite).
// The index is rebuilt nightly into a new file that replaces the old one, so
// the handle is reopened whenever the file changes.
type Archive struct {
	cfg   *Config
	mu    sync.Mutex
	db    *sql.DB
	mtime time.Time
}

func (a *Archive) handle() (*sql.DB, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	st, err := os.Stat(a.cfg.CollectionDB)
	if err != nil {
		return nil, err
	}
	if a.db != nil && st.ModTime().Equal(a.mtime) {
		return a.db, nil
	}
	if a.db != nil {
		a.db.Close()
	}
	db, err := sql.Open("sqlite", "file:"+a.cfg.CollectionDB+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	a.db, a.mtime = db, st.ModTime()
	return db, nil
}

// SameSize lists archive files of exactly this size: duplicate candidates.
func (a *Archive) SameSize(size int64, limit int) []string {
	db, err := a.handle()
	if err != nil {
		return nil
	}
	rows, err := db.Query("SELECT path FROM files WHERE size = ? LIMIT ?", size, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		out = append(out, p)
	}
	return out
}

// validRel accepts clean relative archive paths only.
func validRel(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	if path.Clean(p) != p {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." || strings.HasPrefix(part, ".") {
			return false
		}
	}
	return true
}
