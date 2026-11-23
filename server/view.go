package archiv

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/url"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
)

// The view: one kind of page for browsing the archive. A view is a set of
// items given by filters, at most one tag per facet; a tag stands for
// everything below it. The page shows the filters as chips, a row per
// facet to narrow the view down further, and the items: as shelves, one
// per next step of the most telling facet, when there are many, else as
// one grid.
//
//	/r/dec/vax?art=hersteller-dokumentation&sprache=de
//	/z/commodore/64er        /treffen/cc-2019        /v?art=cd-rom-sammlung
//
// The path carries the first filter of machine, publication or event; all
// others are parameters named after their facet.

var facetParam = map[string]string{
	"machine": "rechner", "kind": "art", "publication": "zeitschrift", "event": "treffen",
	"system": "system", "language": "sprache", "source": "quelle", "location": "ordner", "period": "zeit",
	"group": "gruppe", "keyword": "stichwort",
}

// tagNone as a filter's tag selects the items without a tag of the facet:
// "?gruppe=-".
const tagNone = -2

var paramFacet = func() map[string]string {
	m := map[string]string{}
	for f, p := range facetParam {
		m[p] = f
	}
	return m
}()

// the facets a view can be filtered by, in the order of the chips; the old
// folder tree filters what an old link names
var viewFacets = []string{"machine", "publication", "event", "group", "kind", "keyword", "system", "language", "source", "location"}

// the rows to narrow down, in their order, and the facets that may form
// shelves; the groups are the folders of the old tree below a machine,
// magazine or meeting
var rowFacets = []string{"machine", "publication", "event", "group", "kind", "keyword", "system", "language", "source"}
var shelfFacets = []string{"machine", "publication", "event", "group", "kind"}

var pathFacets = []struct{ prefix, facet string }{{"r", "machine"}, {"z", "publication"}, {"treffen", "event"}}

const (
	gridPage   = 60 // items per page of a grid
	shelfItems = 14 // items on a shelf
	maxShelves = 24
	smallSet   = 24 // a set this small shows only the rows that are a choice
	tinySet    = 6  // a set this small is one grid
)

type Filter struct {
	Facet string
	Tag   int64 // 0: anything in the facet (the root of /r/, /z/ ...)
	Ref   *TagRef
	Path  []TagRef // the tag and those above it, from the top
}

type View struct {
	Tag     *TagView // the view of a single tag: its page text, editing, comments
	Filters []Filter
	Title   string
	Total   int
	Chips   []Chip
	Rows    []RefineRow
	Shelves []Shelf
	Grid    []Card
	Page    int
	More    string // URL of the next page of the grid
	GridURL string // the whole set as a grid
	ZipURL  string // the whole set as one ZIP file
	ZipSize int64
	AsGrid  bool
	GridOK  bool   // small enough to page through as one grid
	Scope   string // for the search box: tag ids, comma separated
	ByRank  bool   // shelves in the order of their best item: the hits of a search, best first
	Empty   bool
}

type Chip struct {
	Facet  string
	Label  string
	Crumbs []Crumb // each level, linking to the view with that level
	Remove string
	Edit   string // the view of the tag alone, at its edit form
}

type RefineRow struct {
	Facet   string
	Label   string
	Options []RefineOption
	More    int // options folded away behind a button
}

type RefineOption struct {
	Label string
	Href  string
	Count int
	Tag   int64
	Extra bool // shown only when the row is unfolded
}

// rowShown is how many options of a row show at first: the ones with the
// most items, in the row's order, so a long row stays a few lines high.
const rowShown = 12

type Shelf struct {
	Label string
	Href  string
	Count int
	Cards []Card
}

// Card is an item as the views show it.
type Card struct {
	ID    int64
	Title string
	Sub   string
	Href  string // the item
	File  string // its only file, straight
	Thumb string
	Kind  string // image, pdf, other
	Type  string // badge text for items without a picture
	Files int
	Width bool // a landscape picture: wider tile
}

