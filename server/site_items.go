package archiv

import (
	"database/sql"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// Item pages under their citable URLs:
//
//	/a/12345                 the item
//	/a/12345/<slug>          the same; a slug that is not current redirects
//	/a/12345/<file name>     a file of the item
//	/a/12345/datei/2         its n-th file
//	/a/12345/zip             all its files as one ZIP
//	/h/<sha256>              exactly these bytes, wherever they lie

type ItemView struct {
	ID         int64
	Title      string
	Heading    string // the title, or for a bare file name its readable form
	Descr      string
	Year       string
	State      string
	MergedInto int64
	Withdrawn  string
	Root       string
	Slug       string
	Files      []ItemFileView
	Tags       []TagRef
	Bytes      int64
	Missing    int
	Canonical  string
	ZipHref    string
	Rev        int
	Rels       []RelView
	Comments   []*CommentView
	Proposed   int // open proposals for this item
	History    bool

	// for the view
	Kind      string // image, pdf, other
	Preview   string // the large picture
	Open      string // the file to open
	TagLinks  []TagLink
	Near      []Card
	NearLabel string
	NearHref  string
	// opened from a search: the pages its PDFs have hits on
	Q         string
	Finds     []Find
	FindsMore int
}

type TagLink struct {
	Facet, Label, Href string
}

type RelView struct {
	Kind, Label string
	Item        int64
	Title       string
	Outgoing    bool
}

type ItemFileView struct {
	N          int
	Name, Path string
	Href       string
	Size       int64
	Role       string
	Type       string
	Present    bool
	Thumb      string
	Sha        string
}

func (a *App) loadItem(id int64) (*ItemView, error) {
	it := &ItemView{ID: id}
	var merged sql.NullInt64
	var root sql.NullString
	err := a.cat.QueryRow("SELECT title, descr, year, state, merged_into, withdrawn_reason, root, rev FROM items WHERE id = ?", id).
		Scan(&it.Title, &it.Descr, &it.Year, &it.State, &merged, &it.Withdrawn, &root, &it.Rev)
	if err != nil {
		return nil, err
	}
	it.MergedInto = merged.Int64
	it.Root = root.String
	it.Slug = slugify(it.Title)
	it.Canonical = siteURL + "/a/" + strconv.FormatInt(id, 10)
	last := a.lastSync()
	rows, err := a.cat.Query(`SELECT f.name, f.path, f.role, f.sha256, COALESCE(p.size, fl.size), COALESCE(p.seen, 0) >= ?
	    FROM item_files f JOIN files fl ON fl.sha256 = f.sha256 LEFT JOIN file_paths p ON p.path = f.path
	    WHERE f.item = ? ORDER BY f.ord, f.name`, last, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var f ItemFileView
		rows.Scan(&f.Name, &f.Path, &f.Role, &f.Sha, &f.Size, &f.Present)
		f.N = len(it.Files) + 1
		f.Type = typeHint(f.Name)
		f.Href = "/a/" + strconv.FormatInt(id, 10) + "/" + strings.TrimPrefix(href(f.Name), "/")
		if !f.Present {
			it.Missing++
		} else {
			it.Bytes += f.Size
		}
		it.Files = append(it.Files, f)
	}
	rows.Close()
	it.Heading = it.Title
	if len(it.Files) == 1 && it.Title == it.Files[0].Name {
		it.Heading = prettyName(it.Title)
	}
	if len(it.Files) <= 50 {
		for i := range it.Files {
			if e := extOf(it.Files[i].Name); e == "pdf" || imageExts["."+e] {
				it.Files[i].Thumb = a.thumbFor("thumbs", it.Files[i].Path)
			}
		}
	}
	if len(it.Files) > 1 {
		it.ZipHref = "/a/" + strconv.FormatInt(id, 10) + "/zip"
	}
	rows, err = a.cat.Query(`SELECT r.kind, r.b, i.title, 1 FROM item_rel r JOIN items i ON i.id = r.b WHERE r.a = ?
	    UNION ALL SELECT r.kind, r.a, i.title, 0 FROM item_rel r JOIN items i ON i.id = r.a WHERE r.b = ?`, id, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var rv RelView
		rows.Scan(&rv.Kind, &rv.Item, &rv.Title, &rv.Outgoing)
		rv.Label = relKinds[rv.Kind]
		if !rv.Outgoing {
			rv.Label = relInverse[rv.Kind]
		}
		it.Rels = append(it.Rels, rv)
	}
	rows.Close()
	a.cat.QueryRow("SELECT COUNT(*) FROM revisions WHERE target = ? AND state = 'proposed'", targetOf("item", id)).Scan(&it.Proposed)
	a.cat.QueryRow("SELECT EXISTS (SELECT 1 FROM revisions WHERE target = ?)", targetOf("item", id)).Scan(&it.History)
	rows, err = a.cat.Query("SELECT t.id FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = ? ORDER BY t.facet, t.name", id)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var t int64
		rows.Scan(&t)
		ids = append(ids, t)
	}
	rows.Close()
	for _, t := range ids {
		if ref := a.tagRef(t); ref != nil {
			if ref.Facet != "location" {
				it.Tags = append(it.Tags, *ref)
			}
		}
	}
	return it, nil
}

