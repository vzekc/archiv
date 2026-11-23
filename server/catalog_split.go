package archiv

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Splitting an item that is one archive into an item per top folder and
// loose file of the archive, as intake does for an archive split there. A
// loose file named like a folder (decus_c.zip beside decus_c/) goes with
// that folder. The new items carry the tags of the old one, which is
// withdrawn with a pointer to them.
//
//	archiv split-item -item 94794 -user 1

func splitItemCmd(args []string) {
	fs := flag.NewFlagSet("split-item", flag.ExitOnError)
	item := fs.Int64("item", 0, "the item holding the archive")
	user := fs.String("user", "", "the moderator in whose name it is done")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		log.Fatal(err)
	}
	if *item == 0 || *user == "" {
		log.Fatal("split-item: -item and -user are needed")
	}
	a, err := newApp(cfg)
	if err != nil {
		log.Fatal(err)
	}
	ids, err := a.splitItem(context.Background(), *item, *user)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("split-item: A%d split into %d items, A%d to A%d", *item, len(ids), ids[0], ids[len(ids)-1])
}

// splitParts are the parts of an unpacked archive: the top folders, with
// the loose files named like them, and the other loose files.
func splitParts(from string) ([][]string, error) {
	entries, err := os.ReadDir(from)
	if err != nil {
		return nil, err
	}
	folders := map[string]int{}
	var parts [][]string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			folders[strings.ToLower(e.Name())] = len(parts)
			parts = append(parts, []string{e.Name()})
		}
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || strings.HasPrefix(n, ".") || listingNames[strings.ToLower(n)] {
			continue
		}
		if k, ok := folders[strings.ToLower(diskStem.ReplaceAllString(n, ""))]; ok {
			parts[k] = append(parts[k], n)
			continue
		}
		parts = append(parts, []string{n})
	}
	sort.SliceStable(parts, func(i, j int) bool { return naturalLess(strings.ToLower(parts[i][0]), strings.ToLower(parts[j][0])) })
	return parts, nil
}

func (a *App) splitItem(ctx context.Context, id int64, user string) ([]int64, error) {
	var state, src, name string
	if err := a.cat.QueryRow("SELECT state FROM items WHERE id = ?", id).Scan(&state); err != nil || state != "live" {
		return nil, fmt.Errorf("A%d is not live", id)
	}
	var n int
	a.cat.QueryRow("SELECT COUNT(*), MIN(path), MIN(name) FROM item_files WHERE item = ?", id).Scan(&n, &src, &name)
	if n != 1 || kindOf(name) != "archive" {
		return nil, fmt.Errorf("A%d is not a single archive", id)
	}
	tmp, err := os.MkdirTemp(a.cfg.StagingDir, "split-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := unpack(ctx, a.archivePath(src), tmp, 16<<30); err != nil {
		return nil, fmt.Errorf("unpacking %s: %v", src, err)
	}
	parts, err := splitParts(tmp)
	if err != nil {
		return nil, err
	}
	var tags []int64
	rows, err := a.cat.Query("SELECT it.tag FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = ? AND t.facet != 'location'", id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t int64
		rows.Scan(&t)
		tags = append(tags, t)
	}
	rows.Close()

	by := "split:" + strconv.FormatInt(id, 10)
	var ids []int64
	touched := map[string]bool{}
	for _, part := range parts {
		title := folderTitle(diskStem.ReplaceAllString(part[0], ""))
		if !isDir(filepath.Join(tmp, part[0])) {
			title = part[0]
		}
		res, err := a.cat.Exec("INSERT INTO items (title, descr, state, created, updated) VALUES (?, ?, 'missing', ?, ?)",
			title, fmt.Sprintf("aus %s (A%d)", name, id), now(), now())
		if err != nil {
			return ids, err
		}
		nid, _ := res.LastInsertId()
		dir := storeDir(nid)
		if err := os.MkdirAll(a.archivePath(dir), 0o2775); err != nil {
			return ids, err
		}
		touched[dir] = true
		for _, e := range part {
			p := filepath.Join(tmp, e)
			if isDir(p) {
				err = copyTree(p, a.archivePath(dir))
			} else {
				err = copyFile(p, a.archivePath(dir+"/"+cleanName(e)))
			}
			if err != nil {
				return ids, err
			}
		}
		tx, err := a.cat.Begin()
		if err != nil {
			return ids, err
		}
		ord := 0
		err = filepath.WalkDir(a.archivePath(dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.Type().IsRegular() {
				return err
			}
			rel, _ := filepath.Rel(a.archivePath(dir), p)
			rel = filepath.ToSlash(rel)
			sum, err := fileSHA256(p)
			if err != nil {
				return err
			}
			st, _ := d.Info()
			if _, err := tx.Exec("INSERT OR IGNORE INTO files (sha256, size, kind, added) VALUES (?, ?, ?, ?)", sum, st.Size(), kindOf(rel), now()); err != nil {
				return err
			}
			if _, err := tx.Exec(`INSERT INTO file_paths (path, sha256, size, mtime, seen) VALUES (?, ?, ?, ?, ?)
			    ON CONFLICT (path) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, mtime = excluded.mtime, seen = excluded.seen`,
				dir+"/"+rel, sum, st.Size(), st.ModTime().Unix(), now()); err != nil {
				return err
			}
			ord++
			_, err = tx.Exec("INSERT INTO item_files (item, sha256, name, path, ord) VALUES (?, ?, ?, ?, ?)", nid, sum, rel, dir+"/"+rel, ord)
			return err
		})
		if err == nil {
			for _, t := range tags {
				if _, err = tx.Exec("INSERT OR IGNORE INTO item_tags (item, tag, added_by, added_at) VALUES (?, ?, ?, ?)", nid, t, user, now()); err != nil {
					break
				}
			}
		}
		if err == nil {
			data, _ := json.Marshal(map[string]any{"from": id, "part": path.Base(part[0])})
			_, err = tx.Exec(`INSERT INTO revisions (target, op, data, user, at, reason, state, batch, decided_by, decided_at)
			    VALUES (?, 'create-item', ?, ?, ?, ?, 'applied', ?, ?, ?)`,
				targetOf("item", nid), string(data), user, now(), fmt.Sprintf("aufgeteilt aus A%d", id), by, user, now())
		}
		if err == nil {
			_, err = tx.Exec("UPDATE items SET state = 'live', rev = 1 WHERE id = ?", nid)
		}
		if err != nil {
			tx.Rollback()
			return ids, err
		}
		if err := tx.Commit(); err != nil {
			return ids, err
		}
		ids = append(ids, nid)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("A%d: the archive is empty", id)
	}
	var rev int
	a.cat.QueryRow("SELECT rev FROM items WHERE id = ?", id).Scan(&rev)
	if _, err := a.cat.Apply(user, []Change{{Target: targetOf("item", id), Op: "withdraw", Rev: rev,
		Data:   map[string]any{"reason": fmt.Sprintf("aufgeteilt in %d Objekte, A%d bis A%d", len(ids), ids[0], ids[len(ids)-1])},
		Reason: "aufgeteilt"}}); err != nil {
		return ids, err
	}
	var changes []Change
	for _, nid := range ids {
		changes = append(changes, Change{Target: targetOf("item", nid)})
	}
	a.cat.refreshFTS(changes)
	return ids, a.refreshArchive(ctx, touched)
}
