package archiv

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Editing on the site: the item form, comments and reports, the history of
// items and tags, and the moderators' queue of proposals and reports.

func (a *App) editRoutes(mux *http.ServeMux) {
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	page("POST /a/{id}/bearbeiten", a.post(a.editing(a.editItem)))
	page("POST /a/{id}/kommentar", a.post(a.editing(a.commentItem)))
	page("GET /a/{id}/verlauf", a.itemHistory)
	page("POST /schlagwort/{id}/bearbeiten", a.post(a.editing(a.editTag)))
	page("POST /schlagwort/{id}/kommentar", a.post(a.editing(a.commentTag)))
	page("POST /schlagwort/{id}/bild", a.editing(a.uploadPageImage))
	page("GET /schlagwort/{id}/verlauf", a.tagHistory)
	page("GET /aenderungen", a.recentChanges)
	page("GET /api/schlagworte", a.tagSuggest)
	page("GET /moderation", a.mod(a.queue))
	page("POST /moderation/vorschlaege", a.mod(a.post(a.editing(a.decideProposals))))
	page("POST /moderation/meldung", a.mod(a.post(a.editing(a.closeReport))))
	page("POST /moderation/zuruecknehmen", a.mod(a.post(a.editing(a.revertRevisions))))
}

// editing refuses changes while the catalog is read-only.
func (a *App) editing(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.cfg.Editing {
			a.renderSite(w, r, http.StatusForbidden, "site-error", &SitePage{Title: "Noch nicht freigeschaltet",
				Data: "Der Katalog kann bearbeitet werden, sobald die alte Seite abgeschaltet ist."})
			return
		}
		h(w, r)
	}
}

// mayEdit tells whether the user's changes apply directly.
func (a *App) mayEdit(u *User) bool { return u != nil && (u.Moderator || u.inGroup(a.cfg.EditGroup)) }

func (a *App) mayCreateTags(u *User) bool {
	return u != nil && (u.Moderator || u.inGroup(a.cfg.TagCreateGrp))
}

// commit applies the changes or proposes them, by the user's rights, and
// says what happened.
func (a *App) commit(w http.ResponseWriter, r *http.Request, changes []Change, back string) {
	u := currentUser(r)
	if len(changes) == 0 {
		http.Redirect(w, r, back+"?ok=nichts", http.StatusSeeOther)
		return
	}
	var err error
	note := "gespeichert"
	if a.mayEdit(u) {
		_, err = a.cat.Apply(u.ID, changes)
	} else {
		_, err = a.cat.Propose(u.ID, changes)
		note = "vorgeschlagen"
	}
	if err != nil {
		a.editFailed(w, r, err, back)
		return
	}
	if err := a.cat.refreshFTS(changes); err != nil {
		logf("full text after edit: %v", err)
	}
	http.Redirect(w, r, back+"?ok="+note, http.StatusSeeOther)
}

func (a *App) editFailed(w http.ResponseWriter, r *http.Request, err error, back string) {
	var ce *ConflictError
	msg := "Das ließ sich nicht speichern."
	switch {
	case errors.As(err, &ce):
		msg = "In der Zwischenzeit hat " + a.names.Name(ce.User) + " das geändert (" + fmtTime(ce.At) + "). Bitte die Seite neu laden und die Änderung noch einmal machen."
	case errors.Is(err, errConflict):
		msg = "Das wurde in der Zwischenzeit geändert. Bitte die Seite neu laden."
	case errors.Is(err, errInvalid):
		msg = "Das geht so nicht: " + strings.TrimPrefix(err.Error(), errInvalid.Error()+": ")
	default:
		logf("edit: %v", err)
	}
	a.renderSite(w, r, http.StatusConflict, "site-error", &SitePage{Title: "Nicht gespeichert", Data: msg, Query: back})
}

// refreshFTS brings the full text of the changed items up to date.
func (c *Catalog) refreshFTS(changes []Change) error {
	seen := map[int64]bool{}
	for _, ch := range changes {
		kind, id := splitTarget(ch.Target)
		if kind != "item" || seen[id] {
			continue
		}
		seen[id] = true
		if into := num64(ch.Data, "into"); ch.Op == "merge" && into != 0 {
			seen[into] = false
		}
	}
	for id := range seen {
		if _, err := c.Exec("DELETE FROM items_fts WHERE rowid = ?", id); err != nil {
			return err
		}
		if _, err := c.Exec(`INSERT INTO items_fts (rowid, title, descr, tags, names)
		    SELECT i.id, i.title, i.descr,
		        COALESCE((SELECT group_concat(t.name, ' ') FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = i.id), ''),
		        COALESCE((SELECT group_concat(f.name, ' ') FROM item_files f WHERE f.item = i.id), '')
		    FROM items i WHERE i.id = ? AND i.state = 'live'`, id); err != nil {
			return err
		}
	}
	return nil
}

