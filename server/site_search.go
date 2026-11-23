package archiv

import (
	"database/sql"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
)

// Search over the catalog (titles, descriptions, tags, file names) and the
// text of the documents (pages.sqlite, page by page). The hits are a set of
// items like a view, narrowed down by the same rows and chips:
//
//	/suche?q=axp&rechner=dec&art=hersteller-dokumentation
//
// A large set is shown as shelves, one per next step of the most telling
// facet. A small one, or the list asked for, shows first the items that
// are about the term (it is in their title, tags or file names), then those
// whose text mentions it, the most pages first; issues of one magazine and
// copies of one document form a group. Text hits link to their pages.
// Tags whose name matches lead to their views.

const (
	docHits   = 5000  // best item hits considered
	pageHits  = 50000 // best page hits considered
	morePages = 12    // pages listed per document
	groupsPer = 25    // groups of text hits per page
	groupHits = 8     // hits shown per group
	markOpen  = "\x02"
	markClose = "\x03"
)

type SearchView struct {
	*View
	Q       string
	Tiles   []SearchTile
	Leads   []Shelf // the items of the tags the query names, first
	NoText  bool    // only titles, tags and file names, not the text of the documents
	TextURL string  // the same search with or without the text
	List    bool    // about and text sections, not shelves
	ListURL string
	About   []Card
	AboutN  int
	Groups  []HitGroup
	TextN   int
	Pages   int
	PrevURL string
	NextURL string
	Capped  bool
	Error   string
	Files   int64
	Bytes   int64
	Stamp   int64
}

// SearchTile is a tag whose name matches: a way into its view.
type SearchTile struct {
	Facet, Label, Path, Href, Thumb string
	Count                           int
	id                              int64
	named                           bool // all words of the query are in its path
}

// HitGroup is one text hit, or several of the same magazine or document.
type HitGroup struct {
	Label string
	Href  string // the search within the magazine
	Hits  []TextHit
	More  int // hits not shown
	Pages int // pages with hits, all of the group
}

type TextHit struct {
	Card
	Snippet  template.HTML
	Pages    []PageLink
	MoreLeft int
	NPages   int
}

type PageLink struct {
	N    int
	Href string
}

type textHit struct {
	path    string
	pageIDs []int64
	pages   []int
	rank    float64
	dsnip   string
}

type TagRef struct {
	ID          int64
	Facet, Name string
	Href        string
	Path        string // "DEC › VAX"
}

var (
	ftsTerm  = regexp.MustCompile(`"([^"]+)"|(\S+)`)
	ftsClean = regexp.MustCompile(`[^\p{L}\p{N}_.\- ]+`)
	ftsSpace = regexp.MustCompile(`\s+`)
)

// ftsQuery turns free text into a safe FTS5 expression: every term quoted,
// the last one also as a prefix, all joined by AND.
func ftsQuery(q string) string {
	var terms []string
	for _, m := range ftsTerm.FindAllStringSubmatch(q, -1) {
		raw := m[1]
		if raw == "" {
			raw = m[2]
		}
		c := strings.TrimSpace(ftsSpace.ReplaceAllString(ftsClean.ReplaceAllString(raw, " "), " "))
		if c != "" {
			terms = append(terms, c)
		}
	}
	for i, t := range terms {
		quoted := `"` + strings.ReplaceAll(t, `"`, "") + `"`
		if i == len(terms)-1 && !strings.Contains(t, " ") {
			quoted += "*"
		}
		terms[i] = quoted
	}
	return strings.Join(terms, " AND ")
}

// snippetHTML escapes a snippet and turns the markers into <mark>.
func snippetHTML(s string) template.HTML {
	e := template.HTMLEscapeString(s)
	e = strings.ReplaceAll(e, markOpen, "<mark>")
	e = strings.ReplaceAll(e, markClose, "</mark>")
	return template.HTML(e)
}

type pagesStore struct {
	mu sync.Mutex
	db *sql.DB
}

