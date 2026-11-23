package archiv

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Items move from the old tree into objekte/<id/1000>/<id>/, a place that
// follows from the id alone and never changes again. The files are renamed
// on the share, which keeps their mtime; the catalog learns the new paths,
// and the old ones redirect to the item. A mirror moves only as a whole, so
// its inner links keep working. Nothing is moved unless a moderator chose
// the item.

// storeDir is the folder of an item below the archive root.
func storeDir(id int64) string {
	return fmt.Sprintf("objekte/%d/%d", id/1000, id)
}

func (a *App) storeItem(user string, id int64) error {
	var root, state string
	a.cat.QueryRow("SELECT COALESCE(root, ''), state FROM items WHERE id = ?", id).Scan(&root, &state)
	if state != "live" {
		return fmt.Errorf("nur Objekte, die da sind, lassen sich verschieben")
	}
	rows, err := a.cat.Query("SELECT name, path FROM item_files WHERE item = ? ORDER BY ord, name", id)
	if err != nil {
		return err
	}
	type mv struct{ name, from, to string }
	var moves []mv
	dest := storeDir(id)
	for rows.Next() {
		var m mv
		rows.Scan(&m.name, &m.from)
		moves = append(moves, m)
	}
	rows.Close()
	if len(moves) == 0 {
		return fmt.Errorf("das Objekt hat keine Dateien")
	}
	var dirMove *mv // a mirror's folder moves in one rename
	if root != "" && !strings.HasPrefix(root, "objekte/") {
		if st, err := os.Stat(a.archivePath(root)); err == nil && st.IsDir() {
			dirMove = &mv{from: root, to: dest + "/" + path.Base(root)}
		}
	}
	for i := range moves {
		m := &moves[i]
		switch {
		case strings.HasPrefix(m.from, "objekte/"):
			m.to = m.from
		case dirMove != nil && strings.HasPrefix(m.from, dirMove.from+"/"):
			m.to = dirMove.to + strings.TrimPrefix(m.from, dirMove.from)
		default:
			m.to = dest + "/" + path.Base(m.from)
		}
	}
	if err := os.MkdirAll(a.archivePath(dest), 0o2775); err != nil {
		return err
	}
	// on the share first; a failure leaves the catalog as it was, and what
	// moved already is found by the next sync as moved
	if dirMove != nil {
		if _, err := os.Stat(a.archivePath(dirMove.to)); err == nil {
			return fmt.Errorf("%s gibt es schon", dirMove.to)
		}
		if err := os.Rename(a.archivePath(dirMove.from), a.archivePath(dirMove.to)); err != nil {
			return err
		}
		a.moveDerived(dirMove.from, dirMove.to)
	}
	for _, m := range moves {
		if m.from == m.to || (dirMove != nil && strings.HasPrefix(m.from, dirMove.from+"/")) {
			continue
		}
		if _, err := os.Stat(a.archivePath(m.to)); err == nil {
			return fmt.Errorf("%s gibt es schon", m.to)
		}
		if err := os.Rename(a.archivePath(m.from), a.archivePath(m.to)); err != nil {
			return err
		}
		a.moveDerived(m.from, m.to)
	}

	tx, err := a.cat.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, m := range moves {
		if m.from == m.to {
			continue
		}
		if _, err := tx.Exec("UPDATE file_paths SET path = ? WHERE path = ?", m.to, m.from); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE item_files SET path = ? WHERE item = ? AND path = ?", m.to, id, m.from); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO redirects (old_path, item, name, at) VALUES (?, ?, ?, ?)", m.from, id, m.name, now()); err != nil {
			return err
		}
	}
	if dirMove != nil {
		tx.Exec("UPDATE items SET root = ? WHERE id = ?", dirMove.to, id)
		tx.Exec("INSERT OR REPLACE INTO redirects (old_path, item, name, at) VALUES (?, ?, NULL, ?)", dirMove.from, id, now())
	}
	// the old folders no longer hold the item
	tx.Exec("DELETE FROM item_tags WHERE item = ? AND tag IN (SELECT id FROM tags WHERE facet = 'location')", id)
	data, _ := json.Marshal(map[string]any{"to": dest, "files": len(moves)})
	if _, err := tx.Exec(`INSERT INTO revisions (target, op, data, user, at, state, batch, decided_by, decided_at)
	    VALUES (?, 'move-file', ?, ?, ?, 'applied', ?, ?, ?)`, targetOf("item", id), string(data), user, now(), newBatchID(), user, now()); err != nil {
		return err
	}
	tx.Exec("UPDATE items SET rev = rev + 1, updated = ? WHERE id = ?", now(), id)
	return tx.Commit()
}

func (a *App) archivePath(rel string) string {
	return filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(rel))
}

// moveDerived takes the thumbnail and preview along, which are filed by
// path while the old site runs.
func (a *App) moveDerived(from, to string) {
	for _, d := range []string{"thumbs", "previews"} {
		src := filepath.Join(a.cfg.ArchiveRoot, d, filepath.FromSlash(from))
		dst := filepath.Join(a.cfg.ArchiveRoot, d, filepath.FromSlash(to))
		for _, suffix := range []string{".jpg", ""} { // a file's picture, or a folder of them
			if _, err := os.Stat(src + suffix); err == nil {
				os.MkdirAll(filepath.Dir(dst+suffix), 0o2775)
				os.Rename(src+suffix, dst+suffix)
			}
		}
	}
	thumbs.mu.Lock()
	thumbs.seen = map[string]thumbEntry{}
	thumbs.mu.Unlock()
}

// storedID is the item a path below objekte/ belongs to, or 0.
func storedID(p string) int64 {
	parts := strings.Split(p, "/")
	if len(parts) < 3 || parts[0] != "objekte" {
		return 0
	}
	id, _ := strconv.ParseInt(parts[2], 10, 64)
	return id
}
