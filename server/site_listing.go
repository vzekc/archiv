package archiv

import (
	"bytes"
	"database/sql"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// The folders of the old tree under their old URLs. A folder with a
// hand-written index.html gets that page; any other gets a listing from the
// catalog, laid out as bin/generate-pages.py did: subfolders, a contact sheet
// once there are three pictures or PDFs with a thumbnail, covers for the
// magazine folders, the files, and the ZIP download.

const (
	sheetMin    = 3        // fewer pictures stay in the plain list
	zipMaxBytes = 20 << 30 // larger folders get no ZIP link
)

var coverPrefixes = []string{"Zeitschriften"}

type ListDir struct {
	Path, Name, Label, Descr string
	Count                    int
	Cover, Preview           string
}

type ListFile struct {
	Item       int64
	Path, Name string
	Descr      string
	Size       int64
	Type       string
	Thumb      string
	Preview    string // large first page of a PDF
	IsDoc      bool
}

type Listing struct {
	Path, Heading, Summary string
	Parent                 string // URL of the folder above, "" at the root
	Dirs, Covers           []ListDir
	Files, Sheet           []ListFile
	SheetHeading           string
	ZipFiles               int64
	ZipBytes               int64
	ZipHref                string
	Lightbox, Previews     bool
}

func (a *App) oldPath(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "Methode nicht erlaubt", http.StatusMethodNotAllowed)
		return
	}
	rel, ok := cleanRel(r.PathValue("path"))
	if !ok {
		a.notFound(w, r, "")
		return
	}
	full := filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if err != nil {
		if to := a.movedTo(rel); to != "" {
			http.Redirect(w, r, to, http.StatusMovedPermanently)
			return
		}
		a.notFound(w, r, "Diese Datei oder dieses Verzeichnis gibt es nicht (mehr).")
		return
	}
	if top := strings.Split(rel, "/")[0]; rel != "" && skipTopDirs[top] {
		// previews are files to fetch, never folders to show
		if (top == "thumbs" || top == "previews") && !st.IsDir() {
			w.Header().Set("Cache-Control", "public, max-age=86400")
			a.serveFile(w, r, full, st)
			return
		}
		a.notFound(w, r, "")
		return
	}
	if !st.IsDir() {
		// a listing or entry page of the old site: its folder instead
		if n := strings.ToLower(st.Name()); (n == "index.html" || n == "index.htm") && isGeneratedPage(full) {
			http.Redirect(w, r, href(parentDir(rel))+"/", http.StatusMovedPermanently)
			return
		}
		// Apache serves files itself; this is for whatever reaches the service
		a.serveFile(w, r, full, st)
		return
	}
	if !strings.HasSuffix(r.URL.Path, "/") {
		to := r.URL.Path + "/"
		if r.URL.RawQuery != "" {
			to += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, to, http.StatusMovedPermanently)
		return
	}
	for _, idx := range []string{"index.html", "index.htm"} {
		p := filepath.Join(full, idx)
		if fst, err := os.Stat(p); err == nil && !fst.IsDir() && !isGeneratedPage(p) {
			a.serveFile(w, r, p, fst)
			return
		}
	}
	l, err := a.listing(rel)
	if err != nil {
		logf("listing %s: %v", rel, err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	a.renderSite(w, r, http.StatusOK, "site-listing", &SitePage{
		Title: l.Heading, Crumbs: a.crumbsFor(rel), Scope: rel, Data: l})
}

func (a *App) serveFile(w http.ResponseWriter, r *http.Request, full string, st os.FileInfo) {
	f, err := os.Open(full)
	if err != nil {
		a.notFound(w, r, "")
		return
	}
	defer f.Close()
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// isGeneratedPage tells an index.html written by Filepage,
// bin/generate-pages.py or bin/build-landing.py from a hand-written one.
func isGeneratedPage(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	return bytes.Contains(head, []byte(generatedMarker)) || bytes.Contains(head, []byte(filepageMarker)) ||
		bytes.Contains(head, []byte(landingMarker))
}

// movedTo is where a path of the old tree went, or "".
func (a *App) movedTo(rel string) string {
	var item int64
	var name sql.NullString
	if err := a.cat.QueryRow("SELECT item, name FROM redirects WHERE old_path = ?", rel).Scan(&item, &name); err != nil {
		return ""
	}
	to := "/a/" + strconv.FormatInt(item, 10)
	if name.Valid && name.String != "" {
		to += "/" + strings.TrimPrefix(href(name.String), "/")
	}
	return to
}

// pathRange is the index range of the paths below folder rel.
func pathRange(rel string) (lo, hi string) {
	if rel == "" {
		return "", "\U0010FFFF"
	}
	return rel + "/", rel + "0" // '0' follows '/'
}

func (a *App) lastSync() int64 {
	n, _ := strconv.ParseInt(a.cat.meta("last_sync"), 10, 64)
	return n
}

// treeSize counts the files below a folder as the last sync found them.
func (a *App) treeSize(rel string) (n, bytes int64) {
	lo, hi := pathRange(rel)
	a.cat.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM file_paths WHERE path >= ? AND path < ? AND seen >= ?",
		lo, hi, a.lastSync()).Scan(&n, &bytes)
	return
}

func (a *App) listing(rel string) (*Listing, error) {
	c := a.cat
	l := &Listing{Path: rel}
	var tagID int64
	var title, descr string
	if rel != "" {
		err := c.QueryRow("SELECT id, name, descr FROM tags WHERE facet = 'location' AND path = ?", rel).Scan(&tagID, &title, &descr)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		l.Parent = href(parentDir(rel)) + "/"
		if parentDir(rel) == "" {
			l.Parent = "/"
		}
	}
	l.Heading = title
	if l.Heading == "" {
		l.Heading = path.Base(rel)
		if rel == "" {
			l.Heading = "Archiv"
		}
	}

	// subfolders, with the number of their entries
	var dirs []ListDir
	q := "SELECT id, path, name, descr FROM tags WHERE facet = 'location' AND parent = ? AND merged_into IS NULL"
	args := []any{tagID}
	if rel == "" {
		q = "SELECT id, path, name, descr FROM tags WHERE facet = 'location' AND parent IS NULL AND merged_into IS NULL AND path IS NOT NULL"
		args = nil
	}
	rows, err := c.Query(q, args...)
	if err != nil {
		return nil, err
	}
	type sub struct {
		id int64
		ListDir
	}
	var subs []sub
	for rows.Next() {
		var s sub
		rows.Scan(&s.id, &s.Path, &s.Label, &s.Descr)
		s.Name = path.Base(s.Path)
		subs = append(subs, s)
	}
	rows.Close()
	last := a.lastSync()
	for _, s := range subs {
		var nd, nf int
		c.QueryRow("SELECT COUNT(*) FROM tags WHERE facet = 'location' AND parent = ?", s.id).Scan(&nd)
		lo, hi := pathRange(s.Path)
		c.QueryRow(`SELECT COUNT(*) FROM file_paths WHERE path >= ? AND path < ? AND seen >= ?
		    AND instr(substr(path, ?), '/') = 0`, lo, hi, last, len(lo)+1).Scan(&nf)
		if nd+nf == 0 && !a.dirOnDisk(s.Path) {
			continue // a folder that is gone
		}
		s.Count = nd + nf
		dirs = append(dirs, s.ListDir)
	}
	sort.Slice(dirs, func(i, j int) bool { return strings.ToLower(dirs[i].Name) < strings.ToLower(dirs[j].Name) })

	// the files lying directly in this folder
	lo, hi := pathRange(rel)
	rows, err = c.Query(`SELECT f.item, f.path, fp.size, i.descr, i.root IS NOT NULL
	    FROM item_files f JOIN file_paths fp ON fp.path = f.path JOIN items i ON i.id = f.item
	    WHERE f.path >= ? AND f.path < ? AND instr(substr(f.path, ?), '/') = 0 AND fp.seen >= ?
	    ORDER BY f.path`, lo, hi, len(lo)+1, last)
	if err != nil {
		return nil, err
	}
	var files []ListFile
	for rows.Next() {
		var f ListFile
		var inMirror bool
		rows.Scan(&f.Item, &f.Path, &f.Size, &f.Descr, &inMirror)
		if inMirror {
			f.Descr = "" // the mirror's description belongs to the whole site
		}
		f.Name = path.Base(f.Path)
		f.Type = typeHint(f.Name)
		files = append(files, f)
	}
	rows.Close()
	sort.SliceStable(files, func(i, j int) bool { return files[i].Name < files[j].Name })

	// pictures and documents with a thumbnail make a contact sheet
	var sheet, rest []ListFile
	for _, f := range files {
		e := extOf(f.Name)
		if imageExts["."+e] || e == "pdf" {
			if t := a.thumbFor("thumbs", f.Path); t != "" {
				f.Thumb = t
				f.IsDoc = e == "pdf"
				if f.IsDoc {
					f.Preview = a.previewFor(f.Path)
				}
				sheet = append(sheet, f)
				continue
			}
		}
		rest = append(rest, f)
	}
	if len(sheet) < sheetMin {
		sheet, rest = nil, files
	}
	l.Sheet, l.Files = sheet, rest
	if len(sheet) > 0 {
		docs := 0
		for _, f := range sheet {
			if f.IsDoc {
				docs++
			} else {
				l.Lightbox = true
			}
		}
		switch {
		case docs == 0:
			l.SheetHeading = strconv.Itoa(len(sheet)) + " Bilder"
		case docs == len(sheet):
			word := "Dokumente"
			if strings.HasPrefix(rel+"/", "Zeitschriften/") {
				word = "Hefte"
			}
			l.SheetHeading = strconv.Itoa(docs) + " " + word
		default:
			l.SheetHeading = strconv.Itoa(len(sheet)) + " Bilder und Dokumente"
		}
		if docs > 0 {
			l.Previews = true
		}
	}

	// below the magazines, subfolders show the cover of their first issue
	if underCoverPrefix(rel) {
		memo := map[string]string{}
		var withCover, plain []ListDir
		for _, d := range dirs {
			if cv := a.coverOf(d.Path, memo, 0); cv != "" {
				d.Cover = a.thumbFor("thumbs", cv)
				d.Preview = a.previewFor(cv)
				withCover = append(withCover, d)
			} else {
				plain = append(plain, d)
			}
		}
		if len(withCover) >= sheetMin {
			l.Covers, dirs = withCover, plain
			l.Previews = true
		}
	}
	l.Dirs = dirs

	var bits []string
	if descr != "" {
		bits = append(bits, descr)
	}
	if n := len(l.Dirs) + len(l.Covers); n > 0 {
		bits = append(bits, plural(n, "Unterverzeichnis", "Unterverzeichnisse"))
	}
	if len(files) > 0 {
		var total int64
		for _, f := range files {
			total += f.Size
		}
		bits = append(bits, plural(len(files), "Datei", "Dateien")+" · "+humanSize(total))
	}
	l.Summary = strings.Join(bits, " · ")
	if l.Summary == "" {
		l.Summary = "leer"
	}
	l.ZipFiles, l.ZipBytes = a.treeSize(rel)
	l.ZipHref = zipHref(rel)
	return l, nil
}

// zipHref is the ZIP download of a folder; slashes stay readable.
func zipHref(rel string) string {
	return "/zip?p=" + strings.ReplaceAll(url.QueryEscape(rel), "%2F", "/")
}

func underCoverPrefix(rel string) bool {
	for _, p := range coverPrefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// coverOf is the PDF whose first page stands for a folder: its own first
// PDF with a thumbnail, else the cover of its first subfolder that has one.
func (a *App) coverOf(rel string, memo map[string]string, depth int) string {
	if c, ok := memo[rel]; ok {
		return c
	}
	memo[rel] = ""
	if depth > 4 {
		return ""
	}
	lo, hi := pathRange(rel)
	rows, err := a.cat.Query(`SELECT path FROM file_paths WHERE path >= ? AND path < ? AND seen >= ?
	    AND instr(substr(path, ?), '/') = 0 AND lower(path) LIKE '%.pdf' ORDER BY path LIMIT 20`, lo, hi, a.lastSync(), len(lo)+1)
	if err == nil {
		var cands []string
		for rows.Next() {
			var p string
			rows.Scan(&p)
			cands = append(cands, p)
		}
		rows.Close()
		for _, p := range cands {
			if a.thumbFor("thumbs", p) != "" {
				memo[rel] = p
				return p
			}
		}
	}
	rows, err = a.cat.Query(`SELECT path FROM tags WHERE facet = 'location' AND parent = (SELECT id FROM tags WHERE facet = 'location' AND path = ?)
	    ORDER BY lower(path)`, rel)
	if err != nil {
		return ""
	}
	var subs []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		subs = append(subs, p)
	}
	rows.Close()
	for _, s := range subs {
		if c := a.coverOf(s, memo, depth+1); c != "" {
			memo[rel] = c
			return c
		}
	}
	return ""
}

func (a *App) dirOnDisk(rel string) bool {
	st, err := os.Stat(filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(rel)))
	return err == nil && st.IsDir()
}