// pages opens the page store read-only; nil while there is none.
func (a *App) pages() *sql.DB {
	a.pagesDB.mu.Lock()
	defer a.pagesDB.mu.Unlock()
	if a.pagesDB.db == nil {
		if _, err := os.Stat(a.cfg.PagesDB); err != nil {
			return nil
		}
		db, err := sql.Open("sqlite", "file:"+a.cfg.PagesDB+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil || db.Ping() != nil {
			return nil
		}
		a.pagesDB.db = db
	}
	return a.pagesDB.db
}

// searchURL is the search for q with these filters.
// url is the URL of this search with other filters, keeping whether it
// reads the text.
func (sv *SearchView) url(a *App, filters []Filter, extra ...string) string {
	if sv.NoText {
		extra = append(extra, "volltext", "nein")
	}
	return a.searchURL(sv.Q, filters, extra...)
}

func (a *App) searchURL(q string, filters []Filter, extra ...string) string {
	v := url.Values{}
	v.Set("q", q)
	for _, f := range filters {
		if f.Tag == 0 {
			v.Set(facetParam[f.Facet], "alle")
		} else {
			v.Set(facetParam[f.Facet], a.slugPathOf(f.Tag))
		}
	}
	for i := 0; i+1 < len(extra); i += 2 {
		v.Set(extra[i], extra[i+1])
	}
	return "/suche?" + strings.ReplaceAll(v.Encode(), "%2F", "/")
}

// searchFilters reads the filters of a search: the facet parameters, and
// the search box's in= (tags:<id>,<facet> from a view, or a folder).
func (a *App) searchFilters(qv url.Values) (filters []Filter, canonical bool) {
	add := func(f Filter) { filters = withFilter(filters, f) }
	for _, facet := range viewFacets {
		v := strings.Trim(qv.Get(facetParam[facet]), "/")
		if v == "alle" {
			filters = append(filters, Filter{Facet: facet})
		} else if v != "" {
			if f, ok := a.filterFor(facet, v); ok {
				add(f)
			}
		}
	}
	in := strings.Trim(qv.Get("in"), "/ ")
	if in == "" {
		return filters, true
	}
	if t, ok := strings.CutPrefix(in, "tags:"); ok {
		for _, s := range strings.Split(t, ",") {
			if id, err := strconv.ParseInt(s, 10, 64); err == nil {
				if f, ok := a.filterOf(id); ok {
					add(f)
				}
			} else if isFacet(s) {
				filters = append(withFilter(filters, Filter{Facet: s}), Filter{Facet: s})
			}
		}
		return filters, false
	}
	// a folder: the nearest one the catalog knows
	for p := in; p != ""; p = parentDir(p) {
		var id int64
		if a.cat.QueryRow("SELECT id FROM tags WHERE facet = 'location' AND path = ?", p).Scan(&id) == nil {
			if f, ok := a.filterOf(id); ok {
				add(f)
			}
			break
		}
	}
	return filters, false
}

func (a *App) search(w http.ResponseWriter, r *http.Request) {
	qv := r.URL.Query()
	q := strings.TrimSpace(qv.Get("q"))
	filters, canonical := a.searchFilters(qv)
	if !canonical && q != "" {
		extra := []string{}
		if qv.Get("volltext") == "nein" {
			extra = []string{"volltext", "nein"}
		}
		http.Redirect(w, r, a.searchURL(q, filters, extra...), http.StatusFound)
		return
	}
	sv := &SearchView{Q: q, View: &View{Filters: filters}, NoText: qv.Get("volltext") == "nein"}
	page := &SitePage{Title: "Suche", Query: q, Data: sv}
	if q == "" {
		a.cat.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM file_paths WHERE seen >= ?", a.lastSync()).Scan(&sv.Files, &sv.Bytes)
		sv.Stamp = a.lastSync()
		a.renderSite(w, r, http.StatusOK, "site-search", page)
		return
	}
	page.Title = "Suche: " + q
	pageNo, _ := strconv.Atoi(qv.Get("seite"))
	if err := a.runSearch(sv, max(pageNo, 1), qv.Get("ansicht") == "liste"); err != nil {
		logf("search %q: %v", q, err)
		sv.Error = "Die Suchanfrage konnte nicht ausgewertet werden."
	}
	var ids []string
	for _, f := range filters {
		if f.Tag != 0 {
			ids = append(ids, strconv.FormatInt(f.Tag, 10))
		} else {
			ids = append(ids, f.Facet)
		}
	}
	if len(ids) > 0 {
		page.ScopeView, page.ScopeTags = sv.View.Title, strings.Join(ids, ",")
	}
	a.renderSite(w, r, http.StatusOK, "site-search", page)
}

func (a *App) runSearch(sv *SearchView, pageNo int, asList bool) error {
	match := ftsQuery(sv.Q)
	if match == "" {
		return nil
	}
	idx := a.index()
	link := func(f []Filter) string { return sv.url(a, f) }
	if sv.NoText {
		sv.TextURL = a.searchURL(sv.Q, sv.Filters)
	} else {
		sv.TextURL = a.searchURL(sv.Q, sv.Filters, "volltext", "nein")
	}

	// about the term: it is in the title, the tags or the file names
	var about []int64
	isAbout := map[int64]bool{}
	rows, err := a.cat.Query(`SELECT rowid FROM items_fts WHERE items_fts MATCH ? ORDER BY bm25(items_fts, 6.0, 6.0, 1.5, 12.0) LIMIT `+strconv.Itoa(docHits),
		"{title tags names} : ("+match+")")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		about = append(about, id)
		isAbout[id] = true
	}
	rows.Close()

	// mentioned: in the description, or in the text of the documents
	text := map[int64]*textHit{}
	if sv.NoText {
		goto mentionsDone
	}
	rows, err = a.cat.Query(`SELECT rowid, snippet(items_fts, 1, ?, ?, '…', 16) FROM items_fts WHERE items_fts MATCH ? LIMIT `+strconv.Itoa(docHits),
		markOpen, markClose, "{descr} : ("+match+")")
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var sn string
		rows.Scan(&id, &sn)
		text[id] = &textHit{dsnip: sn, rank: 0}
	}
	rows.Close()
	if pdb := a.pages(); pdb != nil {
		rows, err := pdb.Query(`SELECT pg.id, pg.path, pg.page, pages_fts.rank FROM pages_fts JOIN pages pg ON pg.id = pages_fts.rowid
		    WHERE pages_fts MATCH ? ORDER BY rank LIMIT `+strconv.Itoa(pageHits), match)
		if err != nil {
			return err
		}
		type pageHit struct {
			id   int64
			path string
			page int
			rank float64
		}
		var phs []pageHit
		for rows.Next() {
			var p pageHit
			rows.Scan(&p.id, &p.path, &p.page, &p.rank)
			phs = append(phs, p)
		}
		rows.Close()
		sv.Capped = len(phs) >= pageHits
		items := a.itemsOfPaths(uniquePaths(phs, func(p pageHit) string { return p.path }))
		for _, p := range phs {
			id, ok := items[p.path]
			if !ok {
				continue
			}
			h := text[id]
			if h == nil {
				h = &textHit{rank: p.rank}
				text[id] = h
			}
			if h.path == "" {
				h.path, h.rank = p.path, p.rank
			}
			if h.path == p.path {
				h.pageIDs = append(h.pageIDs, p.id)
				h.pages = append(h.pages, p.page)
			}
		}
	}
	// the most pages first; among equals the best match
