package archiv

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// Reading a PDF at its hits: /a/<id>/lesen?pfad=<file>&seite=<n>&q=<term>
// shows the document page by page in the browser (PDF.js), marks the term
// where the PDF has a text layer, and steps from hit page to hit page. The
// passage our page store has for a page is shown below it, so the hits in
// scans, whose text lives only in the page store, are found as well.
//
// An item page or panel opened from a search lists the same hits as finds.

const maxFinds = 60

// Find is a page of a document of an item with a hit.
type Find struct {
	Path    string
	Name    string // the file, when the item has several
	Page    int
	Snippet template.HTML
	Href    string // the reader at this page
}

// readerURL is the reader for a file of an item at a page.
func readerURL(item int64, filePath string, page int, q string) string {
	v := url.Values{}
	v.Set("pfad", filePath)
	if page > 0 {
		v.Set("seite", strconv.Itoa(page))
	}
	if q != "" {
		v.Set("q", q)
	}
	return "/a/" + itoa(item) + "/lesen?" + strings.ReplaceAll(v.Encode(), "%2F", "/")
}

// finds are the pages of the item's PDFs that the query matches, in the
// order of the files and pages, at most limit; more tells how many follow.
func (a *App) finds(it *ItemView, q string, limit int) (out []Find, more int) {
	match := ftsQuery(q)
	pdb := a.pages()
	if match == "" || pdb == nil {
		return nil, 0
	}
	var paths []string
	for _, f := range it.Files {
		if strings.EqualFold(extOf(f.Name), "pdf") {
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		return nil, 0
	}
	args := []any{markOpen, markClose, match}
	for _, p := range paths {
		args = append(args, p)
	}
	rows, err := pdb.Query(`SELECT pg.path, pg.page, snippet(pages_fts, 0, ?, ?, '…', 20) FROM pages_fts JOIN pages pg ON pg.id = pages_fts.rowid
	    WHERE pages_fts MATCH ? AND pg.path IN (`+inList(len(paths))+`)`, args...)
	if err != nil {
		logf("finds %d %q: %v", it.ID, q, err)
		return nil, 0
	}
	defer rows.Close()
	order := map[string]int{}
	for i, p := range paths {
		order[p] = i
	}
	var all []Find
	for rows.Next() {
		var f Find
		var s string
		rows.Scan(&f.Path, &f.Page, &s)
		f.Snippet = snippetHTML(s)
		f.Href = readerURL(it.ID, f.Path, f.Page, q)
		if len(paths) > 1 {
			f.Name = baseName(f.Path)
		}
		all = append(all, f)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Path != all[j].Path {
			return order[all[i].Path] < order[all[j].Path]
		}
		return all[i].Page < all[j].Page
	})
	if len(all) > limit {
		return all[:limit], len(all) - limit
	}
	return all, 0
}

func baseName(p string) string { return p[strings.LastIndex(p, "/")+1:] }

type ReaderView struct {
	Item *ItemView
	File *ItemFileView
	Q    string
	Page int
	Data string // for the script, as JSON: file, page, hit pages, terms
}

// reader shows a PDF of the item in the reader.
func (a *App) reader(w http.ResponseWriter, r *http.Request, it *ItemView) {
	qv := r.URL.Query()
	var f *ItemFileView
	for i := range it.Files {
		pdf := strings.EqualFold(extOf(it.Files[i].Name), "pdf")
		if pdf && (f == nil || it.Files[i].Path == qv.Get("pfad")) {
			f = &it.Files[i]
		}
		if f != nil && f.Path == qv.Get("pfad") {
			break
		}
	}
	if f == nil || !f.Present {
		a.notFound(w, r, "Dieses Objekt hat kein PDF zum Lesen.")
		return
	}
	rv := &ReaderView{Item: it, File: f, Q: strings.TrimSpace(qv.Get("q"))}
	rv.Page, _ = strconv.Atoi(qv.Get("seite"))
	all, _ := a.finds(it, rv.Q, 100000)
	hits := []int{}
	snippets := map[string]template.HTML{}
	for _, x := range all {
		if x.Path == f.Path {
			hits = append(hits, x.Page)
			snippets[strconv.Itoa(x.Page)] = x.Snippet
		}
	}
	if rv.Page < 1 {
		rv.Page = 1
		if len(hits) > 0 {
			rv.Page = hits[0]
		}
	}
	var terms []string
	for _, m := range ftsTerm.FindAllStringSubmatch(rv.Q, -1) {
		t := m[1]
		if t == "" {
			t = m[2]
		}
		if t = strings.TrimSpace(ftsSpace.ReplaceAllString(ftsClean.ReplaceAllString(t, " "), " ")); t != "" {
			terms = append(terms, t)
		}
	}
	data, _ := json.Marshal(map[string]any{
		"file": f.Href, "name": f.Name, "page": rv.Page, "hits": hits, "snippets": snippets, "terms": terms,
	})
	rv.Data = string(data)
	a.renderSite(w, r, http.StatusOK, "v-reader", &SitePage{Title: it.Title, Query: rv.Q, Data: rv})
}
