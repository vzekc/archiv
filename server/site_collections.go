package archiv

import (
	"net/http"
	"strconv"
	"strings"
)

// Curated collections: an ordered list of items with an introduction, for
// where a hand-made order says more than a tag ("Getting started with the
// PDP-11"). They are put together in the archive browser and shown at
// /s/<id>.

type CollectionView struct {
	ID    int64
	Title string
	Intro string
	Owner string
	Rev   int
	Items []ItemRow
}

func (a *App) collectionPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	v := &CollectionView{ID: id}
	if err != nil || a.cat.QueryRow("SELECT title, intro, owner, rev FROM collections WHERE id = ?", id).Scan(&v.Title, &v.Intro, &v.Owner, &v.Rev) != nil {
		a.notFound(w, r, "Diese Sammlung gibt es nicht.")
		return
	}
	rows, err := a.cat.Query("SELECT item FROM coll_items WHERE coll = ? ORDER BY ord", id)
	if err == nil {
		var ids []int64
		for rows.Next() {
			var x int64
			rows.Scan(&x)
			ids = append(ids, x)
		}
		rows.Close()
		byID := map[int64]ItemRow{}
		for _, it := range a.itemsWithIDs(ids) {
			byID[it.ID] = it
		}
		for _, x := range ids {
			if it, ok := byID[x]; ok {
				v.Items = append(v.Items, it)
			}
		}
	}
	a.renderSite(w, r, http.StatusOK, "site-collection", &SitePage{Title: v.Title,
		Crumbs: []Crumb{{"/", "Start"}, {"/s/" + strconv.FormatInt(id, 10), v.Title}}, Data: v})
}

// itemsWithIDs loads the rows of the given live items.
func (a *App) itemsWithIDs(ids []int64) []ItemRow {
	if len(ids) == 0 {
		return nil
	}
	var out []ItemRow
	for len(ids) > 0 {
		chunk := ids[:min(len(ids), 500)]
		ids = ids[len(chunk):]
		// itemsWithTags selects by tag; the same row shape by id
		out = append(out, a.itemRows("i.id IN ("+inList(len(chunk))+")", int64Args(chunk))...)
	}
	return out
}

func (a *App) editCollection(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	back := "/s/" + strconv.FormatInt(id, 10)
	var title, intro string
	if a.cat.QueryRow("SELECT title, intro FROM collections WHERE id = ?", id).Scan(&title, &intro) != nil {
		a.notFound(w, r, "")
		return
	}
	rev, _ := strconv.Atoi(r.FormValue("rev"))
	target := targetOf("coll", id)
	var changes []Change
	for _, f := range []struct{ name, cur string }{{"title", title}, {"intro", intro}} {
		if v, ok := r.Form[f.name]; ok {
			nv := strings.TrimSpace(strings.ReplaceAll(v[0], "\r\n", "\n"))
			if nv != f.cur {
				changes = append(changes, Change{Target: target, Op: "set-coll", Data: map[string]any{"field": f.name, "old": f.cur, "new": nv}, Rev: -1})
			}
		}
	}
	if len(changes) > 0 {
		changes[0].Rev = rev
	}
	a.commit(w, r, changes, back)
}