func (a *App) viewRoutes(mux *http.ServeMux) {
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	page("GET /{$}", a.startPage)
	page("GET /v", a.viewHandler(""))
	for _, pf := range pathFacets {
		page("GET /"+pf.prefix+"/{rest...}", a.viewHandler(pf.facet))
		page("GET /"+pf.prefix, redirectTo("/"+pf.prefix+"/"))
	}
	page("GET /t/{rest...}", a.viewHandler("*"))
	page("GET /cd/", redirectTo("/v?art=cd-rom-sammlung"))
	page("GET /m/", redirectTo("/v?art=mirror"))
	page("GET /b/", redirectTo("/v?art=buch"))
}

// viewHandler parses the filters of a request: the path for facet (or
// /t/<facet>/... when facet is "*"), then the parameters.
func (a *App) viewHandler(facet string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var filters []Filter
		rest := strings.Trim(r.PathValue("rest"), "/")
		if facet == "*" {
			f, p, _ := strings.Cut(rest, "/")
			if !isFacet(f) {
				a.notFound(w, r, "")
				return
			}
			facet, rest = f, p
		}
		if facet != "" {
			ft, ok := a.filterFor(facet, rest)
			if !ok {
				a.tagMoved(w, r, facet, strings.Split(rest, "/"))
				return
			}
			filters = append(filters, ft)
		}
		q := r.URL.Query()
		for _, f := range viewFacets {
			v := q.Get(facetParam[f])
			if v == "" || f == facet {
				continue
			}
			ft, ok := a.filterFor(f, strings.Trim(v, "/"))
			if !ok || ft.Tag == 0 {
				continue
			}
			filters = append(filters, ft)
		}
		page, _ := strconv.Atoi(q.Get("seite"))
		if q.Get("als") == "zip" {
			title, _ := a.chips(filters, func([]Filter) string { return "" }, "/")
			a.viewZip(w, r, filters, title)
			return
		}
		v, err := a.buildView(filters, max(page, 1), q.Get("ansicht") == "raster")
		if err != nil {
			logf("view: %v", err)
			http.Error(w, "interner Fehler", http.StatusInternalServerError)
			return
		}
		if q.Get("teil") == "raster" {
			// the next page of the grid, for loading more in place
			a.renderSite(w, r, http.StatusOK, "v-gridpage", &SitePage{Data: v})
			return
		}
		var ids []string
		for _, f := range filters {
			if f.Tag == tagNone {
				continue
			}
			if f.Tag != 0 {
				ids = append(ids, strconv.FormatInt(f.Tag, 10))
			} else {
				ids = append(ids, f.Facet)
			}
		}
		v.Scope = strings.Join(ids, ",")
		if v.ZipSize = a.viewZipSize(filters, v.Total); v.ZipSize > 0 {
			v.ZipURL = a.viewURL(filters, "als", "zip")
		}
		if len(filters) == 1 && filters[0].Tag > 0 {
			v.Tag = a.tagInfo(filters[0].Ref)
		}
		p := &SitePage{Title: v.Title, Data: v}
		if v.Scope != "" {
			p.ScopeView, p.ScopeTags = v.Title, v.Scope
		}
		a.renderSite(w, r, http.StatusOK, "v-view", p)
	}
}

// filterFor resolves "dec/vax" in a facet; "" is the facet's root.
func (a *App) filterFor(facet, slugPath string) (Filter, bool) {
	ft := Filter{Facet: facet}
	if slugPath == "" {
		return ft, true
	}
	if slugPath == "-" {
		ft.Tag = tagNone
		return ft, true
	}
	id, ok := a.tagByPath(facet, strings.Split(slugPath, "/"))
	if !ok {
		return ft, false
	}
	ft.Tag = id
	ft.Ref = a.tagRef(id)
	for _, an := range a.ancestors(ft.Ref) {
		ft.Path = append([]TagRef{an}, ft.Path...)
	}
	ft.Path = append(ft.Path, *ft.Ref)
	return ft, true
}

