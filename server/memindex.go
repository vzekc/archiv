package archiv

import (
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// The catalog in memory, for the views: the tags with their tree, every
// live item with its direct tags, title and first file, and the items in
// title order. Faceted browsing is set arithmetic over this; asking SQLite
// for every count of every facet of a view of 40,000 items takes seconds.
// The index is rebuilt when the catalog has changed: after a sync, or when
// a revision was written.

// listingNames are the file lists that came with the BBS and CD-ROM
// collections, and the caches the operating systems left in folders: part
// of the archive, but nothing to browse or find.
var listingNames = map[string]bool{
	"00-index.txt": true, "-index": true, "00index.txt": true, "files.bbs": true, "wildcat.txt": true,
	"descript.ion": true, "!dir.txt": true, "!dir": true, "0.gallery.html": true, "allfiles.lst": true,
	"files.lst": true, "00_index.txt": true, "thumbs.db": true, ".ds_store": true, "desktop.ini": true,
}

type memTag struct {
	ID       int64
	Facet    string
	Name     string
	Slug     string
	Parent   int64
	Sort     int
	Children []int64
	Anc      []int64 // itself and the tags above it, nearest first
	Items    []int32 // indexes into memIndex.items carrying it directly
}

type memItem struct {
	ID    int64
	Title string
	Descr string
	Root  string // a mirror's folder
	Path  string // its first file
	Pic   string // the file its card shows: the cover, else the first picture or PDF
	Files int
	Tags  []int64
	Rank  int // the sort of its kind: what a shelf shows first
}

type memIndex struct {
	key   string
	tags  map[int64]*memTag
	roots map[string][]int64 // facet -> tags without a parent
	items []memItem          // in title order
	byID  map[int64]int32
}

var mem struct {
	sync.Mutex
	idx *memIndex
}

// memKey says whether the catalog has changed since the index was built.
func (a *App) memKey() string {
	var rev, items int64
	a.cat.QueryRow("SELECT COALESCE(MAX(id), 0) FROM revisions").Scan(&rev)
	a.cat.QueryRow("SELECT COALESCE(MAX(updated), 0) + COUNT(*) FROM items").Scan(&items)
	return a.cat.meta("last_sync") + "/" + itoa(rev) + "/" + itoa(items)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// index returns the current index, building it when the catalog changed.
func (a *App) index() *memIndex {
	key := a.memKey()
	mem.Lock()
	defer mem.Unlock()
	if mem.idx != nil && mem.idx.key == key {
		return mem.idx
	}
	idx, err := a.buildIndex(key)
	if err != nil {
		logf("index: %v", err)
		if mem.idx != nil {
			return mem.idx
		}
		return &memIndex{tags: map[int64]*memTag{}, roots: map[string][]int64{}, byID: map[int64]int32{}}
	}
	mem.idx = idx
	return idx
}

func (a *App) buildIndex(key string) (*memIndex, error) {
	idx := &memIndex{key: key, tags: map[int64]*memTag{}, roots: map[string][]int64{}, byID: map[int64]int32{}}
	rows, err := a.cat.Query("SELECT id, facet, name, slug, COALESCE(parent, 0), sort FROM tags WHERE merged_into IS NULL")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &memTag{}
		rows.Scan(&t.ID, &t.Facet, &t.Name, &t.Slug, &t.Parent, &t.Sort)
		idx.tags[t.ID] = t
	}
	rows.Close()
	for _, t := range idx.tags {
		if p := idx.tags[t.Parent]; p != nil {
			p.Children = append(p.Children, t.ID)
		} else {
			idx.roots[t.Facet] = append(idx.roots[t.Facet], t.ID)
		}
		for x := t; x != nil && len(t.Anc) < 16; x = idx.tags[x.Parent] {
			t.Anc = append(t.Anc, x.ID)
		}
	}

	rows, err = a.cat.Query("SELECT id, title, descr, COALESCE(root, '') FROM items WHERE state = 'live'")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it memItem
		rows.Scan(&it.ID, &it.Title, &it.Descr, &it.Root)
		idx.items = append(idx.items, it)
	}
	rows.Close()
	sort.Slice(idx.items, func(i, j int) bool {
		ti, tj := strings.ToLower(idx.items[i].Title), strings.ToLower(idx.items[j].Title)
		if ti != tj {
			return naturalLess(ti, tj)
		}
		return idx.items[i].ID < idx.items[j].ID
	})
	for i := range idx.items {
		idx.byID[idx.items[i].ID] = int32(i)
	}

	rows, err = a.cat.Query("SELECT item, path, role FROM item_files ORDER BY item, ord, name")
	if err != nil {
		return nil, err
	}
	covered := map[int64]bool{}
	for rows.Next() {
		var id int64
		var p, role string
		rows.Scan(&id, &p, &role)
		if i, ok := idx.byID[id]; ok {
			it := &idx.items[i]
			if it.Files == 0 {
				it.Path = p
			}
			it.Files++
			switch e := extOf(p); {
			case role == "cover" && !covered[id]:
				it.Pic, covered[id] = p, true
			case it.Pic == "" && (imageExts["."+e] || e == "pdf"):
				it.Pic = p
			}
		}
	}
	rows.Close()
	// file lists of the collections are no items to browse
	kept := idx.items[:0]
	for _, it := range idx.items {
		if it.Files == 1 && it.Root == "" && listingNames[strings.ToLower(path.Base(it.Path))] {
			continue
		}
		kept = append(kept, it)
	}
	idx.items = kept
	clear(idx.byID)
	for i := range idx.items {
		idx.byID[idx.items[i].ID] = int32(i)
	}

	rows, err = a.cat.Query("SELECT item, tag FROM item_tags")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id, tag int64
		rows.Scan(&id, &tag)
		i, ok := idx.byID[id]
		t := idx.tags[tag]
		if !ok || t == nil {
			continue
		}
		idx.items[i].Tags = append(idx.items[i].Tags, tag)
		t.Items = append(t.Items, i)
	}
	rows.Close()
	for i := range idx.items {
		it := &idx.items[i]
		it.Rank = noKind
		for _, tid := range it.Tags {
			if t := idx.tags[tid]; t.Facet == "kind" && t.Sort < it.Rank {
				it.Rank = t.Sort
			}
		}
	}
	return idx, nil
}

