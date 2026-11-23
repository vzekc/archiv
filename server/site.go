package archiv

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The public site: landing page, the old folders, search, ZIP downloads,
// item pages, tag pages. Everything is rendered from the catalog; Apache
// serves the files themselves.

func (a *App) siteRoutes(mux *http.ServeMux) {
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	page("GET /suche", a.search)
	page("GET /search.php", a.oldScript("/suche"))
	page("GET /zip", a.zipDir)
	page("GET /zip.php", a.oldScript("/zip"))
	page("GET /mirrors", redirectTo("/m/"))
	page("GET /mirrors.html", redirectTo("/m/"))
	page("GET /a/{id}", a.itemPage)
	page("GET /a/{id}/{rest...}", a.itemSub)
	page("GET /h/{sha}", a.byHash)
	page("GET /s/{id}", a.collectionPage)
	page("POST /s/{id}/bearbeiten", a.post(a.editing(a.editCollection)))
	a.viewRoutes(mux)
	a.editRoutes(mux)
	a.browserRoutes(mux)
	// everything else is a path of the old tree; registered for all methods
	// so that it does not collide with the upload handler
	page("/{path...}", a.oldPath)
}

// redirectTo answers with a permanent redirect, keeping the query.
func redirectTo(to string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u := to
		if r.URL.RawQuery != "" {
			u += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, u, http.StatusMovedPermanently)
	}
}

// oldScript sends the PHP pages' URLs to their successors, parameters and all.
func (a *App) oldScript(to string) http.HandlerFunc { return redirectTo(to) }

// SitePage is what every page template gets.
type SitePage struct {
	Title    string
	User     *User
	Crumbs   []Crumb
	Scope    string  // folder the header search starts in
	ScopeTag *TagRef // or the tag
	// or a view: its title and the ids of its tags
	ScopeView, ScopeTags string
	Areas                []SearchArea
	Assets               func(string) string
	Data                 any
	Stamp                string
	Query                string
	CSRF                 string
	Notice               string
	// the catalog can be edited; this user's changes apply directly
	Editing, CanEdit, CanCreateTags bool
}

var siteNotices = map[string]string{
	"gespeichert":     "Gespeichert.",
	"vorgeschlagen":   "Danke! Dein Vorschlag ist bei den Moderatoren.",
	"nichts":          "Es gab nichts zu ändern.",
	"kommentiert":     "Dein Beitrag ist gespeichert.",
	"entschieden":     "Entschieden.",
	"erledigt":        "Die Meldung ist abgeschlossen.",
	"zurueckgenommen": "Zurückgenommen.",
	"bild":            "Das Bild ist hochgeladen und steht am Ende des Seitentexts.",
}

type Crumb struct{ Href, Label string }

func (a *App) renderSite(w http.ResponseWriter, r *http.Request, status int, name string, p *SitePage) {
	p.User = currentUser(r)
	p.Assets = a.asset
	p.CSRF = a.csrfToken(w, r)
	p.Notice = siteNotices[r.URL.Query().Get("ok")]
	p.Editing = a.cfg.Editing && p.User != nil && !p.User.Reader
	p.CanEdit = a.mayEdit(p.User)
	p.CanCreateTags = a.mayCreateTags(p.User)
	if p.Areas == nil {
		p.Areas = a.searchAreas()
	}
	p.Stamp = fmtDate(time.Now().Unix())
	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		logf("template %s: %v", name, err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "private, no-cache")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (a *App) notFound(w http.ResponseWriter, r *http.Request, text string) {
	if text == "" {
		text = "Diese Seite gibt es nicht."
	}
	a.renderSite(w, r, http.StatusNotFound, "site-error", &SitePage{Title: "Nicht gefunden", Data: text})
}

// asset is the URL of a file in html/assets with its mtime, so that
// browsers never keep a stale copy.
func (a *App) asset(name string) string {
	return "/assets/" + name + "?v=" + strconv.FormatInt(startTime, 10)
}

// href is the URL of a path below the archive root.
func href(rel string) string {
	if rel == "" {
		return "/"
	}
	parts := strings.Split(rel, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/" + strings.Join(parts, "/")
}

// cleanRel turns a URL path into a path below the archive root, or "" and
// false if it leaves the archive.
func cleanRel(p string) (string, bool) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", true
	}
	if !validRel(p) {
		return "", false
	}
	return p, true
}

// crumbsFor is the trail from the root to a folder of the old tree.
func (a *App) crumbsFor(rel string) []Crumb {
	out := []Crumb{{Href: "/", Label: "Start"}}
	if rel == "" {
		return out
	}
	acc := ""
	for _, p := range strings.Split(rel, "/") {
		acc = joinRel(acc, p)
		out = append(out, Crumb{Href: href(acc) + "/", Label: p})
	}
	return out
}