// editItem turns the item form into changes.
func (a *App) editItem(w http.ResponseWriter, r *http.Request) {
	id, ok := a.itemID(w, r)
	if !ok {
		return
	}
	it, err := a.loadItem(id)
	if err != nil {
		a.notFound(w, r, "")
		return
	}
	back := "/a/" + strconv.FormatInt(id, 10)
	rev, _ := strconv.Atoi(r.FormValue("rev"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	target := targetOf("item", id)
	var changes []Change
	add := func(op string, d map[string]any) {
		changes = append(changes, Change{Target: target, Op: op, Data: d, Rev: rev, Reason: reason})
	}
	for _, f := range []struct{ name, cur string }{{"title", it.Title}, {"descr", it.Descr}, {"year", it.Year}} {
		if v, ok := r.Form[f.name]; ok {
			nv := strings.TrimSpace(strings.ReplaceAll(v[0], "\r\n", "\n"))
			if nv != f.cur {
				add("set-field", map[string]any{"field": f.name, "old": f.cur, "new": nv})
			}
		}
	}
	u := currentUser(r)
	for _, v := range r.Form["tag_remove"] {
		if t, err := strconv.ParseInt(v, 10, 64); err == nil {
			add("remove-tag", map[string]any{"tag": t})
		}
	}
	for _, v := range r.Form["tag_add"] {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if name, ok := groupName(v); ok {
			t, err := a.itemGroup(u, it, name)
			if err != nil {
				a.editFailed(w, r, err, back)
				return
			}
			add("add-tag", map[string]any{"tag": t, "by": u.ID})
			continue
		}
		t, err := a.resolveTag(v)
		if err == sql.ErrNoRows && a.mayCreateTags(u) {
			t, err = a.createTagPath(u.ID, v)
		}
		if err != nil {
			a.editFailed(w, r, fmt.Errorf("%w: das Schlagwort „%s“ gibt es nicht", errInvalid, v), back)
			return
		}
		add("add-tag", map[string]any{"tag": t, "by": u.ID})
	}
	if k, other := r.FormValue("rel_kind"), strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(r.FormValue("rel_item"))), "A"); k != "" && other != "" {
		n, err := strconv.ParseInt(other, 10, 64)
		if err != nil {
			a.editFailed(w, r, fmt.Errorf("%w: „%s“ ist keine Objektnummer", errInvalid, other), back)
			return
		}
		add("add-rel", map[string]any{"kind": k, "item": n})
	}
	for _, v := range r.Form["rel_remove"] {
		k, n, _ := strings.Cut(v, ":")
		if b, err := strconv.ParseInt(n, 10, 64); err == nil {
			add("remove-rel", map[string]any{"kind": k, "item": b})
		}
	}
	for _, f := range it.Files {
		k := strconv.Itoa(f.N)
		if v := r.FormValue("role_" + k); v != "" && v != f.Role {
			add("set-role", map[string]any{"name": f.Name, "old": f.Role, "new": v})
		}
		name := f.Name
		if v, ok := r.Form["fname_"+k]; ok && strings.TrimSpace(v[0]) != "" && strings.TrimSpace(v[0]) != f.Name {
			name = strings.TrimSpace(v[0])
			add("rename-file", map[string]any{"old": f.Name, "new": name})
		}
		switch r.FormValue("fact_" + k) {
		case "split":
			add("split-file", map[string]any{"name": name})
		case "move":
			into := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(r.FormValue("finto_"+k))), "A")
			n, err := strconv.ParseInt(into, 10, 64)
			if err != nil {
				a.editFailed(w, r, fmt.Errorf("%w: „%s“ ist keine Objektnummer", errInvalid, r.FormValue("finto_"+k)), back)
				return
			}
			add("split-file", map[string]any{"name": name, "into": n})
		case "remove":
			add("remove-file", map[string]any{"name": name, "reason": reason})
		}
	}
	if into := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(r.FormValue("merge_into"))), "A"); into != "" {
		n, err := strconv.ParseInt(into, 10, 64)
		if err != nil {
			a.editFailed(w, r, fmt.Errorf("%w: „%s“ ist keine Objektnummer", errInvalid, into), back)
			return
		}
		add("merge", map[string]any{"into": n})
		back = "/a/" + into
	}
	if r.FormValue("withdraw") != "" {
		add("withdraw", map[string]any{"reason": reason})
	}
	if r.FormValue("restore") != "" && it.State == "withdrawn" {
		add("restore", map[string]any{})
	}
	// changes after the first are based on the revision the first one makes
	for i := range changes {
		if i > 0 {
			changes[i].Rev = -1
		}
	}
	a.commit(w, r, changes, back)
}

// groupName is the path of "Gruppe: A › B", if v names a group.
func groupName(v string) (string, bool) {
	f, rest, ok := strings.Cut(v, ":")
	f = strings.TrimSpace(f)
	if !ok || !strings.EqualFold(f, facetLabels["group"]) && !strings.EqualFold(f, "group") || strings.TrimSpace(rest) == "" {
		return "", false
	}
	return strings.TrimSpace(rest), true
}

