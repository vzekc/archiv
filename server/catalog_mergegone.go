package archiv

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Retiring files that were taken out of the archive because another file
// holds the same document in better form. The item of each removed file is
// merged into the item of the file that stays, and the removed file's path
// redirects to that file, so old links and item numbers keep working.
//
//	archiv merge-gone -user 1 -map removed.tsv
//
// Each line of the map is "removed path<TAB>kept path", both relative to the
// archive root; empty lines and lines starting with # are skipped. The removed
// file must be gone from disk, and its item must hold only that file. All
// lines are applied in one transaction: one bad line changes nothing.
func mergeGoneCmd(args []string) {
	fs := flag.NewFlagSet("merge-gone", flag.ExitOnError)
	user := fs.String("user", "", "the moderator in whose name it is done")
	mapFile := fs.String("map", "", "file with lines \"removed path<TAB>kept path\"")
	reason := fs.String("reason", "Dublette entfernt", "reason recorded in the history")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		log.Fatal(err)
	}
	if *user == "" || *mapFile == "" {
		log.Fatal("merge-gone: -user and -map are needed")
	}
	pairs, err := readMergeMap(*mapFile)
	if err != nil {
		log.Fatal(err)
	}
	c, err := openCatalog(cfg.CatalogDB)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	if err := mergeGone(c, cfg.ArchiveRoot, *user, *reason, pairs); err != nil {
		log.Fatal(err)
	}
	log.Printf("merge-gone: %d items merged", len(pairs))
}

func readMergeMap(name string) ([][2]string, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var pairs [][2]string
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		removed, kept, ok := strings.Cut(line, "\t")
		if !ok || removed == "" || kept == "" {
			return nil, fmt.Errorf("%s:%d: want \"removed path<TAB>kept path\"", name, n)
		}
		pairs = append(pairs, [2]string{removed, kept})
	}
	return pairs, sc.Err()
}

// mergeGone merges the item of each removed path into the item of its kept
// path and leaves a redirect from the removed path to the kept file.
func mergeGone(c *Catalog, root, user, reason string, pairs [][2]string) error {
	tx, err := c.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	batch := newBatchID()
	for _, p := range pairs {
		removed, kept := p[0], p[1]
		if _, err := os.Stat(filepath.Join(root, removed)); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%s: still on disk", removed)
		}
		var from int64
		if err := tx.QueryRow("SELECT item FROM item_files WHERE path = ?", removed).Scan(&from); err != nil {
			return fmt.Errorf("%s: no item holds it", removed)
		}
		var n int
		tx.QueryRow("SELECT COUNT(*) FROM item_files WHERE item = ?", from).Scan(&n)
		if n != 1 {
			return fmt.Errorf("%s: A%d has %d files", removed, from, n)
		}
		var into int64
		var name string
		if err := tx.QueryRow("SELECT f.item, f.name FROM item_files f JOIN items i ON i.id = f.item WHERE f.path = ? AND i.state = 'live'",
			kept).Scan(&into, &name); err != nil {
			return fmt.Errorf("%s: no live item holds it", kept)
		}
		if into == from {
			return fmt.Errorf("%s and %s are both in A%d", removed, kept, from)
		}
		for _, q := range []string{
			"DELETE FROM item_files WHERE item = ?1",
			"DELETE FROM item_tags WHERE item = ?1",
			"INSERT OR IGNORE INTO item_rel (a, kind, b) SELECT ?2, kind, b FROM item_rel WHERE a = ?1 AND b != ?2",
			"INSERT OR IGNORE INTO item_rel (a, kind, b) SELECT a, kind, ?2 FROM item_rel WHERE b = ?1 AND a != ?2",
			"DELETE FROM item_rel WHERE a = ?1 OR b = ?1",
			"UPDATE OR IGNORE coll_items SET item = ?2 WHERE item = ?1",
			"DELETE FROM coll_items WHERE item = ?1",
			"UPDATE items SET state = 'merged', merged_into = ?2, rev = rev + 1, updated = ?3 WHERE id = ?1",
			// links that already led to the removed item lead on to the kept file
			"UPDATE redirects SET item = ?2, name = ?4 WHERE item = ?1",
			"INSERT OR REPLACE INTO redirects (old_path, item, name, at) VALUES (?5, ?2, ?4, ?3)",
		} {
			if _, err := tx.Exec(q, from, into, now(), name, removed); err != nil {
				return fmt.Errorf("%s: %w", removed, err)
			}
		}
		data, _ := json.Marshal(map[string]any{"into": into, "removed": removed, "kept": kept})
		if _, err := tx.Exec(`INSERT INTO revisions (target, op, data, user, at, reason, state, batch, decided_by, decided_at)
		    VALUES (?, 'merge', ?, ?, ?, ?, 'applied', ?, ?, ?)`,
			targetOf("item", from), string(data), user, now(), reason, batch, user, now()); err != nil {
			return err
		}
	}
	if err := rebuildFTS(tx); err != nil {
		return err
	}
	return tx.Commit()
}
