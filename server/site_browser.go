package archiv

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The moderators' archive browser: a file manager whose operations are tag
// changes. The page is /browser; it talks to the JSON API below. Every
// operation is a batch of catalog changes (the same code path as the edit
// forms), so history, rights and conflicts work the same, and a batch can
// be undone as a whole. Open browsers learn about changes through
// server-sent events.

func (a *App) browserRoutes(mux *http.ServeMux) {
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	page("GET /browser", a.mod(a.browserPage))
	page("GET /api/tree", a.mod(a.apiTree))
	page("GET /api/node", a.mod(a.apiNode))
	page("GET /api/item", a.mod(a.apiItem))
	page("POST /api/batch", a.mod(a.apiPost(a.apiBatch)))
	page("POST /api/undo", a.mod(a.apiPost(a.apiUndo)))
	page("GET /api/events", a.mod(a.apiEvents))
	page("POST /api/upload", a.mod(a.apiPost(a.apiUpload)))
}

func (a *App) browserPage(w http.ResponseWriter, r *http.Request) {
	a.renderSite(w, r, http.StatusOK, "site-browser", &SitePage{Title: "Archiv-Browser",
		Crumbs: []Crumb{{"/", "Start"}, {"/browser", "Archiv-Browser"}}})
}

// apiPost checks the CSRF token of a JSON request (header X-CSRF-Token) and
// that the catalog may be edited.
func (a *App) apiPost(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/upload" {
			r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
		}
		if !a.csrfOK(r) {
			jsonError(w, http.StatusForbidden, "Die Seite ist abgelaufen. Bitte neu laden.")
			return
		}
		if !a.cfg.Editing {
			jsonError(w, http.StatusForbidden, "Der Katalog ist noch schreibgeschützt.")
			return
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func jsonError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// Nodes of the tree:
//
//	f:<facet>     the top of a facet
//	t:<id>        a tag (location tags are the old folders)
//	unsorted      items without a machine or kind tag
//	s:<path>      the storage: folders and files as they lie on the share
//	q:<text>      a search
type Node struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Facet    string `json:"facet,omitempty"`
	Tag      int64  `json:"tag,omitempty"`
	Count    int    `json:"count"`
	Children bool   `json:"children"`
	Href     string `json:"href,omitempty"`
	Kind     string `json:"kind"` // facet, tag, view, storage
	Rev      int    `json:"rev,omitempty"`
}

var browserFacets = []string{"machine", "kind", "publication", "event", "system", "language", "source", "location"}

func (a *App) apiTree(w http.ResponseWriter, r *http.Request) {
	node := r.URL.Query().Get("node")
	var out []Node
	switch {
	case node == "" || node == "root":
		for _, f := range browserFacets {
			label := facetLabels[f]
			if f == "location" {
				label = "Alter Ordnerbaum"
			}
			out = append(out, Node{ID: "f:" + f, Label: label, Facet: f, Children: true, Kind: "facet"})
		}
		var n int
		a.cat.QueryRow("SELECT COUNT(*) " + unsortedWhere).Scan(&n)
		var nc int
		a.cat.QueryRow("SELECT COUNT(*) FROM collections").Scan(&nc)
		out = append(out, Node{ID: "coll", Label: "Sammlungen", Count: nc, Children: nc > 0, Kind: "collections"},
			Node{ID: "unsorted", Label: "Unsortiert", Count: n, Kind: "view"},
			Node{ID: "s:", Label: "Speicherort", Children: true, Kind: "storage"})
	case strings.HasPrefix(node, "f:"):
		out = a.tagNodes("parent IS NULL AND facet = ?", node[2:])
	case strings.HasPrefix(node, "t:"):
		id, _ := strconv.ParseInt(node[2:], 10, 64)
		out = a.tagNodes("parent = ?", id)
	case node == "coll":
		out = a.collNodes()
	case strings.HasPrefix(node, "s:"):
		for _, d := range a.storageDirs(node[2:]) {
			out = append(out, Node{ID: "s:" + d, Label: path.Base(d), Children: true, Kind: "storage", Href: href(d) + "/"})
		}
	}
	writeJSON(w, out)
}

const unsortedWhere = "FROM items i WHERE " + needsTagsWhere

