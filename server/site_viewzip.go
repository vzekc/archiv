package archiv

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
)

// A view as one ZIP file: every item its filters select, each in a folder
// "<number>-<name>/" with its files. The view page offers it when the set
// is small enough to size up quickly and the files fit under zipMaxBytes.

const viewZipMaxItems = 2000 // larger sets get no ZIP link

type viewZipFile struct {
	Item int64
	Name string
	Path string
	Size int64
}

// viewZipFiles lists the present files of the items, in the items' order
// and each item's file order, with their total size.
func (a *App) viewZipFiles(ids []int64) ([]viewZipFile, int64, error) {
	last := a.lastSync()
	byItem := map[int64][]viewZipFile{}
	var total int64
	for from := 0; from < len(ids); from += 500 {
		chunk := ids[from:min(from+500, len(ids))]
		args := []any{last}
		marks := make([]string, len(chunk))
		for i, id := range chunk {
			args = append(args, id)
			marks[i] = "?"
		}
		rows, err := a.cat.Query(`SELECT f.item, f.name, f.path, COALESCE(p.size, fl.size)
		    FROM item_files f JOIN files fl ON fl.sha256 = f.sha256 JOIN file_paths p ON p.path = f.path
		    WHERE p.seen >= ? AND f.item IN (`+strings.Join(marks, ",")+`) ORDER BY f.item, f.ord, f.name`, args...)
		if err != nil {
			return nil, 0, err
		}
		for rows.Next() {
			var f viewZipFile
			rows.Scan(&f.Item, &f.Name, &f.Path, &f.Size)
			byItem[f.Item] = append(byItem[f.Item], f)
			total += f.Size
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, 0, err
		}
	}
	var out []viewZipFile
	for _, id := range ids {
		out = append(out, byItem[id]...)
	}
	return out, total, nil
}

// viewItemIDs are the items of the filters, in title order.
func (a *App) viewItemIDs(filters []Filter) []int64 {
	idx := a.index()
	set := idx.set(filters)
	ids := make([]int64, len(set))
	for i, n := range set {
		ids[i] = idx.items[n].ID
	}
	return ids
}

// viewZipSize is the size of the view's ZIP, or 0 when the view offers none.
func (a *App) viewZipSize(filters []Filter, total int) int64 {
	if len(filters) == 0 || total == 0 || total > viewZipMaxItems {
		return 0
	}
	_, size, err := a.viewZipFiles(a.viewItemIDs(filters))
	if err != nil {
		logf("view zip: %v", err)
		return 0
	}
	if size > zipMaxBytes {
		return 0
	}
	return size
}

func (a *App) viewZip(w http.ResponseWriter, r *http.Request, filters []Filter, title string) {
	ids := a.viewItemIDs(filters)
	if len(filters) == 0 || len(ids) == 0 {
		a.zipFail(w, r, http.StatusNotFound, "Nichts herunterzuladen", "Diese Ansicht enthält keine Objekte.", "")
		return
	}
	if len(ids) > viewZipMaxItems {
		a.zipFail(w, r, http.StatusRequestEntityTooLarge, "Zu groß für einen ZIP-Download",
			"Diese Ansicht enthält "+strconv.Itoa(len(ids))+" Objekte; bitte weiter eingrenzen.", "")
		return
	}
	files, size, err := a.viewZipFiles(ids)
	if err != nil {
		logf("view zip: %v", err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	if len(files) == 0 {
		a.zipFail(w, r, http.StatusNotFound, "Nichts herunterzuladen", "Die Objekte dieser Ansicht haben keine Dateien im Archiv.", "")
		return
	}
	if size > zipMaxBytes {
		a.zipFail(w, r, http.StatusRequestEntityTooLarge, "Zu groß für einen ZIP-Download",
			"Die Dateien dieser Ansicht sind zusammen "+humanSize(size)+" groß; bitte weiter eingrenzen.", "")
		return
	}
	user := "anonymous:" + clientIP(r)
	if u := currentUser(r); u != nil && u.ID != "" {
		user = u.ID
	}
	release, status, msg := zips.take(user)
	if release == nil {
		a.zipFail(w, r, status, "Bitte warten", msg, "")
		return
	}
	defer release()
	titles := map[int64]string{}
	idx := a.index()
	for _, id := range ids {
		if n, ok := idx.byID[id]; ok {
			titles[id] = idx.items[n].Title
		}
	}
	base := slugify(title)
	if base == "" {
		base = "archiv"
	}
	streamZip(w, r, base, func(add func(e zipEntry) bool) {
		for _, f := range files {
			dir := strconv.FormatInt(f.Item, 10)
			if s := slugify(titles[f.Item]); s != "" {
				dir += "-" + s
			}
			if !add(zipEntry{Name: base + "/" + dir + "/" + f.Name, Full: filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(f.Path))}) {
				return
			}
		}
	}, size, func(n int, sent int64, err error) {
		if err != nil {
			logf("zip: view %s after %d files: %v", base, n, err)
		}
	})
}