mentionsDone:
	var mentioned []int64
	for id := range text {
		if !isAbout[id] {
			mentioned = append(mentioned, id)
		}
	}
	// the items whose tags share words with the query first (the PDP-11
	// manuals for "PDP-11/34"), then the most pages, then the best match
	qw := words(sv.Q)
	tagged := map[int64]int{}
	for _, id := range mentioned {
		if i, ok := idx.byID[id]; ok {
			tagged[id] = tagWords(idx, idx.items[i].Tags, qw)
		}
	}
	sort.Slice(mentioned, func(i, j int) bool {
		if x, y := tagged[mentioned[i]], tagged[mentioned[j]]; x != y {
			return x > y
		}
		a, b := text[mentioned[i]], text[mentioned[j]]
		if len(a.pages) != len(b.pages) {
			return len(a.pages) > len(b.pages)
		}
		if a.rank != b.rank {
			return a.rank < b.rank
		}
		return mentioned[i] < mentioned[j]
	})

	toSet := func(ids []int64) itemSet {
		var out itemSet
		for _, id := range ids {
			if i, ok := idx.byID[id]; ok {
				out = append(out, i)
			}
		}
		return idx.restrict(out, sv.Filters)
	}
	aboutSet, textSet := toSet(about), toSet(mentioned)
	all := append(append(itemSet{}, aboutSet...), textSet...)

	v := sv.View
	v.Total = len(all)
	v.Empty = v.Total == 0
	v.Title, v.Chips = a.chips(sv.Filters, link, sv.url(a, nil))
	v.Rows = a.refineRows(idx, all, sv.Filters, link)
	if pageNo == 1 {
		sv.Tiles = a.searchTiles(idx, sv.Q)
		// the query names tags: their items lead, before the hits
		for _, t := range sv.Tiles {
			if !t.named || len(sv.Leads) >= maxLeads {
				continue
			}
			if set := idx.restrict(idx.below(t.id), sv.Filters); len(set) > 0 {
				sv.Leads = append(sv.Leads, Shelf{Label: t.Label, Href: t.Href, Count: len(set), Cards: a.memCards(idx, idx.ranked(set, shelfItems))})
			}
		}
	}

	sv.List = asList || v.Total <= gridPage
	if !sv.List {
		// what the query is about leads; the mentions are shelved
		v.ByRank = true
		// what the tags' shelves above hold already is not repeated
		var shown []int64
		for _, t := range sv.Tiles {
			if t.named {
				shown = append(shown, t.id)
			}
		}
		var lead itemSet
		if len(shown) > 0 {
			lead, _ = idx.without(aboutSet, shown, 0)
		} else {
			lead = aboutSet
		}
		if len(lead) > 0 {
			sv.Leads = append(sv.Leads, Shelf{Label: "Im Titel, in Schlagworten oder Dateinamen", Href: sv.url(a, sv.Filters, "ansicht", "liste"),
				Count: len(lead), Cards: a.memCards(idx, lead[:min(len(lead), shelfItems)])})
		}
		shelved := all
		if len(aboutSet) > 0 {
			shelved = textSet
		}
		a.shelves(idx, v, shelved)
		sv.List = len(v.Shelves) == 0 && len(sv.Leads) == 0
	}
	sv.ListURL = sv.url(a, sv.Filters, "ansicht", "liste")
	if !sv.List {
		return nil
	}

	sv.AboutN, sv.TextN = len(aboutSet), len(textSet)
	if pageNo == 1 {
		sv.About = a.memCards(idx, aboutSet[:min(len(aboutSet), gridPage)])
	}
	groups := groupTextHits(idx, textSet)
	sv.Pages = (len(groups) + groupsPer - 1) / groupsPer
	from := min((pageNo-1)*groupsPer, len(groups))
	to := min(from+groupsPer, len(groups))
	pdb := a.pages()
	for _, g := range groups[from:to] {
		hg := HitGroup{Label: g.label}
		if g.tag != 0 && len(g.items) > 1 {
			hg.Href = link(withFilter(sv.Filters, Filter{Facet: "publication", Tag: g.tag}))
		}
		for k, i := range g.items {
			h := text[idx.items[i].ID]
			hg.Pages += len(h.pages)
			if k >= groupHits {
				hg.More++
				continue
			}
			hg.Hits = append(hg.Hits, a.textHit(idx, i, h, pdb, match, sv.Q))
		}
		sv.Groups = append(sv.Groups, hg)
	}
	if pageNo > 1 {
		sv.PrevURL = sv.url(a, sv.Filters, "ansicht", "liste", "seite", strconv.Itoa(pageNo-1))
	}
	if to < len(groups) {
		sv.NextURL = sv.url(a, sv.Filters, "ansicht", "liste", "seite", strconv.Itoa(pageNo+1))
	}
	return nil
}