// tagNodes lists tags with the number of their items and whether they
// have children.
func (a *App) tagNodes(where string, arg any) []Node {
	rows, err := a.cat.Query(`SELECT t.id, t.facet, t.name, t.rev,
	        (SELECT COUNT(*) FROM item_tags it JOIN items i ON i.id = it.item WHERE it.tag = t.id AND i.state = 'live'),
	        EXISTS (SELECT 1 FROM tags c WHERE c.parent = t.id AND c.merged_into IS NULL)
	    FROM tags t WHERE t.merged_into IS NULL AND `+where+` ORDER BY t.sort, lower(t.name)`, arg)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		rows.Scan(&n.Tag, &n.Facet, &n.Label, &n.Rev, &n.Count, &n.Children)
		n.ID = "t:" + strconv.FormatInt(n.Tag, 10)
		n.Kind = "tag"
		out = append(out, n)
	}
	for i := range out {
		if ref := a.tagRef(out[i].Tag); ref != nil {
			out[i].Href = ref.Href
		}
	}
	return out
}

// storageDirs are the folders directly below a folder of the share, as
// the last sync found them.
func (a *App) storageDirs(rel string) []string {
	lo, hi := pathRange(rel)
	rows, err := a.cat.Query(`SELECT DISTINCT substr(rest, 1, instr(rest, '/') - 1) FROM
	    (SELECT substr(path, ?) AS rest FROM file_paths WHERE path >= ? AND path < ? AND seen >= ?) WHERE instr(rest, '/') > 0`,
		len(lo)+1, lo, hi, a.lastSync())
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var d string
		rows.Scan(&d)
		out = append(out, joinRel(rel, d))
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i]) < strings.ToLower(out[j]) })
	return out
}

// BrowserItem is an item as the browser shows it.
type BrowserItem struct {
	ID     int64   `json:"id"`
	Title  string  `json:"title"`
	Descr  string  `json:"descr"`
	Year   string  `json:"year"`
	Rev    int     `json:"rev"`
	Files  int     `json:"files"`
	Size   int64   `json:"size"`
	Name   string  `json:"name"`
	Path   string  `json:"path"`
	Type   string  `json:"type"`
	Thumb  string  `json:"thumb,omitempty"`
	Tags   []BrTag `json:"tags"`
	Others int     `json:"others"` // tags besides the node's, outside the location facet
	Stored bool    `json:"stored"` // lies below objekte/
	State  string  `json:"state"`
}

type BrTag struct {
	ID    int64  `json:"id"`
	Facet string `json:"facet"`
	Label string `json:"label"`
}

type NodeContent struct {
	Node  Node          `json:"node"`
	Items []BrowserItem `json:"items"`
	Total int           `json:"total"`
	Sub   []Node        `json:"sub"`
}