// itemGroup finds the group of that name below the item's machine,
// magazine or meeting, or creates it for a user who may create tags. A
// group belongs to one machine, magazine or meeting: "Peripherie" of the
// PDP-11 is not that of the VAX.
func (a *App) itemGroup(u *User, it *ItemView, name string) (int64, error) {
	scope := it.treeTag()
	if scope == nil {
		return 0, fmt.Errorf("%w: eine Gruppe gehört zu einem Rechner, einer Zeitschrift oder einem Treffen; das Objekt hat keines", errInvalid)
	}
	var parent int64
	for _, n := range splitTagPath(name) {
		var id int64
		err := a.cat.QueryRow(`SELECT id FROM tags WHERE facet = 'group' AND scope = ? AND COALESCE(parent, 0) = ? AND merged_into IS NULL AND lower(name) = lower(?)`,
			scope.ID, parent, n).Scan(&id)
		if err == nil {
			parent = id
			continue
		}
		if !a.mayCreateTags(u) {
			return 0, fmt.Errorf("%w: die Gruppe „%s“ gibt es unter %s nicht, und neue legen Moderatoren an", errInvalid, n, scope.Name)
		}
		ch := Change{Target: "tag:0", Op: "create-tag", Data: map[string]any{"facet": "group", "parent": parent, "scope": scope.ID, "name": n}, Rev: -1}
		if _, err := a.cat.Apply(u.ID, []Change{ch}); err != nil {
			return 0, err
		}
		parent = num64(ch.Data, "id")
	}
	return parent, nil
}

// resolveTag finds a tag by "Facet: Name › Name" or "Name › Name" (any
// facet, if unique), as the autocompletion writes them.
func (a *App) resolveTag(s string) (int64, error) {
	facet := ""
	if f, rest, ok := strings.Cut(s, ":"); ok {
		for k, l := range facetLabels {
			if strings.EqualFold(strings.TrimSpace(f), l) || strings.EqualFold(strings.TrimSpace(f), k) {
				facet, s = k, rest
			}
		}
	}
	names := splitTagPath(s)
	q := "SELECT id FROM tags WHERE facet != 'location' AND merged_into IS NULL AND parent IS NULL AND lower(name) = lower(?)"
	args := []any{names[0]}
	if facet != "" {
		q += " AND facet = ?"
		args = append(args, facet)
	}
	rows, err := a.cat.Query(q, args...)
	if err != nil {
		return 0, err
	}
	var cands []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		cands = append(cands, id)
	}
	rows.Close()
	var found []int64
	for _, c := range cands {
		id := c
		for _, n := range names[1:] {
			// a tag merged into another stands for that one
			if a.cat.QueryRow(`SELECT id FROM tags WHERE parent = ? AND lower(name) = lower(?) ORDER BY merged_into IS NOT NULL LIMIT 1`, id, n).Scan(&id) != nil {
				id = 0
				break
			}
			for k := 0; k < 8; k++ {
				var to sql.NullInt64
				if a.cat.QueryRow("SELECT merged_into FROM tags WHERE id = ?", id).Scan(&to) != nil || !to.Valid {
					break
				}
				id = to.Int64
			}
		}
		if id != 0 {
			found = append(found, id)
		}
	}
	slices.Sort(found)
	found = slices.Compact(found)
	if len(found) != 1 {
		return 0, sql.ErrNoRows
	}
	return found[0], nil
}

func splitTagPath(s string) []string {
	s = strings.ReplaceAll(s, " > ", " › ")
	var out []string
	for _, p := range strings.Split(s, "›") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		out = []string{""}
	}
	return out
}

// createTagPath creates the missing levels of "Facet: A › B" for a user
// who may create tags.
func (a *App) createTagPath(user, s string) (int64, error) {
	f, rest, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("%w: ein neues Schlagwort braucht seine Facette, etwa „Rechner: Hersteller › Modell“", errInvalid)
	}
	facet := ""
	for k, l := range facetLabels {
		if strings.EqualFold(strings.TrimSpace(f), l) || strings.EqualFold(strings.TrimSpace(f), k) {
			facet = k
		}
	}
	if facet == "" || facet == "location" {
		return 0, fmt.Errorf("%w: Facette %q", errInvalid, f)
	}
	var parent int64
	for _, n := range splitTagPath(rest) {
		var id int64
		if a.cat.QueryRow("SELECT id FROM tags WHERE facet = ? AND COALESCE(parent, 0) = ? AND merged_into IS NULL AND lower(name) = lower(?)", facet, parent, n).Scan(&id) == nil {
			parent = id
			continue
		}
		ch := Change{Target: "tag:0", Op: "create-tag", Data: map[string]any{"facet": facet, "parent": parent, "name": n}, Rev: -1}
		if _, err := a.cat.Apply(user, []Change{ch}); err != nil {
			return 0, err
		}
		parent = num64(ch.Data, "id")
	}
	return parent, nil
}