// filterOf is the filter for a tag.
func (a *App) filterOf(id int64) (Filter, bool) {
	ref := a.tagRef(id)
	if ref == nil {
		return Filter{}, false
	}
	f := Filter{Facet: ref.Facet, Tag: id, Ref: ref}
	for _, an := range a.ancestors(ref) {
		f.Path = append([]TagRef{an}, f.Path...)
	}
	f.Path = append(f.Path, *ref)
	return f, true
}

// slugPathOf is the URL form of a tag: "dec/vax".
func (a *App) slugPathOf(id int64) string {
	var slugs []string
	for p := id; p != 0 && len(slugs) < 12; {
		var slug string
		var parent sql.NullInt64
		if a.cat.QueryRow("SELECT slug, parent FROM tags WHERE id = ?", p).Scan(&slug, &parent) != nil {
			break
		}
		slugs = append([]string{slug}, slugs...)
		p = parent.Int64
	}
	return strings.Join(slugs, "/")
}

// viewURL is the URL of the view with these filters.
func (a *App) viewURL(filters []Filter, extra ...string) string {
	var pathPart string
	q := url.Values{}
	used := ""
	for _, pf := range pathFacets {
		for _, f := range filters {
			if f.Facet == pf.facet && used == "" && f.Tag != tagNone {
				pathPart = "/" + pf.prefix + "/"
				if f.Tag != 0 {
					pathPart += a.slugPathOf(f.Tag)
				}
				used = f.Facet
			}
		}
	}
	for _, f := range filters {
		if f.Facet == used {
			continue
		}
		switch {
		case f.Tag == tagNone:
			q.Set(facetParam[f.Facet], "-")
		case f.Tag != 0:
			q.Set(facetParam[f.Facet], a.slugPathOf(f.Tag))
		}
	}
	for i := 0; i+1 < len(extra); i += 2 {
		q.Set(extra[i], extra[i+1])
	}
	if pathPart == "" {
		pathPart = "/v"
	}
	if len(q) > 0 {
		return pathPart + "?" + strings.ReplaceAll(q.Encode(), "%2F", "/")
	}
	return pathPart
}

// withFilter returns the filters with f in place of its facet's filter;
// a zero tag removes the facet.
func withFilter(filters []Filter, f Filter) []Filter {
	var out []Filter
	placed := false
	for _, x := range filters {
		if x.Facet != f.Facet {
			out = append(out, x)
			continue
		}
		if f.Tag != 0 {
			out = append(out, f)
		}
		placed = true
	}
	if !placed && f.Tag != 0 {
		out = append(out, f)
	}
	return out
}

func (a *App) buildView(filters []Filter, page int, asGrid bool) (*View, error) {
	idx := a.index()
	v := &View{Filters: filters, Page: page, AsGrid: asGrid}
	set := idx.set(filters)
	v.Total = len(set)
	v.Empty = v.Total == 0

	var link linkFunc = func(f []Filter) string { return a.viewURL(f) }
	v.Title, v.Chips = a.chips(filters, link, "/")
	if v.Title == "" {
		v.Title = "Alles"
	}
	v.GridURL = a.viewURL(filters, "ansicht", "raster")

	v.Rows = a.refineRows(idx, set, filters, link)

	if !asGrid && v.Total > tinySet {
		a.shelves(idx, v, set)
	}
	if len(v.Shelves) == 0 {
		v.AsGrid = true
		from := min((page-1)*gridPage, len(set))
		to := min(from+gridPage, len(set))
		v.Grid = a.memCards(idx, set[from:to])
		if to < len(set) {
			v.More = a.viewURL(filters, "ansicht", "raster", "seite", strconv.Itoa(page+1))
		}
	}
	return v, nil
}

// linkFunc is the URL of the page with other filters: a view, or a search.
type linkFunc func([]Filter) string