func (a *App) apiNode(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	node := q.Get("node")
	offset, _ := strconv.Atoi(q.Get("offset"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	c := &NodeContent{Node: Node{ID: node}}
	var where string
	var args []any
	switch {
	case strings.HasPrefix(node, "t:"):
		id, _ := strconv.ParseInt(node[2:], 10, 64)
		ref := a.tagRef(id)
		if ref == nil {
			jsonError(w, http.StatusNotFound, "Das Schlagwort gibt es nicht mehr.")
			return
		}
		c.Node = Node{ID: node, Label: ref.Path, Facet: ref.Facet, Tag: id, Kind: "tag", Href: ref.Href}
		a.cat.QueryRow("SELECT rev FROM tags WHERE id = ?", id).Scan(&c.Node.Rev)
		where, args = "i.id IN (SELECT item FROM item_tags WHERE tag = ?)", []any{id}
		c.Sub = a.tagNodes("parent = ?", id)
	case strings.HasPrefix(node, "f:"):
		c.Node = Node{ID: node, Label: facetLabels[node[2:]], Facet: node[2:], Kind: "facet"}
		c.Sub = a.tagNodes("parent IS NULL AND facet = ?", node[2:])
		writeJSON(w, c)
		return
	case node == "coll":
		c.Node = Node{ID: node, Label: "Sammlungen", Kind: "collections"}
		c.Sub = a.collNodes()
		writeJSON(w, c)
		return
	case strings.HasPrefix(node, "c:"):
		id, _ := strconv.ParseInt(node[2:], 10, 64)
		var title string
		var rev int
		if a.cat.QueryRow("SELECT title, rev FROM collections WHERE id = ?", id).Scan(&title, &rev) != nil {
			jsonError(w, http.StatusNotFound, "Die Sammlung gibt es nicht mehr.")
			return
		}
		c.Node = Node{ID: node, Label: "Sammlung: " + title, Kind: "collection", Tag: id, Rev: rev, Href: "/s/" + node[2:]}
		a.cat.QueryRow("SELECT COUNT(*) FROM coll_items ci JOIN items i ON i.id = ci.item WHERE ci.coll = ? AND i.state = 'live'", id).Scan(&c.Total)
		c.Items = a.browserItems(`i.state = 'live' AND i.id IN (SELECT item FROM coll_items WHERE coll = ?)
		    ORDER BY (SELECT ord FROM coll_items WHERE coll = ? AND item = i.id) LIMIT ? OFFSET ?`, []any{id, id, limit, offset}, 0)
		writeJSON(w, c)
		return
	case node == "unsorted":
		c.Node = Node{ID: node, Label: "Unsortiert", Kind: "view"}
		where = strings.TrimPrefix(strings.SplitN(unsortedWhere, "WHERE ", 2)[1], "")
	case strings.HasPrefix(node, "s:"):
		rel := node[2:]
		c.Node = Node{ID: node, Label: "Speicherort: /" + rel, Kind: "storage", Href: href(rel) + "/"}
		lo, hi := pathRange(rel)
		where = `i.id IN (SELECT f.item FROM item_files f WHERE f.path >= ? AND f.path < ? AND instr(substr(f.path, ?), '/') = 0)`
		args = []any{lo, hi, len(lo) + 1}
		for _, d := range a.storageDirs(rel) {
			c.Sub = append(c.Sub, Node{ID: "s:" + d, Label: path.Base(d), Children: true, Kind: "storage"})
		}
	case strings.HasPrefix(node, "q:"):
		text := node[2:]
		c.Node = Node{ID: node, Label: "Suche: " + text, Kind: "view"}
		where, args = "i.id IN (SELECT rowid FROM items_fts WHERE items_fts MATCH ?)", []any{ftsQuery(text)}
	default:
		jsonError(w, http.StatusNotFound, "unbekannter Knoten")
		return
	}
	a.cat.QueryRow("SELECT COUNT(*) FROM items i WHERE i.state = 'live' AND "+where, args...).Scan(&c.Total)
	c.Items = a.browserItems("i.state = 'live' AND "+where+" ORDER BY lower(i.title), i.id LIMIT ? OFFSET ?",
		append(args, limit, offset), c.Node.Tag)
	writeJSON(w, c)
}

// browserItems loads items with their tags.
func (a *App) browserItems(where string, args []any, here int64) []BrowserItem {
	rows, err := a.cat.Query(`SELECT i.id, i.title, i.descr, i.year, i.rev, i.state, i.root,
	        (SELECT COUNT(*) FROM item_files f WHERE f.item = i.id),
	        (SELECT COALESCE(SUM(fl.size), 0) FROM item_files f JOIN files fl ON fl.sha256 = f.sha256 WHERE f.item = i.id),
	        COALESCE((SELECT f.path FROM item_files f WHERE f.item = i.id ORDER BY f.ord, f.name LIMIT 1), '')
	    FROM items i WHERE `+where, args...)
	if err != nil {
		logf("browser items: %v", err)
		return nil
	}
	var out []BrowserItem
	for rows.Next() {
		var it BrowserItem
		var root sql.NullString
		rows.Scan(&it.ID, &it.Title, &it.Descr, &it.Year, &it.Rev, &it.State, &root, &it.Files, &it.Size, &it.Path)
		if root.Valid {
			it.Path = root.String
		}
		it.Name = path.Base(it.Path)
		if it.Files == 1 {
			it.Type = typeHint(it.Name)
		}
		it.Stored = strings.HasPrefix(it.Path, "objekte/")
		out = append(out, it)
	}
	rows.Close()
	for i := range out {
		it := &out[i]
		if e := extOf(it.Name); it.Files == 1 && (e == "pdf" || imageExts["."+e]) {
			it.Thumb = a.thumbFor("thumbs", it.Path)
		}
		trows, err := a.cat.Query(`SELECT t.id, t.facet FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = ? ORDER BY t.facet, t.name`, it.ID)
		if err != nil {
			continue
		}
		var ids []BrTag
		for trows.Next() {
			var t BrTag
			trows.Scan(&t.ID, &t.Facet)
			ids = append(ids, t)
		}
		trows.Close()
		for _, t := range ids {
			if ref := a.tagRef(t.ID); ref != nil {
				t.Label = facetLabels[t.Facet] + ": " + ref.Path
				it.Tags = append(it.Tags, t)
				if t.ID != here && t.Facet != "location" {
					it.Others++
				}
			}
		}
	}
	return out
}

func (a *App) apiItem(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
	items := a.browserItems("i.id = ?", []any{id}, 0)
	if len(items) == 0 {
		jsonError(w, http.StatusNotFound, "Das Objekt gibt es nicht.")
		return
	}
	writeJSON(w, items[0])
}

func (a *App) collNodes() []Node {
	rows, err := a.cat.Query(`SELECT c.id, c.title, c.rev, (SELECT COUNT(*) FROM coll_items ci WHERE ci.coll = c.id) FROM collections c ORDER BY lower(c.title)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		var n Node
		rows.Scan(&n.Tag, &n.Label, &n.Rev, &n.Count)
		n.ID = "c:" + strconv.FormatInt(n.Tag, 10)
		n.Kind = "collection"
		n.Href = "/s/" + strconv.FormatInt(n.Tag, 10)
		out = append(out, n)
	}
	return out
}

// Op is one operation of the browser, in its own terms.
type Op struct {
	Op     string   `json:"op"` // move, add, remove, set, new-folder, rename-folder, move-folder, merge-folders, withdraw, store
	Items  []int64  `json:"items"`
	Revs   []int    `json:"revs"` // revisions of the items as the browser saw them
	From   int64    `json:"from"`
	To     int64    `json:"to"`
	Tag    int64    `json:"tag"`
	Field  string   `json:"field"`
	Value  string   `json:"value"`
	Values []string `json:"values"` // one per item (pasting a column)
	Facet  string   `json:"facet"`
	Name   string   `json:"name"`
	Reason string   `json:"reason"`
}

func (a *App) apiBatch(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ops []Op `json:"ops"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		jsonError(w, http.StatusBadRequest, "unlesbare Anfrage")
		return
	}
	u := currentUser(r)
	var changes []Change
	var stores []int64
	for _, op := range req.Ops {
		ch, st, err := a.opChanges(u, op)
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		changes = append(changes, ch...)
		stores = append(stores, st...)
	}
	if len(changes) == 0 && len(stores) == 0 {
		writeJSON(w, map[string]any{"batch": "", "changes": 0})
		return
	}
	batch := ""
	if len(changes) > 0 {
		var err error
		if batch, err = a.cat.Apply(u.ID, changes); err != nil {
			var ce *ConflictError
			msg := err.Error()
			if errors.As(err, &ce) {
				msg = "In der Zwischenzeit hat " + a.names.Name(ce.User) + " das geändert. Die Ansicht wird neu geladen."
			}
			jsonError(w, http.StatusConflict, msg)
			a.events.publish(BrowserEvent{Targets: targetsOf(changes)})
			return
		}
		a.cat.refreshFTS(changes)
	}
	for _, id := range stores {
		if err := a.storeItem(u.ID, id); err != nil {
			jsonError(w, http.StatusInternalServerError, fmt.Sprintf("A%d nach objekte/: %v", id, err))
			return
		}
	}
	ev := BrowserEvent{Batch: batch, User: u.ID, Targets: targetsOf(changes)}
	for _, id := range stores {
		ev.Targets = append(ev.Targets, targetOf("item", id))
	}
	a.events.publish(ev)
	writeJSON(w, map[string]any{"batch": batch, "changes": len(changes) + len(stores)})
}

func targetsOf(changes []Change) []string {
	seen := map[string]bool{}
	var out []string
	for _, ch := range changes {
		for _, t := range []string{ch.Target} {
			if !seen[t] {
				seen[t] = true
				out = append(out, t)
			}
		}
		for _, k := range []string{"tag", "into", "new", "old", "parent"} {
			if n := num64(ch.Data, k); n != 0 && (k != "tag" || strings.HasPrefix(ch.Op, "add") || strings.HasPrefix(ch.Op, "remove")) {
				t := targetOf("tag", n)
				if !seen[t] {
					seen[t] = true
					out = append(out, t)
				}
			}
		}
	}
	return out
}

// opChanges turns a browser operation into catalog changes; items to move
// into objekte/ come back separately.
func (a *App) opChanges(u *User, op Op) ([]Change, []int64, error) {
	rev := func(i int) int {
		if i < len(op.Revs) {
			return op.Revs[i]
		}
		return -1
	}
	item := func(id int64) string { return targetOf("item", id) }
	var out []Change
	switch op.Op {
	case "move", "add":
		if op.To == 0 {
			return nil, nil, fmt.Errorf("Ziel fehlt")
		}
		for i, id := range op.Items {
			out = append(out, Change{Target: item(id), Op: "add-tag", Data: map[string]any{"tag": op.To, "by": u.ID}, Rev: rev(i)})
			if op.Op == "move" && op.From != 0 && op.From != op.To {
				out = append(out, Change{Target: item(id), Op: "remove-tag", Data: map[string]any{"tag": op.From}, Rev: -1})
			}
		}
	case "remove":
		for i, id := range op.Items {
			out = append(out, Change{Target: item(id), Op: "remove-tag", Data: map[string]any{"tag": op.From}, Rev: rev(i)})
		}
	case "set":
		if !itemFields[op.Field] {
			return nil, nil, fmt.Errorf("Feld %q", op.Field)
		}
		for i, id := range op.Items {
			v := op.Value
			if i < len(op.Values) {
				v = op.Values[i]
			}
			var cur string
			a.cat.QueryRow("SELECT "+op.Field+" FROM items WHERE id = ?", id).Scan(&cur)
			if strings.TrimSpace(v) == cur {
				continue
			}
			out = append(out, Change{Target: item(id), Op: "set-field", Data: map[string]any{"field": op.Field, "old": cur, "new": strings.TrimSpace(v)}, Rev: rev(i)})
		}
	case "withdraw":
		for i, id := range op.Items {
			out = append(out, Change{Target: item(id), Op: "withdraw", Data: map[string]any{"reason": op.Reason}, Rev: rev(i), Reason: op.Reason})
		}
	case "new-folder":
		if !a.mayCreateTags(u) {
			return nil, nil, fmt.Errorf("neue Ordner legen Moderatoren an")
		}
		facet := op.Facet
		if op.To != 0 {
			a.cat.QueryRow("SELECT facet FROM tags WHERE id = ?", op.To).Scan(&facet)
		}
		if facet == "location" {
			return nil, nil, fmt.Errorf("der alte Ordnerbaum bleibt, wie er ist")
		}
		out = append(out, Change{Target: "tag:0", Op: "create-tag", Data: map[string]any{"facet": facet, "parent": op.To, "name": op.Name}, Rev: -1})
	case "rename-folder":
		var cur, facet string
		a.cat.QueryRow("SELECT name, facet FROM tags WHERE id = ?", op.Tag).Scan(&cur, &facet)
		if facet == "location" {
			return nil, nil, fmt.Errorf("der alte Ordnerbaum bleibt, wie er ist")
		}
		out = append(out, Change{Target: targetOf("tag", op.Tag), Op: "set-field", Data: map[string]any{"field": "name", "old": cur, "new": op.Name}, Rev: rev(0)})
	case "move-folder", "merge-folders":
		var f1, f2 string
		var parent sql.NullInt64
		a.cat.QueryRow("SELECT facet, parent FROM tags WHERE id = ?", op.Tag).Scan(&f1, &parent)
		if op.To != 0 {
			a.cat.QueryRow("SELECT facet FROM tags WHERE id = ?", op.To).Scan(&f2)
		} else {
			f2 = f1
		}
		if f1 == "location" || f1 != f2 {
			return nil, nil, fmt.Errorf("Ordner lassen sich nur innerhalb ihrer Facette verschieben")
		}
		if op.Op == "move-folder" {
			out = append(out, Change{Target: targetOf("tag", op.Tag), Op: "move-tag", Data: map[string]any{"old": parent.Int64, "new": op.To}, Rev: rev(0)})
		} else {
			out = append(out, Change{Target: targetOf("tag", op.Tag), Op: "merge-tag", Data: map[string]any{"into": op.To}, Rev: rev(0)})
		}
	case "store":
		return nil, op.Items, nil
	case "new-collection":
		out = append(out, Change{Target: "coll:0", Op: "create-coll", Data: map[string]any{"title": op.Name, "owner": u.ID}, Rev: -1})
	case "collect", "uncollect":
		o := map[string]string{"collect": "coll-add", "uncollect": "coll-remove"}[op.Op]
		for _, id := range op.Items {
			out = append(out, Change{Target: targetOf("coll", op.To), Op: o, Data: map[string]any{"item": id}, Rev: -1})
		}
	case "rename-collection":
		var cur string
		a.cat.QueryRow("SELECT title FROM collections WHERE id = ?", op.Tag).Scan(&cur)
		out = append(out, Change{Target: targetOf("coll", op.Tag), Op: "set-coll", Data: map[string]any{"field": "title", "old": cur, "new": op.Name}, Rev: rev(0)})
	default:
		return nil, nil, fmt.Errorf("unbekannte Operation %q", op.Op)
	}
	// within one item, the first change carries the revision
	seen := map[string]bool{}
	for i := range out {
		if seen[out[i].Target] {
			out[i].Rev = -1
		}
		seen[out[i].Target] = true
	}
	return out, nil, nil
}

func (a *App) apiUndo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Batch string `json:"batch"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	u := currentUser(r)
	rows, err := a.cat.Query("SELECT id, target FROM revisions WHERE batch = ? AND state = 'applied' ORDER BY id", req.Batch)
	if err != nil {
		jsonError(w, http.StatusInternalServerError, err.Error())
		return
	}
	var ids []int64
	var targets []string
	for rows.Next() {
		var id int64
		var t string
		rows.Scan(&id, &t)
		ids = append(ids, id)
		targets = append(targets, t)
	}
	rows.Close()
	if len(ids) == 0 {
		jsonError(w, http.StatusNotFound, "Nichts rückgängig zu machen.")
		return
	}
	if err := a.cat.Revert(u.ID, ids, "rückgängig im Archiv-Browser"); err != nil {
		jsonError(w, http.StatusConflict, "Das lässt sich nicht mehr rückgängig machen: "+err.Error())
		return
	}
	var changes []Change
	for _, t := range targets {
		changes = append(changes, Change{Target: t})
	}
	a.cat.refreshFTS(changes)
	a.events.publish(BrowserEvent{User: u.ID, Targets: targets})
	writeJSON(w, map[string]any{"undone": len(ids)})
}

// Live updates

type BrowserEvent struct {
	Batch   string   `json:"batch,omitempty"`
	User    string   `json:"user,omitempty"`
	Targets []string `json:"targets"`
}

type eventHub struct {
	mu   sync.Mutex
	subs map[chan BrowserEvent]bool
}

func newEventHub() *eventHub { return &eventHub{subs: map[chan BrowserEvent]bool{}} }

func (h *eventHub) publish(e BrowserEvent) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // a slow browser misses it and reloads on its next action
		}
	}
}

func (h *eventHub) subscribe() chan BrowserEvent {
	ch := make(chan BrowserEvent, 16)
	h.mu.Lock()
	h.subs[ch] = true
	h.mu.Unlock()
	return ch
}

func (h *eventHub) unsubscribe(ch chan BrowserEvent) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

func (a *App) apiEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "kein Streaming", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	ch := a.events.subscribe()
	defer a.events.unsubscribe(ch)
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	fmt.Fprint(w, ": hallo\n\n")
	fl.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			b, _ := json.Marshal(e)
			fmt.Fprintf(w, "data: %s\n\n", b)
			fl.Flush()
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// apiUpload takes files dropped from the desktop into the browser: each
// becomes an item in objekte/, tagged with the node it was dropped on. The
// upload streams to the share; no moderation round, since only moderators
// can do this.
func (a *App) apiUpload(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	node := r.URL.Query().Get("node")
	var tag int64
	if strings.HasPrefix(node, "t:") {
		tag, _ = strconv.ParseInt(node[2:], 10, 64)
		var facet string
		if a.cat.QueryRow("SELECT facet FROM tags WHERE id = ?", tag).Scan(&facet) != nil || facet == "location" {
			jsonError(w, http.StatusBadRequest, "In den alten Ordnerbaum lässt sich nichts ablegen; bitte ein Schlagwort wählen.")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, a.cfg.MaxFile)
	mr, err := r.MultipartReader()
	if err != nil {
		jsonError(w, http.StatusBadRequest, "keine Dateien")
		return
	}
	var created []int64
	touched := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			jsonError(w, http.StatusBadRequest, err.Error())
			return
		}
		if part.FormName() != "file" || part.FileName() == "" {
			continue
		}
		id, dir, err := a.newDroppedItem(u.ID, part.FileName(), part, tag)
		part.Close()
		if err != nil {
			jsonError(w, http.StatusInternalServerError, part.FileName()+": "+err.Error())
			return
		}
		created = append(created, id)
		touched[dir] = true
	}
	var targets []string
	for _, id := range created {
		targets = append(targets, targetOf("item", id))
	}
	if tag != 0 {
		targets = append(targets, targetOf("tag", tag))
	}
	a.events.publish(BrowserEvent{User: u.ID, Targets: targets})
	// text and thumbnails in the background
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		a.refreshArchive(ctx, touched)
	}()
	writeJSON(w, map[string]any{"items": created})
}

func (a *App) newDroppedItem(user, name string, body io.Reader, tag int64) (int64, string, error) {
	name = cleanName(path.Base(strings.ReplaceAll(name, `\`, "/")))
	title := strings.TrimSuffix(name, path.Ext(name))
	res, err := a.cat.Exec("INSERT INTO items (title, state, created, updated, rev) VALUES (?, 'missing', ?, ?, 1)", title, now(), now())
	if err != nil {
		return 0, "", err
	}
	id, _ := res.LastInsertId()
	dir := storeDir(id)
	fail := func(err error) (int64, string, error) {
		os.RemoveAll(a.archivePath(dir))
		a.cat.Exec("DELETE FROM items WHERE id = ?", id)
		return 0, "", err
	}
	if err := os.MkdirAll(a.archivePath(dir), 0o2775); err != nil {
		return fail(err)
	}
	rel := dir + "/" + name
	tmp := a.archivePath(dir + "/." + name + ".part")
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o664)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, h), body)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fail(err)
	}
	if err := os.Rename(tmp, a.archivePath(rel)); err != nil {
		return fail(err)
	}
	st, err := os.Stat(a.archivePath(rel))
	if err != nil {
		return fail(err)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	tx, err := a.cat.Begin()
	if err != nil {
		return fail(err)
	}
	defer tx.Rollback()
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{"INSERT OR IGNORE INTO files (sha256, size, kind, added) VALUES (?, ?, ?, ?)", []any{sum, n, kindOf(name), now()}},
		{"INSERT OR REPLACE INTO file_paths (path, sha256, size, mtime, seen) VALUES (?, ?, ?, ?, ?)", []any{rel, sum, n, st.ModTime().Unix(), now()}},
		{"INSERT INTO item_files (item, sha256, name, path, ord) VALUES (?, ?, ?, ?, 1)", []any{id, sum, name, rel}},
		{"UPDATE items SET state = 'live' WHERE id = ?", []any{id}},
		{`INSERT INTO revisions (target, op, data, user, at, reason, state, batch, decided_by, decided_at)
		    VALUES (?, 'create-item', '{}', ?, ?, 'im Archiv-Browser abgelegt', 'applied', ?, ?, ?)`, []any{targetOf("item", id), user, now(), newBatchID(), user, now()}},
	} {
		if _, err := tx.Exec(q.sql, q.args...); err != nil {
			return fail(err)
		}
	}
	if tag != 0 {
		if _, err := tx.Exec("INSERT INTO item_tags (item, tag, added_by, added_at) VALUES (?, ?, ?, ?)", id, tag, user, now()); err != nil {
			return fail(err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fail(err)
	}
	a.cat.refreshFTS([]Change{{Target: targetOf("item", id)}})
	return id, dir, nil
}