// noKind ranks the items without a kind after all others.
const noKind = 1 << 20

// ranked keeps up to limit items of set, those of the first kinds first
// (taxonomy.conf, sort=), and of a kind those with a picture to show;
// otherwise in title order.
func (idx *memIndex) ranked(set itemSet, limit int) itemSet {
	out := slices.Clone(set)
	pictureless := func(i int32) int {
		if idx.items[i].Pic == "" {
			return 1
		}
		return 0
	}
	slices.SortStableFunc(out, func(a, b int32) int {
		if d := idx.items[a].Rank - idx.items[b].Rank; d != 0 {
			return d
		}
		return pictureless(a) - pictureless(b)
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

// itemSet is a set of items as indexes, in title order.
type itemSet []int32

// set selects the items of the filters.
func (idx *memIndex) set(filters []Filter) itemSet {
	var in []bool
	for _, f := range filters {
		mark := make([]bool, len(idx.items))
		if f.Tag == tagNone {
			for _, id := range idx.roots[f.Facet] {
				idx.markBelow(id, mark)
			}
			for i := range mark {
				mark[i] = !mark[i]
			}
		} else if f.Tag == 0 {
			for _, id := range idx.roots[f.Facet] {
				idx.markBelow(id, mark)
			}
		} else {
			idx.markBelow(f.Tag, mark)
		}
		if in == nil {
			in = mark
			continue
		}
		for i := range in {
			in[i] = in[i] && mark[i]
		}
	}
	var out itemSet
	for i := range idx.items {
		if in == nil || in[i] {
			out = append(out, int32(i))
		}
	}
	return out
}

// markBelow marks the items carrying the tag or one below it.
func (idx *memIndex) markBelow(id int64, mark []bool) {
	t := idx.tags[id]
	if t == nil {
		return
	}
	for _, i := range t.Items {
		mark[i] = true
	}
	for _, c := range t.Children {
		idx.markBelow(c, mark)
	}
}

// counts tells, for each tag directly below parent (or each top tag of
// the facet when parent is 0), how many items of the set it covers.
func (idx *memIndex) counts(set itemSet, facet string, parent int64) map[int64]int {
	out := map[int64]int{}
	seen := map[int64]bool{}
	for _, i := range set {
		clear(seen)
		for _, tid := range idx.items[i].Tags {
			t := idx.tags[tid]
			if t == nil || t.Facet != facet {
				continue
			}
			// the ancestor of t that hangs directly below parent
			for _, a := range t.Anc {
				at := idx.tags[a]
				if at.Parent == parent {
					if !seen[a] {
						seen[a] = true
						out[a]++
					}
					break
				}
			}
		}
	}
	return out
}

// within keeps the items of set that carry tag or one below it.
func (idx *memIndex) within(set itemSet, tag int64, limit int) (itemSet, int) {
	mark := make([]bool, len(idx.items))
	idx.markBelow(tag, mark)
	var out itemSet
	n := 0
	for _, i := range set {
		if mark[i] {
			n++
			if limit <= 0 || len(out) < limit {
				out = append(out, i)
			}
		}
	}
	return out, n
}

// without keeps the items of set that carry none of the tags nor one
// below them.
func (idx *memIndex) without(set itemSet, tags []int64, limit int) (itemSet, int) {
	mark := make([]bool, len(idx.items))
	for _, t := range tags {
		idx.markBelow(t, mark)
	}
	var out itemSet
	n := 0
	for _, i := range set {
		if !mark[i] {
			n++
			if limit <= 0 || len(out) < limit {
				out = append(out, i)
			}
		}
	}
	return out, n
}

// card turns an item of the index into a card.
func (a *App) memCard(idx *memIndex, i int32) Card {
	it := &idx.items[i]
	c := Card{ID: it.ID, Href: "/a/" + itoa(it.ID), Title: it.Title, Files: it.Files}
	name := path.Base(it.Path)
	pic := it.Pic
	if pic == "" {
		pic = it.Path
	}
	e := extOf(pic)
	switch {
	case it.Root != "":
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
	if it.Files == 1 && it.Root == "" {
		c.File = href(it.Path)
	}
	if c.Kind != "other" && pic != "" {
		c.Thumb = a.thumbFor("thumbs", pic)
	}
	if c.Title == name || c.Title == "" {
		c.Title = prettyName(name)
	} else if it.Descr != "" && it.Descr != it.Title {
		c.Sub = it.Descr
	}
	return c
}

func (a *App) memCards(idx *memIndex, set itemSet) []Card {
	out := make([]Card, 0, len(set))
	for _, i := range set {
		out = append(out, a.memCard(idx, i))
	}
	return out
}

// restrict keeps the items of set that the filters select, in set's order.
func (idx *memIndex) restrict(set itemSet, filters []Filter) itemSet {
	if len(filters) == 0 {
		return set
	}
	marks := make([][]bool, len(filters))
	for k, f := range filters {
		marks[k] = make([]bool, len(idx.items))
		if f.Tag == 0 {
			for _, id := range idx.roots[f.Facet] {
				idx.markBelow(id, marks[k])
			}
		} else {
			idx.markBelow(f.Tag, marks[k])
		}
	}
	var out itemSet
next:
	for _, i := range set {
		for _, m := range marks {
			if !m[i] {
				continue next
			}
		}
		out = append(out, i)
	}
	return out
}

// below is every item carrying the tag or one below it, in title order.
func (idx *memIndex) below(tag int64) itemSet {
	mark := make([]bool, len(idx.items))
	idx.markBelow(tag, mark)
	var out itemSet
	for i, m := range mark {
		if m {
			out = append(out, int32(i))
		}
	}
	return out
}