func (a *App) itemID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	s := strings.TrimPrefix(strings.ToUpper(r.PathValue("id")), "A")
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		a.notFound(w, r, "")
		return 0, false
	}
	return id, true
}

func (a *App) itemPage(w http.ResponseWriter, r *http.Request) {
	id, ok := a.itemID(w, r)
	if !ok {
		return
	}
	it, err := a.loadItem(id)
	if err == sql.ErrNoRows {
		a.notFound(w, r, "Ein Objekt mit dieser Nummer gibt es nicht.")
		return
	} else if err != nil {
		logf("item %d: %v", id, err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	if it.State == "merged" && it.MergedInto != 0 {
		http.Redirect(w, r, "/a/"+strconv.FormatInt(it.MergedInto, 10), http.StatusMovedPermanently)
		return
	}
	it.Comments = a.comments(targetOf("item", id))
	a.forView(it)
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		it.Q = q
		it.Finds, it.FindsMore = a.finds(it, q, maxFinds)
	}
	if r.URL.Query().Get("teil") == "panel" {
		a.renderSite(w, r, http.StatusOK, "v-itempanel", &SitePage{Title: it.Title, Data: it})
		return
	}
	p := &SitePage{Title: it.Title, Data: it}
	if t := it.treeTag(); t != nil {
		p.ScopeView, p.ScopeTags = t.Name, strconv.FormatInt(t.ID, 10)
	}
	a.renderSite(w, r, http.StatusOK, "v-item", p)
}

// itemSub serves what lies below an item's URL: a file by name or number,
// the ZIP, or the item page under any slug.
func (a *App) itemSub(w http.ResponseWriter, r *http.Request) {
	id, ok := a.itemID(w, r)
	if !ok {
		return
	}
	rest := r.PathValue("rest")
	it, err := a.loadItem(id)
	if err != nil {
		a.notFound(w, r, "Ein Objekt mit dieser Nummer gibt es nicht.")
		return
	}
	if it.State == "merged" && it.MergedInto != 0 {
		http.Redirect(w, r, "/a/"+strconv.FormatInt(it.MergedInto, 10)+"/"+strings.TrimPrefix(href(rest), "/"), http.StatusMovedPermanently)
		return
	}
	if rest == "lesen" {
		a.reader(w, r, it)
		return
	}
	if rest == "zip" && len(it.Files) > 1 {
		a.itemZip(w, r, it)
		return
	}
	var f *ItemFileView
	if n, ok := strings.CutPrefix(rest, "datei/"); ok {
		if i, err := strconv.Atoi(n); err == nil && i >= 1 && i <= len(it.Files) {
			f = &it.Files[i-1]
		}
	}
	for i := range it.Files {
		if f == nil && it.Files[i].Name == rest {
			f = &it.Files[i]
		}
	}
	if f == nil {
		// a file renamed, or taken out into another item
		var to int64
		var toName string
		if a.cat.QueryRow("SELECT item, name FROM redirects WHERE old_path = ?", fmt.Sprintf("a/%d/%s", id, rest)).Scan(&to, &toName) == nil && (to != id || toName != rest) {
			http.Redirect(w, r, "/a/"+strconv.FormatInt(to, 10)+"/"+strings.TrimPrefix(href(toName), "/"), http.StatusMovedPermanently)
			return
		}
		var name string
		if a.cat.QueryRow("SELECT name FROM redirects WHERE item = ? AND old_path LIKE '%/' || ?", id, rest).Scan(&name) == nil && name != rest {
			http.Redirect(w, r, "/a/"+strconv.FormatInt(id, 10)+"/"+strings.TrimPrefix(href(name), "/"), http.StatusMovedPermanently)
			return
		}
		if !strings.Contains(rest, "/") && !strings.Contains(rest, ".") {
			// a slug: the page itself
			http.Redirect(w, r, "/a/"+strconv.FormatInt(id, 10), http.StatusFound)
			return
		}
		a.notFound(w, r, "Diese Datei gehört nicht zu dem Objekt.")
		return
	}
	a.serveArchivePath(w, r, f.Path, f.Sha)
}

// serveArchivePath sends a file of the archive; if it is not where the
// catalog says, any other copy of the same bytes will do.
func (a *App) serveArchivePath(w http.ResponseWriter, r *http.Request, rel, sha string) {
	full := filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if err != nil && sha != "" {
		rows, qerr := a.cat.Query("SELECT path FROM file_paths WHERE sha256 = ? ORDER BY seen DESC", sha)
		if qerr == nil {
			for rows.Next() && err != nil {
				var p string
				rows.Scan(&p)
				full = filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(p))
				st, err = os.Stat(full)
			}
			rows.Close()
		}
	}
	if err != nil || st.IsDir() {
		a.notFound(w, r, "Die Datei ist im Moment nicht auf dem Speicher.")
		return
	}
	f, err := os.Open(full)
	if err != nil {
		a.notFound(w, r, "")
		return
	}
	defer f.Close()
	if ct := contentTypes[extOf(rel)]; ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, path.Base(rel), st.ModTime(), f)
}