// textHit is what a text hit shows: the item, the best passage, the pages.
func (a *App) textHit(idx *memIndex, i int32, h *textHit, pdb *sql.DB, match, q string) TextHit {
	th := TextHit{Card: a.memCard(idx, i), NPages: len(h.pages)}
	if len(h.pageIDs) > 0 && pdb != nil {
		var s string
		pdb.QueryRow("SELECT snippet(pages_fts, 0, ?, ?, '…', 24) FROM pages_fts WHERE pages_fts MATCH ? AND rowid = ?",
			markOpen, markClose, match, h.pageIDs[0]).Scan(&s)
		th.Snippet = snippetHTML(s)
		pages := append([]int(nil), h.pages...)
		sort.Ints(pages)
		if len(pages) > morePages {
			th.MoreLeft = len(pages) - morePages
			pages = pages[:morePages]
		}
		id := idx.items[i].ID
		for _, n := range pages {
			th.Pages = append(th.Pages, PageLink{N: n, Href: readerURL(id, h.path, n, q)})
		}
		if len(th.Pages) > 0 {
			th.File = readerURL(id, h.path, h.pages[0], q)
		}
	} else if h.dsnip != "" {
		th.Snippet = snippetHTML(h.dsnip)
	}
	return th
}

type hitGroup struct {
	label string
	tag   int64 // the magazine
	items itemSet
}

// groupTextHits puts the issues of one magazine together, and the copies of
// one document (the same title); a group stands where its best hit is.
func groupTextHits(idx *memIndex, set itemSet) []*hitGroup {
	var out []*hitGroup
	byKey := map[string]*hitGroup{}
	for _, i := range set {
		it := &idx.items[i]
		var key, label string
		var tag int64
		for _, t := range it.Tags {
			if mt := idx.tags[t]; mt != nil && mt.Facet == "publication" {
				if tag == 0 || len(mt.Anc) > len(idx.tags[tag].Anc) {
					tag = t
				}
			}
		}
		if tag != 0 {
			key, label = "p"+itoa(tag), idx.tags[tag].Name
		} else {
			key, label = "t"+titleKey(it.Title), it.Title
		}
		g := byKey[key]
		if g == nil {
			g = &hitGroup{label: label, tag: tag}
			byKey[key] = g
			out = append(out, g)
		}
		g.items = append(g.items, i)
	}
	return out
}