// chips are the filters with a link per level and one to remove each;
// the title names them. Removing the last filter leads to home.
func (a *App) chips(filters []Filter, link linkFunc, home string) (string, []Chip) {
	var titles []string
	var chips []Chip
	for _, f := range filters {
		c := Chip{Facet: f.Facet, Label: facetLabels[f.Facet]}
		if f.Tag == tagNone {
			titles = append(titles, "ohne "+facetLabels[f.Facet])
			c.Crumbs = []Crumb{{Href: link(filters), Label: "ohne"}}
		} else if f.Tag == 0 {
			titles = append(titles, areaTitle(f.Facet))
			c.Crumbs = []Crumb{{Href: link(filters), Label: "alle"}}
		} else {
			titles = append(titles, f.Ref.Name)
			for _, t := range f.Path {
				c.Crumbs = append(c.Crumbs, Crumb{Href: link(withFilter(filters, Filter{Facet: f.Facet, Tag: t.ID})), Label: t.Name})
			}
			c.Edit = a.viewURL([]Filter{f}) + "#bearbeiten"
		}
		rest := withFilter(filters, Filter{Facet: f.Facet})
		c.Remove = link(rest)
		if len(rest) == 0 {
			c.Remove = home
		}
		chips = append(chips, c)
	}
	return strings.Join(titles, " · "), chips
}

// refineRows are the rows to narrow the set down, one per facet.
func (a *App) refineRows(idx *memIndex, set itemSet, filters []Filter, link linkFunc) []RefineRow {
	var rows []RefineRow
	for _, facet := range rowFacets {
		if row := a.refineRow(idx, set, filters, facet, link); len(row.Options) > 0 {
			rows = append(rows, row)
		}
	}
	// a small set is seen at a glance: only the ways further down remain,
	// and the rows that offer a choice
	if len(set) <= smallSet {
		var keep []RefineRow
		for _, r := range rows {
			if filteredBy(filters, r.Facet) || facetIsTree(r.Facet) && hasFacet(filters, r.Facet) || len(r.Options) > 1 {
				keep = append(keep, r)
			}
		}
		return keep
	}
	return rows
}

func areaTitle(facet string) string {
	switch facet {
	case "machine":
		return "Rechner"
	case "publication":
		return "Zeitschriften"
	case "event":
		return "Treffen"
	}
	return facetLabels[facet]
}

// refineRow lists the next level of a facet within the view, with counts:
// the tags below the facet's filter, or its top level.
func (a *App) refineRow(idx *memIndex, set itemSet, filters []Filter, facet string, link linkFunc) RefineRow {
	row := RefineRow{Facet: facet, Label: facetLabels[facet]}
	// groups belong to a machine, magazine, meeting or collection: offered
	// within one
	if facet == "group" && !slices.ContainsFunc(filters, func(f Filter) bool { return (facetIsTree(f.Facet) || f.Facet == "source") && f.Tag > 0 }) {
		return row
	}
	var parent int64
	for _, f := range filters {
		if f.Facet == facet && f.Tag != 0 {
			parent = f.Tag
		}
	}
	counts := idx.counts(set, facet, parent)
	// (nearly) all of the view below one tag of a facet it is not filtered
	// by: the row starts where it divides
	if parent == 0 || !filteredBy(filters, facet) {
		for len(counts) == 1 {
			var only int64
			var n int
			for id, c := range counts {
				only, n = id, c
			}
			// a tag holding (nearly) all of the view says nothing: its
			// children divide it
			if n*10 < len(set)*9 || len(idx.tags[only].Children) == 0 {
				break
			}
			parent = only
			counts = idx.counts(set, facet, parent)
		}
	}
	type opt struct {
		t *memTag
		n int
	}
	var opts []opt
	for id, n := range counts {
		opts = append(opts, opt{idx.tags[id], n})
	}
	// a single step that keeps every item is no choice (one that leads
	// further down was taken above already)
	if len(opts) == 1 && opts[0].n == len(set) {
		return row
	}
	switch facet {
	case "kind":
		sort.Slice(opts, func(i, j int) bool {
			if opts[i].t.Sort != opts[j].t.Sort {
				return opts[i].t.Sort < opts[j].t.Sort
			}
			return opts[i].n > opts[j].n
		})
	case "language", "source", "system", "keyword":
		sort.Slice(opts, func(i, j int) bool { return opts[i].n > opts[j].n })
	default:
		sort.Slice(opts, func(i, j int) bool { return naturalLess(opts[i].t.Name, opts[j].t.Name) })
	}
	for _, o := range opts {
		nf := withFilter(filters, Filter{Facet: facet, Tag: o.t.ID})
		label := o.t.Name
		if facet == "language" {
			label = languageName(label)
		}
		row.Options = append(row.Options, RefineOption{Label: label, Href: link(nf), Count: o.n, Tag: o.t.ID})
	}
	if len(opts) > rowShown+2 {
		ns := make([]int, len(opts))
		for i, o := range opts {
			ns[i] = o.n
		}
		sort.Sort(sort.Reverse(sort.IntSlice(ns)))
		least, room := ns[rowShown-1], rowShown
		for _, o := range opts {
			if o.n > least {
				room--
			}
		}
		for i, o := range opts {
			switch {
			case o.n > least:
			case o.n == least && room > 0:
				room--
			default:
				row.Options[i].Extra = true
				row.More++
			}
		}
	}
	return row
}