// contentTypes for what Go would guess wrong or call octet-stream although
// the browser can show it.
var contentTypes = map[string]string{
	"pdf": "application/pdf", "txt": "text/plain; charset=utf-8", "djvu": "image/vnd.djvu",
	"md": "text/plain; charset=utf-8", "asm": "text/plain; charset=utf-8", "mac": "text/plain; charset=utf-8",
}

func (a *App) byHash(w http.ResponseWriter, r *http.Request) {
	sha := strings.ToLower(r.PathValue("sha"))
	if len(sha) != 64 || strings.Trim(sha, "0123456789abcdef") != "" {
		a.notFound(w, r, "")
		return
	}
	var p string
	if err := a.cat.QueryRow("SELECT path FROM file_paths WHERE sha256 = ? ORDER BY seen DESC, path LIMIT 1", sha).Scan(&p); err != nil {
		a.notFound(w, r, "Dateien mit dieser Prüfsumme gibt es im Archiv nicht.")
		return
	}
	a.serveArchivePath(w, r, p, sha)
}

func (a *App) itemZip(w http.ResponseWriter, r *http.Request, it *ItemView) {
	if it.Bytes > zipMaxBytes {
		a.zipFail(w, r, http.StatusRequestEntityTooLarge, "Zu groß für einen ZIP-Download",
			"Die Dateien dieses Objekts sind zusammen "+humanSize(it.Bytes)+" groß; bitte einzeln herunterladen.", "")
		return
	}
	user := "anonymous"
	if u := currentUser(r); u != nil {
		user = u.ID
	}
	release, status, msg := zips.take(user)
	if release == nil {
		a.zipFail(w, r, status, "Bitte warten", msg, "")
		return
	}
	defer release()
	base := strconv.FormatInt(it.ID, 10) + "-" + it.Slug
	streamZip(w, r, base, func(add func(e zipEntry) bool) {
		for _, f := range it.Files {
			if !f.Present {
				continue
			}
			if !add(zipEntry{Name: base + "/" + f.Name, Full: filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(f.Path))}) {
				return
			}
		}
	}, it.Bytes, func(files int, sent int64, err error) {
		if err != nil {
			logf("zip: item %d after %d files: %v", it.ID, files, err)
		}
	})
}

// forView adds what the item page of the views shows: the picture, the
// file to open, the tags as ways into the views, and what lies nearby.
func (a *App) forView(it *ItemView) {
	if len(it.Files) > 0 {
		f := it.Files[0]
		e := extOf(f.Name)
		// the cover shows what the main file is, which opens
		var cover *ItemFileView
		for i := range it.Files {
			if it.Files[i].Role == "cover" && imageExts["."+extOf(it.Files[i].Name)] {
				cover = &it.Files[i]
				break
			}
		}
		switch {
		case it.Root != "":
			it.Kind, it.Open = "other", href(it.Root)+"/"
		case cover != nil && !imageExts["."+e]:
			it.Kind, it.Preview, it.Open = "image", cover.Href, f.Href
		case imageExts["."+e]:
			it.Kind, it.Preview, it.Open = "image", f.Href, f.Href
		case e == "pdf":
			it.Kind, it.Open = "pdf", f.Href
			if a.thumbFor("thumbs", f.Path) != "" {
				it.Preview = a.previewFor(f.Path)
			}
		default:
			it.Kind, it.Open = "other", f.Href
		}
	}
	for _, t := range it.Tags {
		it.TagLinks = append(it.TagLinks, TagLink{Facet: t.Facet, Label: facetLabels[t.Facet] + ": " + t.Path,
			Href: a.viewURL([]Filter{{Facet: t.Facet, Tag: t.ID}})})
	}
	// nearby: the other items of its group, else of its machine, magazine
	// or meeting
	t := it.deepest("group")
	if t == nil {
		t = it.treeTag()
	}
	if t != nil {
		if cs, err := a.cards(`SELECT i.id FROM items i JOIN item_tags it ON it.item = i.id
		    WHERE it.tag = ? AND i.state = 'live' AND i.root IS NULL AND i.id != ?
		    ORDER BY i.title COLLATE NOCASE LIMIT ?`, t.ID, it.ID, shelfItems); err == nil && len(cs) > 0 {
			it.Near, it.NearLabel = cs, "Mehr zu "+t.Name
			it.NearHref = a.viewURL([]Filter{{Facet: t.Facet, Tag: t.ID}})
		}
	}
}

// treeTag is the item's deepest tag of a machine, a magazine or a meeting,
// in that order.
func (it *ItemView) treeTag() *TagRef {
	for _, facet := range []string{"machine", "publication", "event"} {
		if t := it.deepest(facet); t != nil {
			return t
		}
	}
	return nil
}

// deepest is the item's deepest tag of the facet.
func (it *ItemView) deepest(facet string) *TagRef {
	var best *TagRef
	for i := range it.Tags {
		t := &it.Tags[i]
		if t.Facet == facet && (best == nil || strings.Count(t.Path, " › ") > strings.Count(best.Path, " › ")) {
			best = t
		}
	}
	return best
}