// titleKey compares titles by letters and digits only.
func titleKey(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// maxLeads bounds the shelves of tags a search shows before its hits.
const maxLeads = 3

// leadRank orders the tags a query names: the machine it is about before
// the magazine about it.
var leadRank = map[string]int{"machine": 0, "event": 1, "publication": 2, "system": 3}

// searchTiles are the tags the query names: those whose name contains it,
// and those whose path holds all its words ("dec professional": DEC ›
// PDP-11 › Professional 325/350/380). The latter first, machines before
// magazines, then the largest.
func (a *App) searchTiles(idx *memIndex, q string) []SearchTile {
	qw := words(q)
	if len(qw) == 0 {
		return nil
	}
	lq := strings.ToLower(strings.TrimSpace(q))
	var out []SearchTile
	for id, t := range idx.tags {
		if t.Facet == "location" || t.Facet == "group" {
			continue
		}
		var names []string
		for _, a := range t.Anc {
			names = append(names, idx.tags[a].Name)
		}
		named := hasWords(words(strings.Join(names, " ")), qw)
		if !named && !strings.Contains(strings.ToLower(t.Name), lq) {
			continue
		}
		set := idx.below(id)
		if len(set) == 0 {
			continue
		}
		f, ok := a.filterOf(id)
		if !ok {
			continue
		}
		st := SearchTile{Facet: f.Facet, Label: f.Ref.Name, Href: a.viewURL([]Filter{f}), Count: len(set), id: id, named: named}
		if f.Ref.Path != f.Ref.Name {
			st.Path = f.Ref.Path
		}
		out = append(out, st)
	}
	// a tag named by its parent alone (DEC for "dec professional") is not
	// what the query names, if one below it is
	sort.SliceStable(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if x.named != y.named {
			return x.named
		}
		rx, okx := leadRank[x.Facet]
		ry, oky := leadRank[y.Facet]
		if !okx {
			rx = 9
		}
		if !oky {
			ry = 9
		}
		if rx != ry {
			return rx < ry
		}
		if x.Count != y.Count {
			return x.Count > y.Count
		}
		return x.Label < y.Label
	})
	out = out[:min(len(out), 6)]
	for i := range out {
		for _, k := range idx.below(out[i].id)[:min(len(idx.below(out[i].id)), 40)] {
			if c := a.memCard(idx, k); c.Thumb != "" {
				out[i].Thumb = c.Thumb
				break
			}
		}
	}
	return out
}

// tagWords counts the query words that are words of the tags' paths.
func tagWords(idx *memIndex, tags []int64, q []string) int {
	var names []string
	for _, id := range tags {
		t := idx.tags[id]
		if t == nil || t.Facet == "location" || t.Facet == "kind" || t.Facet == "source" {
			continue
		}
		for _, a := range t.Anc {
			names = append(names, idx.tags[a].Name)
		}
	}
	have := map[string]bool{}
	for _, w := range words(strings.Join(names, " ")) {
		have[w] = true
	}
	n := 0
	for _, w := range q {
		if have[w] {
			n++
		}
	}
	return n
}

// words are the words of a name, lower case, without punctuation.
func words(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// hasWords tells whether every query word is a word of the name, or the
// beginning of one when it is the last ("pro" in "professional").
func hasWords(name, q []string) bool {
	for i, w := range q {
		found := false
		for _, n := range name {
			if n == w || i == len(q)-1 && len(w) >= 3 && strings.HasPrefix(n, w) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func uniquePaths[T any](xs []T, key func(T) string) []string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if k := key(x); !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// itemsOfPaths maps file paths to their live items.
func (a *App) itemsOfPaths(paths []string) map[string]int64 {
	out := map[string]int64{}
	for len(paths) > 0 {
		chunk := paths[:min(500, len(paths))]
		paths = paths[len(chunk):]
		q := "SELECT f.path, f.item FROM item_files f JOIN items i ON i.id = f.item WHERE i.state = 'live' AND f.path IN (" +
			strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + ")"
		args := make([]any, 0, len(chunk)+1)
		for _, p := range chunk {
			args = append(args, p)
		}
		rows, err := a.cat.Query(q, args...)
		if err != nil {
			continue
		}
		for rows.Next() {
			var p string
			var id int64
			rows.Scan(&p, &id)
			out[p] = id
		}
		rows.Close()
	}
	return out
}