func languageName(code string) string {
	if n, ok := map[string]string{"de": "Deutsch", "en": "Englisch", "nl": "Niederländisch", "fr": "Französisch"}[code]; ok {
		return n
	}
	return code
}

// shelves groups a large view by the first facet that divides it.
func (a *App) shelves(idx *memIndex, v *View, set itemSet) {
	order := shelfFacets
	if hasFacet(v.Filters, "source") {
		// a collection is divided by the collections in it first
		order = append([]string{"source"}, shelfFacets...)
	}
	for _, facet := range order {
		var row *RefineRow
		for i := range v.Rows {
			if v.Rows[i].Facet == facet {
				row = &v.Rows[i]
			}
		}
		// a large view needs a facet that divides it; a small one is
		// divided by any part of a facet that leaves a rest beside it
		small := v.Total <= gridPage
		if row == nil || len(row.Options) == 0 || len(row.Options) == 1 && !small {
			continue
		}
		opts := append([]RefineOption(nil), row.Options...)
		switch {
		case v.ByRank:
			// set is in the order of relevance: a shelf ranks by its first item
			first := map[int64]int{}
			for _, o := range opts {
				first[o.Tag] = len(set)
				mark := make([]bool, len(idx.items))
				idx.markBelow(o.Tag, mark)
				for k, i := range set {
					if mark[i] {
						first[o.Tag] = k
						break
					}
				}
			}
			sort.SliceStable(opts, func(i, j int) bool { return first[opts[i].Tag] < first[opts[j].Tag] })
			opts = opts[:min(len(opts), maxShelves)]
		case len(opts) > maxShelves:
			sort.SliceStable(opts, func(i, j int) bool { return opts[i].Count > opts[j].Count })
			opts = opts[:maxShelves]
		}
		covered := 0
		for _, o := range opts {
			covered += o.Count
		}
		if covered*2 < v.Total && !small {
			continue // this facet says little about most of the view
		}
		if len(opts) == 1 && covered >= v.Total {
			continue // one part holding all is no division
		}
		for _, o := range opts {
			sub, n := idx.within(set, o.Tag, 0)
			if !v.ByRank {
				sub = idx.ranked(sub, shelfItems)
			}
			v.Shelves = append(v.Shelves, Shelf{Label: o.Label, Href: o.Href, Count: n, Cards: a.memCards(idx, sub[:min(len(sub), shelfItems)])})
		}
		// what no shelf holds: the items without a tag of the facet, or
		// carrying the filter's tag itself
		var tags []int64
		for _, o := range row.Options {
			tags = append(tags, o.Tag)
		}
		if rest, n := idx.without(set, tags, 0); n > 0 {
			if !v.ByRank {
				rest = idx.ranked(rest, shelfItems)
			}
			sh := Shelf{Label: "Weitere", Count: n, Cards: a.memCards(idx, rest[:min(len(rest), shelfItems)])}
			if !hasFacet(v.Filters, facet) {
				sh.Label = "Ohne " + facetLabels[facet]
				sh.Href = a.viewURL(withFilter(v.Filters, Filter{Facet: facet, Tag: tagNone}))
			}
			at := len(v.Shelves)
			if v.ByRank {
				// the rest takes the place of its best item among the shelves
				pos := map[int32]int{}
				for k, i := range set {
					pos[i] = k
				}
				for k, o := range opts {
					if first := firstIn(idx, set, o.Tag, pos); pos[rest[0]] < first {
						at = len(v.Shelves) - len(opts) + k
						break
					}
				}
			}
			v.Shelves = slices.Insert(v.Shelves, at, sh)
		}
		return
	}
}