// tagSuggest answers the autocompletion of tags: JSON list of "Facet: Path".
func (a *App) tagSuggest(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	facet := r.URL.Query().Get("facet")
	var out []map[string]any
	if len([]rune(q)) >= 2 {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
		args := []any{like}
		sqlq := `SELECT id FROM tags WHERE facet NOT IN ('location', 'group') AND merged_into IS NULL AND name LIKE ? ESCAPE '\'`
		if facet != "" {
			sqlq += " AND facet = ?"
			args = append(args, facet)
		}
		rows, err := a.cat.Query(sqlq+" ORDER BY length(name) LIMIT 20", args...)
		if err == nil {
			var ids []int64
			for rows.Next() {
				var id int64
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
			for _, id := range ids {
				if t := a.tagRef(id); t != nil {
					out = append(out, map[string]any{"id": t.ID, "facet": t.Facet, "label": facetLabels[t.Facet] + ": " + t.Path})
				}
			}
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// Comments and reports

var commentKinds = map[string]string{
	"comment": "Kommentar", "broken": "Datei kaputt", "missing-pages": "Seiten fehlen",
	"wrong-descr": "Beschreibung falsch", "duplicate": "Dublette", "elsewhere": "gehört woanders hin",
	"copyright": "Urheberrecht",
}

type CommentView struct {
	ID       int64
	Parent   int64
	User     string
	At       int64
	Kind     string
	Body     string
	State    string
	Replies  []*CommentView
	IsReport bool
}

func (a *App) comments(target string) []*CommentView {
	rows, err := a.cat.Query("SELECT id, COALESCE(parent, 0), user, at, kind, body, state FROM comments WHERE target = ? ORDER BY id", target)
	if err != nil {
		return nil
	}
	defer rows.Close()
	byID := map[int64]*CommentView{}
	var top []*CommentView
	for rows.Next() {
		c := &CommentView{}
		rows.Scan(&c.ID, &c.Parent, &c.User, &c.At, &c.Kind, &c.Body, &c.State)
		c.IsReport = c.Kind != "comment"
		byID[c.ID] = c
		if p := byID[c.Parent]; p != nil {
			p.Replies = append(p.Replies, c)
		} else {
			top = append(top, c)
		}
	}
	return top
}

func (a *App) addComment(w http.ResponseWriter, r *http.Request, target, back string) {
	u := currentUser(r)
	body := strings.TrimSpace(r.FormValue("body"))
	kind := r.FormValue("kind")
	if commentKinds[kind] == "" {
		kind = "comment"
	}
	if body == "" {
		a.editFailed(w, r, fmt.Errorf("%w: der Text fehlt", errInvalid), back)
		return
	}
	if len(body) > 20000 {
		body = body[:20000]
	}
	parent, _ := strconv.ParseInt(r.FormValue("parent"), 10, 64)
	res, err := a.cat.Exec("INSERT INTO comments (target, parent, user, at, kind, body) VALUES (?, NULLIF(?, 0), ?, ?, ?, ?)",
		target, parent, u.ID, now(), kind, body)
	if err != nil {
		a.editFailed(w, r, err, back)
		return
	}
	id, _ := res.LastInsertId()
	if parent != 0 {
		a.notifyReply(parent, id, back)
	}
	http.Redirect(w, r, back+"?ok=kommentiert#k"+strconv.FormatInt(id, 10), http.StatusSeeOther)
}

func (a *App) commentItem(w http.ResponseWriter, r *http.Request) {
	id, ok := a.itemID(w, r)
	if !ok {
		return
	}
	a.addComment(w, r, targetOf("item", id), "/a/"+strconv.FormatInt(id, 10))
}

func (a *App) commentTag(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ref := a.tagRef(id)
	if ref == nil {
		a.notFound(w, r, "")
		return
	}
	a.addComment(w, r, targetOf("tag", id), ref.Href)
}

// editTag changes a tag: name, description, page text, place in the tree,
// merging into another, and new tags below it. A folder only takes a name,
// a description and a page text; its place is that of the folder on disk.
func (a *App) editTag(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	f, ok := a.filterOf(id)
	if !ok {
		a.notFound(w, r, "")
		return
	}
	ref := f.Ref
	back := a.viewURL([]Filter{f})
	if ref.Facet == "location" {
		for _, k := range []string{"move_under", "merge_into", "new_child"} {
			delete(r.Form, k)
		}
	}
	var name, descr, page string
	var parent sql.NullInt64
	a.cat.QueryRow("SELECT name, descr, parent FROM tags WHERE id = ?", id).Scan(&name, &descr, &parent)
	a.cat.QueryRow("SELECT body FROM tag_pages WHERE tag = ?", id).Scan(&page)
	rev, _ := strconv.Atoi(r.FormValue("rev"))
	reason := strings.TrimSpace(r.FormValue("reason"))
	target := targetOf("tag", id)
	var changes []Change
	add := func(t, op string, d map[string]any) {
		changes = append(changes, Change{Target: t, Op: op, Data: d, Rev: -1, Reason: reason})
	}
	if v, ok := r.Form["name"]; ok && strings.TrimSpace(v[0]) != name {
		add(target, "set-field", map[string]any{"field": "name", "old": name, "new": strings.TrimSpace(v[0])})
	}
	if v, ok := r.Form["descr"]; ok && strings.TrimSpace(v[0]) != descr {
		add(target, "set-field", map[string]any{"field": "descr", "old": descr, "new": strings.TrimSpace(v[0])})
	}
	if v, ok := r.Form["page"]; ok {
		nv := strings.TrimSpace(strings.ReplaceAll(v[0], "\r\n", "\n"))
		if nv != page {
			add(targetOf("page", id), "set-page", map[string]any{"old": page, "new": nv})
		}
	}
	u := currentUser(r)
	if v := strings.TrimSpace(r.FormValue("move_under")); v != "" {
		np := int64(0)
		if v != "-" {
			t, err := a.resolveTag(facetLabels[ref.Facet] + ": " + v)
			if err != nil {
				a.editFailed(w, r, fmt.Errorf("%w: „%s“ gibt es nicht", errInvalid, v), back)
				return
			}
			np = t
		}
		add(target, "move-tag", map[string]any{"old": parent.Int64, "new": np})
	}
	if v := strings.TrimSpace(r.FormValue("merge_into")); v != "" {
		t, err := a.resolveTag(facetLabels[ref.Facet] + ": " + v)
		if err != nil || t == id {
			a.editFailed(w, r, fmt.Errorf("%w: „%s“ gibt es nicht", errInvalid, v), back)
			return
		}
		add(target, "merge-tag", map[string]any{"into": t})
		if mf, ok := a.filterOf(t); ok {
			back = a.viewURL([]Filter{mf})
		}
	}
	if v := strings.TrimSpace(r.FormValue("new_child")); v != "" {
		if !a.mayCreateTags(u) {
			a.editFailed(w, r, fmt.Errorf("%w: neue Schlagworte legen Moderatoren an", errInvalid), back)
			return
		}
		add("tag:0", "create-tag", map[string]any{"facet": ref.Facet, "parent": id, "name": v})
	}
	if len(changes) > 0 && changes[0].Target == target {
		changes[0].Rev = rev
	}
	a.commit(w, r, changes, back)
}

// History

type RevView struct {
	Gather       *GatherView // what a proposal to gather items makes
	data         map[string]any
	ID           int64
	Target       string
	TargetLabel  string
	TargetHref   string
	Op           string
	What         string
	User, Reason string
	At           int64
	State        string
	Batch        string
	DecidedBy    string
}

// GatherView shows what a proposal to gather items into one would make:
// the item, its files in their order, and which become its cover.
type GatherView struct {
	Item   int64
	Title  string
	Items  int // the items that go into it, itself included
	Others int // those that go into it
	Total  int // their files
	Covers bool
	Files  []GatherFile // the first ones
	More   int
}

type GatherFile struct {
	Item  int64
	Name  string
	Thumb string
	Ext   string
	Cover bool
}

// gatherShown bounds the files a proposal shows.
const gatherShown = 24

func (a *App) gatherView(target int64, d map[string]any) *GatherView {
	g := &GatherView{Item: target, Title: str(d, "title"), Covers: d["covers"] == true}
	if g.Title == "" {
		a.cat.QueryRow("SELECT title FROM items WHERE id = ?", target).Scan(&g.Title)
	}
	for _, id := range append([]int64{target}, nums(d, "items")...) {
		g.Items++
		// read the files before looking up their pictures: the catalog has
		// one connection
		rows, err := a.cat.Query("SELECT name, path FROM item_files WHERE item = ? ORDER BY ord, name", id)
		if err != nil {
			continue
		}
		var files [][2]string
		for rows.Next() {
			var name, p string
			rows.Scan(&name, &p)
			files = append(files, [2]string{name, p})
		}
		rows.Close()
		for _, np := range files {
			name, p := np[0], np[1]
			g.Total++
			if len(g.Files) >= gatherShown {
				continue
			}
			f := GatherFile{Item: id, Name: name, Ext: strings.ToUpper(strings.TrimPrefix(path.Ext(name), ".")),
				Cover: g.Covers && imageExt.MatchString(name)}
			if e := extOf(name); imageExts["."+e] || e == "pdf" {
				f.Thumb = a.thumbFor("thumbs", p)
			}
			g.Files = append(g.Files, f)
		}
	}
	g.More = g.Total - len(g.Files)
	g.Others = g.Items - 1
	return g
}

func (a *App) revisions(where string, args []any, limit int) []RevView {
	rows, err := a.cat.Query(`SELECT id, target, op, data, user, at, reason, state, COALESCE(batch, ''), COALESCE(decided_by, '')
	    FROM revisions WHERE `+where+` ORDER BY id DESC LIMIT `+strconv.Itoa(limit), args...)
	if err != nil {
		return nil
	}
	var out []RevView
	type raw struct {
		RevView
		raw string
	}
	var rs []raw
	for rows.Next() {
		var x raw
		rows.Scan(&x.ID, &x.Target, &x.Op, &x.raw, &x.User, &x.At, &x.Reason, &x.State, &x.Batch, &x.DecidedBy)
		rs = append(rs, x)
	}
	rows.Close()
	for _, x := range rs {
		var d map[string]any
		json.Unmarshal([]byte(x.raw), &d)
		x.What = a.describeChange(x.Op, d)
		kind, id := splitTarget(x.Target)
		x.data = d
		switch kind {
		case "item":
			var t string
			a.cat.QueryRow("SELECT title FROM items WHERE id = ?", id).Scan(&t)
			x.TargetLabel, x.TargetHref = "A"+strconv.FormatInt(id, 10)+" "+t, "/a/"+strconv.FormatInt(id, 10)
		case "tag", "page":
			if t := a.tagRef(id); t != nil {
				x.TargetLabel, x.TargetHref = facetLabels[t.Facet]+": "+t.Path, t.Href
			}
		case "coll":
			var t string
			a.cat.QueryRow("SELECT title FROM collections WHERE id = ?", id).Scan(&t)
			x.TargetLabel, x.TargetHref = "Sammlung: "+t, "/s/"+strconv.FormatInt(id, 10)
		}
		out = append(out, x.RevView)
	}
	return out
}

var roleLabels = map[string]string{"main": "Hauptdatei", "part": "Teil", "cover": "Titelbild", "extra": "Beigabe"}
var revStates = map[string]string{"applied": "", "proposed": "vorgeschlagen", "rejected": "abgelehnt", "reverted": "zurückgenommen"}

var fieldLabels = map[string]string{"title": "Titel", "descr": "Beschreibung", "year": "Jahr", "name": "Name"}

// describeChange says in words what a revision does.
func (a *App) describeChange(op string, d map[string]any) string {
	tag := func(k string) string {
		if t := a.tagRef(num64(d, k)); t != nil {
			return facetLabels[t.Facet] + ": " + t.Path
		}
		return "Schlagwort " + str(d, k)
	}
	short := func(s string) string {
		if r := []rune(s); len(r) > 120 {
			return string(r[:120]) + "…"
		}
		return s
	}
	switch op {
	case "set-field":
		return fmt.Sprintf("%s: „%s“ → „%s“", fieldLabels[str(d, "field")], short(str(d, "old")), short(str(d, "new")))
	case "add-tag":
		return "Schlagwort dazu: " + tag("tag")
	case "add-tag-path":
		if num64(d, "tag") != 0 {
			return "Schlagwort dazu: " + tag("tag")
		}
		return "Schlagwort dazu: " + facetLabels[str(d, "facet")] + ": " + strings.ReplaceAll(str(d, "path"), " > ", " › ") + " (neu)"
	case "remove-tag":
		return "Schlagwort weg: " + tag("tag")
	case "add-rel":
		return relKinds[str(d, "kind")] + " A" + str(d, "item")
	case "remove-rel":
		return "nicht mehr: " + relKinds[str(d, "kind")] + " A" + str(d, "item")
	case "set-role":
		return fmt.Sprintf("Rolle von %s: %s → %s", str(d, "name"), str(d, "old"), str(d, "new"))
	case "merge":
		return "zusammengelegt mit A" + str(d, "into")
	case "rename-file":
		return fmt.Sprintf("Datei „%s“ heißt jetzt „%s“", str(d, "old"), str(d, "new"))
	case "split-file":
		if into := num64(d, "into"); into != 0 {
			return fmt.Sprintf("Datei „%s“ nach A%d verschoben", str(d, "name"), into)
		}
		return fmt.Sprintf("Datei „%s“ als eigenes Objekt A%d abgetrennt", str(d, "name"), num64(d, "id"))
	case "remove-file":
		return fmt.Sprintf("Datei „%s“ entfernt: %s (zurückgezogen als A%d)", str(d, "name"), str(d, "reason"), num64(d, "id"))
	case "gather":
		if t := str(d, "title"); t != "" {
			return fmt.Sprintf("fasst %d weitere Objekte darin zusammen: „%s“", len(nums(d, "items")), t)
		}
		return fmt.Sprintf("fasst %d weitere Objekte darin zusammen", len(nums(d, "items")))
	case "withdraw":
		return "zurückgezogen: " + str(d, "reason")
	case "restore":
		return "wiederhergestellt"
	case "create-tag":
		return "neues Schlagwort " + str(d, "name")
	case "move-tag":
		if num64(d, "new") == 0 {
			return "nach oben verschoben"
		}
		return "verschoben unter " + tag("new")
	case "merge-tag":
		return "zusammengelegt mit " + tag("into")
	case "set-page":
		return "Seitentext geändert"
	case "create-coll":
		return "neue Sammlung " + str(d, "title")
	case "coll-add":
		return "in die Sammlung: A" + str(d, "item")
	case "coll-remove":
		return "aus der Sammlung: A" + str(d, "item")
	case "set-coll":
		return fmt.Sprintf("Sammlung, %s: „%s“ → „%s“", map[string]string{"title": "Name", "intro": "Einleitung"}[str(d, "field")], short(str(d, "old")), short(str(d, "new")))
	}
	return op
}

type HistoryView struct {
	Label, Href string
	Revs        []RevView
	Revert      bool
}

func (a *App) itemHistory(w http.ResponseWriter, r *http.Request) {
	id, ok := a.itemID(w, r)
	if !ok {
		return
	}
	var title string
	if a.cat.QueryRow("SELECT title FROM items WHERE id = ?", id).Scan(&title) != nil {
		a.notFound(w, r, "")
		return
	}
	h := &HistoryView{Label: title, Href: "/a/" + strconv.FormatInt(id, 10), Revert: currentUser(r).Moderator && a.cfg.Editing}
	h.Revs = a.revisions("target = ?", []any{targetOf("item", id)}, 500)
	a.renderSite(w, r, http.StatusOK, "site-history", &SitePage{Title: "Verlauf: " + title,
		Crumbs: []Crumb{{"/", "Start"}, {h.Href, "A" + strconv.FormatInt(id, 10)}, {h.Href + "/verlauf", "Verlauf"}}, Data: h})
}

func (a *App) tagHistory(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ref := a.tagRef(id)
	if ref == nil {
		a.notFound(w, r, "")
		return
	}
	h := &HistoryView{Label: ref.Path, Href: ref.Href, Revert: currentUser(r).Moderator && a.cfg.Editing}
	h.Revs = a.revisions("target IN (?, ?)", []any{targetOf("tag", id), targetOf("page", id)}, 500)
	a.renderSite(w, r, http.StatusOK, "site-history", &SitePage{Title: "Verlauf: " + ref.Name,
		Crumbs: []Crumb{{"/", "Start"}, {ref.Href, ref.Name}, {"", "Verlauf"}}, Data: h})
}

func (a *App) recentChanges(w http.ResponseWriter, r *http.Request) {
	h := &HistoryView{Label: "Letzte Änderungen", Revert: currentUser(r).Moderator && a.cfg.Editing}
	h.Revs = a.revisions("state IN ('applied', 'reverted')", nil, 300)
	a.renderSite(w, r, http.StatusOK, "site-history", &SitePage{Title: "Letzte Änderungen",
		Crumbs: []Crumb{{"/", "Start"}, {"/aenderungen", "Letzte Änderungen"}}, Data: h})
}

// Moderation queue

type QueueBatch struct {
	Batch string
	User  string
	At    int64
	Revs  []RevView
}

type ReportView struct {
	CommentView
	TargetLabel, TargetHref string
}

type QueueView struct {
	Batches []QueueBatch // those of the page
	Total   int          // all open batches
	Page    int
	Pages   []int
	Reports []ReportView
	Editing bool
}

// queuePage bounds the proposals a page of the queue shows, with what
// each would make.
const queuePage = 20

func (a *App) queue(w http.ResponseWriter, r *http.Request) {
	q := &QueueView{Editing: a.cfg.Editing}
	for _, rv := range a.revisions("state = 'proposed'", nil, 1000) {
		if n := len(q.Batches); n > 0 && q.Batches[n-1].Batch == rv.Batch {
			q.Batches[n-1].Revs = append(q.Batches[n-1].Revs, rv)
			continue
		}
		q.Batches = append(q.Batches, QueueBatch{Batch: rv.Batch, User: rv.User, At: rv.At, Revs: []RevView{rv}})
	}
	q.Total = len(q.Batches)
	q.Page, _ = strconv.Atoi(r.URL.Query().Get("seite"))
	pages := (q.Total + queuePage - 1) / queuePage
	q.Page = max(1, min(q.Page, pages))
	for i := 1; i <= pages && pages > 1; i++ {
		q.Pages = append(q.Pages, i)
	}
	if q.Total > 0 {
		q.Batches = q.Batches[(q.Page-1)*queuePage : min(q.Page*queuePage, q.Total)]
	}
	for _, b := range q.Batches {
		for i := range b.Revs {
			if rv := &b.Revs[i]; rv.Op == "gather" {
				_, id := splitTarget(rv.Target)
				rv.Gather = a.gatherView(id, rv.data)
			}
		}
	}
	rows, err := a.cat.Query("SELECT id, target, user, at, kind, body, state FROM comments WHERE kind != 'comment' AND state = 'open' ORDER BY id")
	if err == nil {
		var reps []ReportView
		var targets []string
		for rows.Next() {
			var rv ReportView
			var t string
			rows.Scan(&rv.ID, &t, &rv.User, &rv.At, &rv.Kind, &rv.Body, &rv.State)
			reps = append(reps, rv)
			targets = append(targets, t)
		}
		rows.Close()
		for i, t := range targets {
			kind, id := splitTarget(t)
			if kind == "item" {
				var title string
				a.cat.QueryRow("SELECT title FROM items WHERE id = ?", id).Scan(&title)
				reps[i].TargetLabel, reps[i].TargetHref = "A"+strconv.FormatInt(id, 10)+" "+title, "/a/"+strconv.FormatInt(id, 10)
			} else if ref := a.tagRef(id); ref != nil {
				reps[i].TargetLabel, reps[i].TargetHref = ref.Path, ref.Href
			}
		}
		q.Reports = reps
	}
	a.renderSite(w, r, http.StatusOK, "site-queue", &SitePage{Title: "Vorschläge und Meldungen",
		Crumbs: []Crumb{{"/", "Start"}, {"/moderation", "Vorschläge und Meldungen"}}, Data: q})
}

func formIDs(r *http.Request, key string) []int64 {
	var out []int64
	for _, v := range r.Form[key] {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func (a *App) decideProposals(w http.ResponseWriter, r *http.Request) {
	ids := formIDs(r, "rev")
	accept := r.FormValue("action") == "accept"
	if err := a.cat.Decide(currentUser(r).ID, ids, accept); err != nil {
		a.editFailed(w, r, err, "/moderation")
		return
	}
	var changes []Change
	for _, id := range ids {
		var t string
		a.cat.QueryRow("SELECT target FROM revisions WHERE id = ?", id).Scan(&t)
		changes = append(changes, Change{Target: t})
	}
	a.cat.refreshFTS(changes)
	a.notifyDecided(ids, accept)
	http.Redirect(w, r, "/moderation?ok=entschieden", http.StatusSeeOther)
}

func (a *App) closeReport(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
	state := "done"
	if r.FormValue("action") == "dismiss" {
		state = "dismissed"
	}
	a.cat.Exec("UPDATE comments SET state = ?, decided_by = ?, decided_at = ? WHERE id = ? AND kind != 'comment'", state, currentUser(r).ID, now(), id)
	if note := strings.TrimSpace(r.FormValue("note")); note != "" {
		var target string
		a.cat.QueryRow("SELECT target FROM comments WHERE id = ?", id).Scan(&target)
		res, err := a.cat.Exec("INSERT INTO comments (target, parent, user, at, kind, body) VALUES (?, ?, ?, ?, 'comment', ?)", target, id, currentUser(r).ID, now(), note)
		if err == nil {
			cid, _ := res.LastInsertId()
			kind, tid := splitTarget(target)
			back := "/a/" + strconv.FormatInt(tid, 10)
			if kind == "tag" {
				if ref := a.tagRef(tid); ref != nil {
					back = ref.Href
				}
			}
			a.notifyReply(id, cid, back)
		}
	}
	http.Redirect(w, r, "/moderation?ok=erledigt", http.StatusSeeOther)
}

func (a *App) revertRevisions(w http.ResponseWriter, r *http.Request) {
	ids := formIDs(r, "rev")
	back := r.FormValue("back")
	if u, err := url.Parse(back); err != nil || u.Host != "" || !strings.HasPrefix(back, "/") {
		back = "/aenderungen"
	}
	if err := a.cat.Revert(currentUser(r).ID, ids, strings.TrimSpace(r.FormValue("reason"))); err != nil {
		a.editFailed(w, r, err, back)
		return
	}
	var changes []Change
	for _, id := range ids {
		var t string
		a.cat.QueryRow("SELECT target FROM revisions WHERE id = ?", id).Scan(&t)
		changes = append(changes, Change{Target: t})
	}
	a.cat.refreshFTS(changes)
	http.Redirect(w, r, back+"?ok=zurueckgenommen", http.StatusSeeOther)
}

// uploadPageImage stores a picture for the page text of a tag under
// seiten/<tag id>/ on the share and appends it to the page text.
func (a *App) uploadPageImage(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	ref := a.tagRef(id)
	u := currentUser(r)
	if ref == nil || !a.mayEdit(u) {
		a.notFound(w, r, "")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 20<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil || !a.csrfOK(r) {
		a.editFailed(w, r, fmt.Errorf("%w: das Bild ist zu groß (höchstens 20 MB) oder die Seite abgelaufen", errInvalid), ref.Href)
		return
	}
	f, hdr, err := r.FormFile("bild")
	if err != nil {
		a.editFailed(w, r, fmt.Errorf("%w: kein Bild", errInvalid), ref.Href)
		return
	}
	defer f.Close()
	ext := strings.ToLower(path.Ext(hdr.Filename))
	if !imageExts[ext] {
		a.editFailed(w, r, fmt.Errorf("%w: nur Bilder (jpg, png, gif, webp)", errInvalid), ref.Href)
		return
	}
	base := slugify(strings.TrimSuffix(path.Base(hdr.Filename), path.Ext(hdr.Filename)))
	dir := filepath.Join(a.cfg.ArchiveRoot, "seiten", strconv.FormatInt(id, 10))
	if err := os.MkdirAll(dir, 0o2775); err != nil {
		a.editFailed(w, r, err, ref.Href)
		return
	}
	name := freeName(dir, base+ext)
	out, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o664)
	if err != nil {
		a.editFailed(w, r, err, ref.Href)
		return
	}
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		a.editFailed(w, r, err, ref.Href)
		return
	}
	out.Close()
	var page string
	a.cat.QueryRow("SELECT body FROM tag_pages WHERE tag = ?", id).Scan(&page)
	img := fmt.Sprintf("![%s](/seiten/%d/%s)", base, id, url.PathEscape(name))
	nv := strings.TrimSpace(page + "\n\n" + img)
	if _, err := a.cat.Apply(u.ID, []Change{{Target: targetOf("page", id), Op: "set-page", Data: map[string]any{"old": page, "new": nv}, Rev: -1}}); err != nil {
		a.editFailed(w, r, err, ref.Href)
		return
	}
	http.Redirect(w, r, ref.Href+"?ok=bild", http.StatusSeeOther)
}