// num formats a count the German way: 12.345.
func num(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// extOf is the lower case extension without the dot.
func extOf(name string) string {
	return strings.TrimPrefix(strings.ToLower(path.Ext(name)), ".")
}

// typeHints name file types with a word rather than their suffix.
var typeHints = map[string]string{
	"pdf": "PDF", "txt": "Text", "doc": "Text", "jpg": "Bild", "jpeg": "Bild",
	"gif": "Bild", "png": "Bild", "bmp": "Bild", "zip": "Archiv",
	"arc": "Archiv", "ark": "Archiv", "lzh": "Archiv", "lbr": "CP/M-Archiv",
	"td0": "Diskette", "imd": "Diskette", "dsk": "Diskette", "img": "Diskette",
	"com": "CP/M-Programm", "asm": "Quelltext", "mac": "Quelltext",
	"c": "Quelltext", "pas": "Quelltext", "bas": "Quelltext",
}

func typeHint(name string) string {
	e := extOf(name)
	if h, ok := typeHints[e]; ok {
		return h
	}
	return e
}

// thumbs remembers which thumbnails exist; asking the share for each file
// of a large folder on every request would be slow.
type thumbCache struct {
	mu   sync.Mutex
	seen map[string]thumbEntry
}

type thumbEntry struct {
	ok bool
	at time.Time
}

var thumbs = &thumbCache{seen: map[string]thumbEntry{}}

// thumbFor is the URL of the picture of a file in dir ("thumbs" or
// "previews"), or "". Pictures are filed by the file's path, or by its
// hash once the old site is gone (thumbs = hash).
func (a *App) thumbFor(dir, rel string) string {
	if a.cfg.ThumbsByHash {
		var sha string
		if a.cat.QueryRow("SELECT sha256 FROM file_paths WHERE path = ?", rel).Scan(&sha) != nil || len(sha) < 2 {
			return ""
		}
		rel = sha[:2] + "/" + sha
	}
	key := dir + "/" + rel
	thumbs.mu.Lock()
	e, ok := thumbs.seen[key]
	thumbs.mu.Unlock()
	if !ok || time.Since(e.at) > 10*time.Minute {
		_, err := os.Stat(filepath.Join(a.cfg.ArchiveRoot, dir, filepath.FromSlash(rel)+".jpg"))
		e = thumbEntry{ok: err == nil, at: time.Now()}
		thumbs.mu.Lock()
		if len(thumbs.seen) > 200000 {
			thumbs.seen = map[string]thumbEntry{}
		}
		thumbs.seen[key] = e
		thumbs.mu.Unlock()
	}
	if !e.ok {
		return ""
	}
	return "/" + dir + href(rel) + ".jpg"
}

// previewFor is the URL of the large first page of a PDF; the browser
// asks for it only on hover, so it is not checked.
func (a *App) previewFor(rel string) string {
	if a.cfg.ThumbsByHash {
		var sha string
		if a.cat.QueryRow("SELECT sha256 FROM file_paths WHERE path = ?", rel).Scan(&sha) == nil && len(sha) > 2 {
			return "/previews/" + sha[:2] + "/" + sha + ".jpg"
		}
	}
	return "/previews" + href(rel) + ".jpg"
}

// SearchArea is one entry of the scope menu of the search box.
type SearchArea struct{ Path, Label string }

var folderNames = map[string]string{
	"OS": "Betriebssysteme", "rechner": "Rechner", "manuals": "Handbücher", "cdrom": "CD-ROMs",
	"archiv": "Archiv", "mirror": "Mirrors", "hifi": "HiFi", "program": "Programme", "roms": "ROMs",
	"saved_mirrors": "Gesicherte Mirrors", "hardware_of_oldcomputers": "Hardware",
}

// searchAreas are the top folders of the old tree: those below public/pub
// and public, and those beside public.
func (a *App) searchAreas() []SearchArea {
	var out []SearchArea
	rows, err := a.cat.Query(`SELECT path FROM tags WHERE facet = 'location' AND merged_into IS NULL AND (
	    path LIKE 'public/pub/%' AND path NOT LIKE 'public/pub/%/%'
	    OR path LIKE 'public/%' AND path NOT LIKE 'public/%/%' AND path != 'public/pub'
	    OR path NOT LIKE '%/%' AND path != 'public')`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		rows.Scan(&p)
		name := path.Base(p)
		label, ok := folderNames[name]
		if !ok {
			label = strings.TrimSpace(strings.ReplaceAll(name, "_", " "))
		}
		out = append(out, SearchArea{Path: p, Label: label})
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Label) < strings.ToLower(out[j].Label) })
	return out
}

// scopeChain is the folder and those above it up to its area, narrowest
// first, for the scope menu.
func scopeChain(p string, areas []SearchArea) []SearchArea {
	isArea := map[string]string{}
	for _, ar := range areas {
		isArea[ar.Path] = ar.Label
	}
	var out []SearchArea
	for p != "" && p != "public" && p != "public/pub" {
		label, ok := isArea[p]
		if !ok {
			label = strings.TrimSpace(strings.ReplaceAll(path.Base(p), "_", " "))
		}
		out = append(out, SearchArea{Path: p, Label: label})
		if ok {
			break
		}
		p = parentDir(p)
	}
	return out
}