// firstIn is the position in set of its first item carrying tag or one
// below it.
func firstIn(idx *memIndex, set itemSet, tag int64, pos map[int32]int) int {
	mark := make([]bool, len(idx.items))
	idx.markBelow(tag, mark)
	for _, i := range set {
		if mark[i] {
			return pos[i]
		}
	}
	return len(set)
}

// cards loads the items a query selects, in its order.
func (a *App) cards(query string, args ...any) ([]Card, error) {
	rows, err := a.cat.Query(query, args...)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]Card, 0, len(ids))
	for _, id := range ids {
		out = append(out, a.card(id))
	}
	return out, nil
}

func (a *App) card(id int64) Card {
	c := Card{ID: id, Href: "/a/" + strconv.FormatInt(id, 10)}
	var descr string
	var root sql.NullString
	a.cat.QueryRow("SELECT title, descr, root, (SELECT COUNT(*) FROM item_files WHERE item = items.id) FROM items WHERE id = ?", id).
		Scan(&c.Title, &descr, &root, &c.Files)
	var p string
	a.cat.QueryRow("SELECT path FROM item_files WHERE item = ? ORDER BY ord, name LIMIT 1", id).Scan(&p)
	name := path.Base(p)
	e := extOf(name)
	switch {
	case root.Valid:
		c.Kind, c.Type = "other", "Website"
	case imageExts["."+e]:
		c.Kind = "image"
	case e == "pdf":
		c.Kind = "pdf"
	default:
		c.Kind = "other"
	}
	if c.Type == "" {
		c.Type = typeHint(name)
	}
	if c.Files == 1 && !root.Valid {
		c.File = href(p)
	}
	if c.Kind != "other" && c.Files >= 1 {
		c.Thumb = a.thumbFor("thumbs", p)
	}
	// the file name says more than a title that only repeats it
	if c.Title == name || c.Title == "" {
		c.Title = prettyName(name)
	} else if descr != "" && descr != c.Title {
		c.Sub = descr
	}
	if c.Files > 1 {
		c.Sub = strings.TrimSpace(c.Sub + " " + fmt.Sprintf("(%d Dateien)", c.Files))
	}
	return c
}

// prettyName makes a file name readable as a title: Big_K_Issue_01.pdf ->
// Big K Issue 01.
func prettyName(name string) string {
	s := strings.TrimSuffix(name, path.Ext(name))
	s = strings.NewReplacer("_", " ").Replace(s)
	return strings.TrimSpace(s)
}

// Start page

type StartView struct {
	Entries []Entry
	Random  []Card
	Recent  []Card
	Files   int64
	Bytes   int64
}

type Entry struct {
	Label, Hint, Href string
	Count             int
	Thumb             string
	Icon              string // the drawing shown when there is no picture
}

