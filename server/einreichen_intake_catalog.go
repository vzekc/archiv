package archiv

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Intake into the catalog: every accepted file becomes an item, or a group
// of disk images becomes one item with many files; the files go to
// objekte/<id/1000>/<id>/, unpacked archives with their inner structure.
// The items carry the tags the moderator chose, the source "Einreichung"
// and the language the AI found.

type intakeItem struct {
	id    int64
	title string
	descr string
	files []*File
	tags  []int64
	part  string // of an archive split into items: the top folder or file it is
}

func (a *App) doIntakeCatalog(ctx context.Context, s *Submission) error {
	files, err := a.db.files("submission = ? AND status = ? ORDER BY id", s.ID, FReady)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return nil
	}
	credit := ""
	if s.Credit {
		credit = "eingereicht von " + a.names.Name(s.Owner)
	}

	// the tags the moderator chose, created where they do not exist yet
	tagsOf := map[string][]int64{}
	for _, f := range files {
		if _, ok := tagsOf[f.Tags]; ok {
			continue
		}
		t, err := a.intakeTags(s, append(tagLines(f.Tags), "Quelle: Einreichung"))
		if err != nil {
			return err
		}
		tagsOf[f.Tags] = t
	}

	// archives split into items are unpacked first: their top folders and
	// files are the items
	unpacked := map[int64]string{} // file -> the folder its contents are in
	unpackOnce := func(f *File) (string, error) {
		if from, ok := unpacked[f.ID]; ok {
			return from, nil
		}
		src := filepath.Join(a.cfg.StagingDir, f.Path)
		info := parseInfo(f.Info)
		tmp := filepath.Join(filepath.Dir(src), "unpacked-"+strconv.FormatInt(f.ID, 10))
		os.RemoveAll(tmp)
		if err := unpack(ctx, src, tmp, unpackLimit(info)); err != nil {
			return "", fmt.Errorf("%s entpacken: %v", f.Name, err)
		}
		from := tmp
		if info.SingleRoot != "" {
			if st, err := os.Stat(filepath.Join(tmp, info.SingleRoot)); err == nil && st.IsDir() {
				from = filepath.Join(tmp, info.SingleRoot)
			}
		}
		if bdir := filepath.Join(a.cfg.BackupDir, strconv.FormatInt(s.ID, 10)); os.MkdirAll(bdir, 0o2775) == nil {
			copyFile(src, filepath.Join(bdir, f.Name))
		}
		unpacked[f.ID] = from
		return from, nil
	}
	var items []*intakeItem
	split := map[int64]bool{}
	for _, f := range files {
		if !f.Split || !f.Unpack || f.Kind != "archive" {
			continue
		}
		from, err := unpackOnce(f)
		if err != nil {
			return err
		}
		entries, err := os.ReadDir(from)
		if err != nil {
			return err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") || listingNames[strings.ToLower(e.Name())] {
				continue
			}
			descr := "aus " + f.Name
			if credit != "" {
				descr += ", " + credit
			}
			items = append(items, &intakeItem{files: []*File{f}, tags: tagsOf[f.Tags], part: e.Name(), title: e.Name(), descr: descr})
		}
		split[f.ID] = true
	}

	// items: disk sets stay together, everything else one item per file
	groups := map[string][]*File{}
	var order []string
	for _, f := range files {
		if split[f.ID] {
			continue
		}
		key := strconv.FormatInt(f.ID, 10)
		if sg := parseSuggestion(f.Suggestion); sg != nil && (f.Kind == "disk" || f.Kind == "iso") && !(f.Unpack && f.Kind == "archive") {
			key = "g\x00" + f.Tags + "\x00" + sg.GroupKey()
		}
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], f)
	}
	for _, key := range order {
		fs := groups[key]
		it := &intakeItem{files: fs, tags: tagsOf[fs[0].Tags]}
		if lang := a.itemLanguage(fs); lang != "" && !strings.Contains("\n"+fs[0].Tags, "\nSprache:") {
			t, err := a.intakeTags(s, []string{"Sprache: " + lang})
			if err != nil {
				return err
			}
			it.tags = append(slices.Clip(it.tags), t...)
		}
		if len(fs) == 1 {
			it.title = strings.TrimSpace(fs[0].Description)
			if it.title == "" {
				it.title = fs[0].Name
			}
			it.descr = strings.TrimSpace(fs[0].Description)
		} else {
			sg := parseSuggestion(fs[0].Suggestion)
			it.title = fmt.Sprintf("%d Dateien aus Einreichung %d", len(fs), s.ID)
			if sg != nil && sg.Group != "" {
				it.title = sg.Group
			}
			if sg != nil {
				it.descr = sg.Reason
			}
		}
		if credit != "" {
			it.descr = strings.TrimSpace(strings.TrimSuffix(it.descr, ".") + ". " + credit)
			it.descr = strings.TrimPrefix(it.descr, ". ")
		}
		items = append(items, it)
	}

	// numbers first: the place on the share follows from them
	tx, err := a.cat.Begin()
	if err != nil {
		return err
	}
	for _, it := range items {
		res, err := tx.Exec("INSERT INTO items (title, descr, state, created, updated) VALUES (?, ?, 'missing', ?, ?)", it.title, it.descr, now(), now())
		if err != nil {
			tx.Rollback()
			return err
		}
		it.id, _ = res.LastInsertId()
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	undo := func() {
		for _, it := range items {
			os.RemoveAll(a.archivePath(storeDir(it.id)))
			a.cat.Exec("DELETE FROM item_files WHERE item = ?", it.id)
			a.cat.Exec("DELETE FROM items WHERE id = ?", it.id)
		}
	}

	type placed struct {
		item       int64
		name, path string
		sha, descr string
		size, mt   int64
		file       *File
	}
	var all []placed
	have := map[string]bool{}
	touched := map[string]bool{}
	for _, it := range items {
		dir := storeDir(it.id)
		if err := os.MkdirAll(a.archivePath(dir), 0o2775); err != nil {
			undo()
			return err
		}
		touched[dir] = true
		for _, f := range it.files {
			if ctx.Err() != nil {
				undo()
				return ctx.Err()
			}
			src := filepath.Join(a.cfg.StagingDir, f.Path)
			if f.Unpack && f.Kind == "archive" {
				from, err := unpackOnce(f)
				if err != nil {
					undo()
					return err
				}
				switch entry := filepath.Join(from, it.part); {
				case it.part == "":
					err = copyTree(from, a.archivePath(dir))
				case isDir(entry):
					err = copyTree(entry, a.archivePath(dir))
				default:
					err = copyFile(entry, a.archivePath(dir+"/"+cleanName(it.part)))
				}
				if err != nil {
					undo()
					return fmt.Errorf("%s ins Archiv kopieren: %v", f.Name, err)
				}
				err = filepath.WalkDir(a.archivePath(dir), func(p string, d fs.DirEntry, err error) error {
					if err != nil || !d.Type().IsRegular() {
						return err
					}
					rel, _ := filepath.Rel(a.archivePath(dir), p)
					rel = filepath.ToSlash(rel)
					if have[dir+"/"+rel] {
						return nil
					}
					have[dir+"/"+rel] = true
					sum, err := fileSHA256(p)
					if err != nil {
						return err
					}
					st, _ := d.Info()
					all = append(all, placed{item: it.id, name: rel, path: dir + "/" + rel, sha: sum, size: st.Size(), mt: st.ModTime().Unix(), file: f})
					return nil
				})
				if err != nil {
					undo()
					return err
				}
				continue
			}
			name := freeName(a.archivePath(dir), cleanName(f.Name))
			dst := a.archivePath(dir + "/" + name)
			if err := copyFile(src, dst); err != nil {
				undo()
				return fmt.Errorf("%s ins Archiv kopieren: %v", f.Name, err)
			}
			st, err := os.Stat(dst)
			if err != nil {
				undo()
				return err
			}
			d := ""
			if len(it.files) > 1 {
				d = strings.TrimSpace(f.Description)
			}
			have[dir+"/"+name] = true
			all = append(all, placed{item: it.id, name: name, path: dir + "/" + name, sha: f.SHA256, descr: d, size: st.Size(), mt: st.ModTime().Unix(), file: f})
		}
	}

	// the catalog in one go
	tx, err = a.cat.Begin()
	if err != nil {
		undo()
		return err
	}
	defer tx.Rollback()
	ord := map[int64]int{}
	for _, p := range all {
		if _, err := tx.Exec("INSERT OR IGNORE INTO files (sha256, size, kind, added) VALUES (?, ?, ?, ?)", p.sha, p.size, kindOf(p.name), now()); err != nil {
			undo()
			return err
		}
		if _, err := tx.Exec(`INSERT INTO file_paths (path, sha256, size, mtime, seen) VALUES (?, ?, ?, ?, ?)
		    ON CONFLICT (path) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size, mtime = excluded.mtime, seen = excluded.seen`,
			p.path, p.sha, p.size, p.mt, now()); err != nil {
			undo()
			return err
		}
		ord[p.item]++
		if _, err := tx.Exec("INSERT INTO item_files (item, sha256, name, path, ord, descr) VALUES (?, ?, ?, ?, ?, ?)",
			p.item, p.sha, p.name, p.path, ord[p.item], p.descr); err != nil {
			undo()
			return err
		}
	}
	by := "einreichen:" + strconv.FormatInt(s.ID, 10)
	for _, it := range items {
		for _, t := range it.tags {
			if _, err := tx.Exec("INSERT OR IGNORE INTO item_tags (item, tag, added_by, added_at) VALUES (?, ?, ?, ?)", it.id, t, by, now()); err != nil {
				undo()
				return err
			}
		}
		data, _ := json.Marshal(map[string]any{"submission": s.ID, "files": len(it.files)})
		tx.Exec(`INSERT INTO revisions (target, op, data, user, at, reason, state, batch, decided_by, decided_at)
		    VALUES (?, 'create-item', ?, ?, ?, 'Einreichung', 'applied', ?, ?, ?)`,
			targetOf("item", it.id), string(data), s.Owner, now(), by, s.DecidedBy.String, now())
		if _, err := tx.Exec("UPDATE items SET state = 'live', rev = 1 WHERE id = ?", it.id); err != nil {
			undo()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		undo()
		return err
	}
	var changes []Change
	for _, it := range items {
		changes = append(changes, Change{Target: targetOf("item", it.id)})
	}
	a.cat.refreshFTS(changes)
	for _, p := range all {
		a.db.Exec("UPDATE files SET status = ?, archive_path = ? WHERE id = ? AND status = ?", FTaken, p.path, p.file.ID, FReady)
	}
	for _, it := range items {
		a.db.logEvent("system", s.ID, "file taken in", "A"+strconv.FormatInt(it.id, 10)+" "+it.title)
	}
	return a.refreshArchive(ctx, touched)
}

// intakeTags finds the tags given as "Facette: A › B" and creates those
// that do not exist yet, in the name of the moderator who accepted.
func (a *App) intakeTags(s *Submission, tags []string) ([]int64, error) {
	var out []int64
	for _, t := range tags {
		id, err := a.resolveTag(t)
		if err != nil {
			if id, err = a.createTagPath(s.DecidedBy.String, t); err != nil {
				return nil, fmt.Errorf("Schlagwort %s: %v", t, err)
			}
		}
		out = append(out, id)
	}
	return out, nil
}

// itemLanguage is the language the AI found for the files of an item, if
// they share one.
func (a *App) itemLanguage(fs []*File) string {
	lang := ""
	for i, f := range fs {
		l := ""
		if sg := parseSuggestion(f.Suggestion); sg != nil {
			l = sg.Language
		}
		if i > 0 && l != lang {
			return ""
		}
		lang = l
	}
	if !langRe.MatchString(lang) {
		return ""
	}
	return lang
}

func facetIsTree(f string) bool { return f == "machine" || f == "publication" || f == "event" }

// folderLabel is the name of a folder of the old tree as the catalog
// knows it.
func (c *Catalog) folderLabel(p string) string {
	var n string
	if c.QueryRow("SELECT name FROM tags WHERE facet = 'location' AND path = ?", p).Scan(&n) == nil && n != "" {
		return n
	}
	return path.Base(p)
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}