func (a *App) startPage(w http.ResponseWriter, r *http.Request) {
	idx := a.index()
	v := &StartView{}
	a.cat.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM file_paths WHERE seen >= ?", a.lastSync()).Scan(&v.Files, &v.Bytes)
	// a picture from a set, a different one each time
	picture := func(set itemSet) string {
		for range 12 {
			if len(set) == 0 {
				break
			}
			c := a.memCard(idx, set[rand.IntN(len(set))])
			if c.Thumb != "" {
				return c.Thumb
			}
		}
		return ""
	}
	entry := func(label, hint, href, icon string, filters []Filter) {
		set := idx.set(filters)
		if len(set) == 0 {
			return
		}
		v.Entries = append(v.Entries, Entry{Label: label, Hint: hint, Href: href, Count: len(set), Thumb: picture(set), Icon: icon})
	}
	source := func(name string) []Filter {
		for _, id := range idx.roots["source"] {
			if idx.tags[id].Name == name {
				return []Filter{{Facet: "source", Tag: id}}
			}
		}
		return []Filter{{Facet: "source", Tag: -1}}
	}
	kind := func(name string) []Filter {
		for _, id := range idx.roots["kind"] {
			if idx.tags[id].Name == name {
				return []Filter{{Facet: "kind", Tag: id}}
			}
		}
		return []Filter{{Facet: "kind", Tag: -1}}
	}
	entry("Rechner", "nach Hersteller, Serie und Modell", "/r/", "machine", []Filter{{Facet: "machine"}})
	entry("Zeitschriften", "nach Plattform und Titel", "/z/", "magazine", []Filter{{Facet: "publication"}})
	entry("Treffen", a.cfg.EventsHint, "/treffen/", "camera", []Filter{{Facet: "event"}})
	entry("CD-ROMs und Sammlungen", a.cfg.CollectionsHint, "/v?quelle=sammlungen", "disc", source("Sammlungen"))
	entry("Gesicherte Websites", "Kopien verschwundener Seiten", "/v?art=mirror", "globe", kind("Mirror"))

	// a shelf of chance finds: covers and photos at random, each once
	seen := map[int32]bool{}
	for tries := 0; tries < 200 && len(v.Random) < shelfItems && len(idx.items) > 0; tries++ {
		i := int32(rand.IntN(len(idx.items)))
		if seen[i] {
			continue
		}
		seen[i] = true
		it := &idx.items[i]
		if e := extOf(it.Path); (e != "pdf" && e != "jpg" && e != "jpeg") || strings.HasPrefix(it.Path, "public/saved_mirrors/") {
			continue
		}
		if c := a.memCard(idx, i); c.Thumb != "" {
			v.Random = append(v.Random, c)
		}
	}
	v.Recent, _ = a.cards("SELECT id FROM items WHERE state = 'live' ORDER BY created DESC, id DESC LIMIT ?", shelfItems)
	a.renderSite(w, r, http.StatusOK, "v-start", &SitePage{Title: "", Data: v})
}

// tagInfo is what the view of a single tag shows besides its items.
func (a *App) tagInfo(ref *TagRef) *TagView {
	t := &TagView{Ref: *ref}
	a.cat.QueryRow("SELECT name, descr, rev FROM tags WHERE id = ?", ref.ID).Scan(&t.Name, &t.Descr, &t.Rev)
	a.cat.QueryRow("SELECT body FROM tag_pages WHERE tag = ?", ref.ID).Scan(&t.Page)
	if i := strings.LastIndex(ref.Path, " › "); i >= 0 {
		t.ParentPath = ref.Path[:i]
	}
	a.cat.QueryRow("SELECT EXISTS (SELECT 1 FROM revisions WHERE target IN (?, ?))", targetOf("tag", ref.ID), targetOf("page", ref.ID)).Scan(&t.History)
	t.Comments = a.comments(targetOf("tag", ref.ID))
	return t
}

func hasFacet(filters []Filter, facet string) bool {
	for _, f := range filters {
		if f.Facet == facet {
			return true
		}
	}
	return false
}

func filteredBy(filters []Filter, facet string) bool {
	for _, f := range filters {
		if f.Facet == facet && f.Tag != 0 {
			return true
		}
	}
	return false
}
