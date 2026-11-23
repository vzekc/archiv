package archiv

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// postForm sends a form as the dev user, or as another one with X-Dev-User,
// with a valid CSRF token.
func postForm(t *testing.T, h http.Handler, path string, v url.Values, user string) *httptest.ResponseRecorder {
	t.Helper()
	tok := strings.Repeat("ab", 16)
	v.Set("csrf", tok)
	r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: tok})
	if user != "" {
		r.Header.Set("X-Dev-User", user)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

const member = "2:Vereinsmitglieder"

func TestSiteEditing(t *testing.T) {
	a := siteApp(t)
	h := a.routes()
	var id string
	a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", sirius+"sirius1.pdf").Scan(&id)
	item := "/a/" + id

	// read-only while the old site runs
	if w := postForm(t, h, item+"/bearbeiten", url.Values{"title": {"x"}, "rev": {"0"}}, ""); w.Code != 403 {
		t.Fatalf("editing while off: %d", w.Code)
	}
	a.cfg.Editing = true

	// a moderator's edit applies at once
	w := postForm(t, h, item+"/bearbeiten", url.Values{"title": {"Sirius 1, Kurzbeschreibung"}, "descr": {"Kurzbeschreibung"},
		"year": {"1982"}, "rev": {"0"}, "tag_add": {"System: CP/M-86"}, "reason": {"steht auf dem Deckblatt"}}, "")
	if w.Code != 303 || !strings.HasSuffix(w.Header().Get("Location"), "ok=gespeichert") {
		t.Fatalf("edit: %d %s %s", w.Code, w.Header().Get("Location"), w.Body.String())
	}
	body := get(t, h, item).Body.String()
	for _, want := range []string{"Sirius 1, Kurzbeschreibung", "1982", "CP/M-86", "Bearbeiten"} {
		if !strings.Contains(body, want) {
			t.Errorf("item page: %q missing", want)
		}
	}
	if !strings.Contains(get(t, h, "/suche?q=deckblatt").Body.String(), "0 Treffer") ||
		!strings.Contains(get(t, h, "/suche?q=cp/m-86").Body.String(), "1 Treffer") {
		t.Errorf("full text not updated")
	}

	// a member proposes; it waits in the queue
	w = postForm(t, h, item+"/bearbeiten", url.Values{"year": {"1983"}, "rev": {"1"}}, member)
	if !strings.HasSuffix(w.Header().Get("Location"), "ok=vorgeschlagen") {
		t.Fatalf("proposal: %d %s", w.Code, w.Header().Get("Location"))
	}
	q := get(t, h, "/moderation").Body.String()
	if !strings.Contains(q, "Jahr: „1982“ → „1983“") {
		t.Fatalf("queue: %s", q)
	}
	if w := get(t, h, "/moderation", "X-Dev-User", member); w.Code == 200 {
		t.Errorf("queue open to members")
	}
	var rev string
	a.cat.QueryRow("SELECT id FROM revisions WHERE state = 'proposed'").Scan(&rev)
	postForm(t, h, "/moderation/vorschlaege", url.Values{"rev": {rev}, "action": {"accept"}}, "")
	if !strings.Contains(get(t, h, item).Body.String(), "1983") {
		t.Errorf("accepted proposal not applied")
	}

	// a stale edit is refused
	if w := postForm(t, h, item+"/bearbeiten", url.Values{"title": {"alt"}, "rev": {"0"}}, ""); w.Code != 409 || !strings.Contains(w.Body.String(), "In der Zwischenzeit") {
		t.Errorf("stale edit: %d", w.Code)
	}

	// report and reply; the report shows in the queue
	postForm(t, h, item+"/kommentar", url.Values{"kind": {"missing-pages"}, "body": {"Seite 3 fehlt"}}, member)
	var cid string
	a.cat.QueryRow("SELECT id FROM comments").Scan(&cid)
	postForm(t, h, "/moderation/meldung", url.Values{"id": {cid}, "action": {"done"}, "note": {"neu gescannt"}}, "")
	body = get(t, h, item).Body.String()
	for _, want := range []string{"Seite 3 fehlt", "Seiten fehlen", "erledigt", "neu gescannt"} {
		if !strings.Contains(body, want) {
			t.Errorf("comments: %q missing", want)
		}
	}
	var jobs int
	a.cat.QueryRow("SELECT COUNT(*) FROM notify_jobs WHERE user = '2'").Scan(&jobs)
	if jobs != 2 { // the decision on the proposal and the answer to the report
		t.Errorf("%d notifications for the member", jobs)
	}

	// history, and revert
	hist := get(t, h, item+"/verlauf").Body.String()
	if !strings.Contains(hist, "Titel: „Kurzbeschreibung“ → „Sirius 1, Kurzbeschreibung“") || !strings.Contains(hist, "steht auf dem Deckblatt") {
		t.Errorf("history: %s", hist)
	}
	var titleRev string
	a.cat.QueryRow("SELECT id FROM revisions WHERE op = 'set-field' AND data LIKE '%\"title\"%'").Scan(&titleRev)
	postForm(t, h, "/moderation/zuruecknehmen", url.Values{"rev": {titleRev}, "back": {item}}, "")
	if body := get(t, h, item).Body.String(); !strings.Contains(body, "<h1>Kurzbeschreibung</h1>") {
		t.Errorf("revert did not restore the title")
	}
	if !strings.Contains(get(t, h, "/aenderungen").Body.String(), "zurückgenommen") {
		t.Errorf("recent changes")
	}
}

func TestSiteTagEditing(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	h := a.routes()
	var id string
	a.cat.QueryRow("SELECT id FROM tags WHERE facet = 'machine' AND name = 'Sirius 1'").Scan(&id)
	w := postForm(t, h, "/schlagwort/"+id+"/bearbeiten", url.Values{"rev": {"0"}, "name": {"Sirius 1"},
		"page": {"Der **Sirius 1** von 1982.\n\n<script>alert(1)</script>\n\n[x](javascript:alert(1))"}, "new_child": {"Sirius 1 Plus"}}, "")
	if w.Code != 303 {
		t.Fatalf("tag edit: %d %s", w.Code, w.Body.String())
	}
	body := get(t, h, "/r/victor/sirius-1").Body.String()
	if !strings.Contains(body, "<strong>Sirius 1</strong>") || strings.Contains(body, "<script>alert") || strings.Contains(body, `href="javascript:`) {
		t.Errorf("page text: %s", body)
	}
	var plus int
	a.cat.QueryRow("SELECT COUNT(*) FROM tags WHERE name = 'Sirius 1 Plus' AND parent = ?", id).Scan(&plus)
	if plus != 1 {
		t.Errorf("new child tag missing")
	}
	// rename: the old URL redirects... to the new slug via the tag page
	var rev string
	a.cat.QueryRow("SELECT rev FROM tags WHERE id = ?", id).Scan(&rev)
	postForm(t, h, "/schlagwort/"+id+"/bearbeiten", url.Values{"rev": {rev}, "name": {"Sirius 1 (Victor 9000)"}}, "")
	if w := get(t, h, "/r/victor/sirius-1-victor-9000"); w.Code != 200 {
		t.Errorf("renamed tag: %d", w.Code)
	}
	for old, to := range map[string]string{"/r/victor/sirius-1": "/r/victor/sirius-1-victor-9000",
		"/r/victor/sirius-1/sirius-1-plus": "/r/victor/sirius-1-victor-9000/sirius-1-plus", "/r/victor/gibts-nicht": "/r/victor"} {
		if w := get(t, h, old); w.Header().Get("Location") != to {
			t.Errorf("%s: %d to %q, want %q", old, w.Code, w.Header().Get("Location"), to)
		}
	}
	if w := get(t, h, "/api/schlagworte?q=sirius"); !strings.Contains(w.Body.String(), "Rechner: Victor › Sirius 1 (Victor 9000)") {
		t.Errorf("suggest: %s", w.Body.String())
	}
	// a folder: a new name, the same place and URL
	var loc, slug string
	a.cat.QueryRow("SELECT id, slug FROM tags WHERE facet = 'location' AND path = 'public/pub/rechner/victor'").Scan(&loc, &slug)
	w = postForm(t, h, "/schlagwort/"+loc+"/bearbeiten", url.Values{"rev": {"0"}, "name": {"Victor-Rechner"}, "move_under": {"-"}}, "")
	if w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/v?ordner=public/pub/rechner/victor") {
		t.Fatalf("folder edit: %d %q", w.Code, w.Header().Get("Location"))
	}
	var name, slug2 string
	var parent int64
	a.cat.QueryRow("SELECT name, slug, COALESCE(parent, 0) FROM tags WHERE id = ?", loc).Scan(&name, &slug2, &parent)
	if name != "Victor-Rechner" || slug2 != slug || parent == 0 {
		t.Errorf("folder after edit: %q %q %d", name, slug2, parent)
	}
	if body := get(t, h, "/v?ordner=public/pub/rechner/victor").Body.String(); !strings.Contains(body, "Victor-Rechner") || !strings.Contains(body, `class="ed"`) {
		t.Errorf("folder view after rename")
	}
}

// "Gruppe: Peripherie" on an item finds or makes the group below its
// machine; members may use groups but not make them.
func TestItemGroup(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	h := a.routes()
	item := func(name string) string {
		var id string
		a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", sirius+name).Scan(&id)
		return id
	}
	rev := func(id string) string {
		var r string
		a.cat.QueryRow("SELECT rev FROM items WHERE id = ?", id).Scan(&r)
		return r
	}
	add := func(id, tag, user string) int {
		return postForm(t, h, "/a/"+id+"/bearbeiten", url.Values{"rev": {rev(id)}, "tag_add": {tag}}, user).Code
	}
	if code := add(item("sirius1.pdf"), "Gruppe: Peripherie", member); code != 409 {
		t.Fatalf("member: %d", code)
	}
	var n int
	a.cat.QueryRow("SELECT COUNT(*) FROM tags WHERE facet = 'group' AND name = 'Peripherie'").Scan(&n)
	if n != 0 {
		t.Fatalf("a member made a group")
	}
	for _, f := range []string{"sirius1.pdf", "sirius1_service_manual.pdf"} {
		if code := add(item(f), "Gruppe: Peripherie", ""); code != 303 {
			t.Fatalf("%s: %d", f, code)
		}
	}
	var scope string
	a.cat.QueryRow("SELECT COUNT(*), (SELECT s.name FROM tags g JOIN tags s ON s.id = g.scope WHERE g.facet = 'group' AND g.name = 'Peripherie') FROM tags WHERE facet = 'group' AND name = 'Peripherie'").Scan(&n, &scope)
	if n != 1 || scope != "Sirius 1" {
		t.Errorf("groups %d, scope %q", n, scope)
	}
	a.cat.QueryRow("SELECT COUNT(*) FROM item_tags it JOIN tags t ON t.id = it.tag WHERE t.facet = 'group' AND t.name = 'Peripherie'").Scan(&n)
	if n != 2 {
		t.Errorf("%d items in the group", n)
	}
}

// A keyword given to an item is a way into a view and found by the search.
func TestKeyword(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	h := a.routes()
	var id, rev string
	a.cat.QueryRow("SELECT i.id, i.rev FROM items i JOIN item_files f ON f.item = i.id WHERE f.path = ?", sirius+"sirius1.pdf").Scan(&id, &rev)
	if w := postForm(t, h, "/a/"+id+"/bearbeiten", url.Values{"rev": {rev}, "tag_add": {"Stichwort: Z80"}}, ""); w.Code != 303 {
		t.Fatalf("edit: %d", w.Code)
	}
	if body := get(t, h, "/v?stichwort=z80").Body.String(); !strings.Contains(body, "/a/"+id) {
		t.Errorf("view of the keyword")
	}
	if body := get(t, h, "/a/"+id).Body.String(); !strings.Contains(body, "Stichwort: Z80") {
		t.Errorf("item page")
	}
	if body := get(t, h, "/suche?q=z80").Body.String(); !strings.Contains(body, `class="tile f-keyword"`) {
		t.Errorf("search tile")
	}
}

// The files of an item: renamed, taken out as an item of their own, and
// removed; the old addresses lead to them, and a sync keeps it so.
func TestFileEdits(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	h := a.routes()
	id := func(name string) string {
		var x string
		a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", sirius+name).Scan(&x)
		return x
	}
	main, b, c := id("sirius1.pdf"), id("sirius1_service_manual.pdf"), id("prospekt ä.pdf")
	if _, err := a.cat.Apply("1", []Change{
		{Target: "item:" + b, Op: "merge", Data: map[string]any{"into": atoi(main)}, Rev: -1},
		{Target: "item:" + c, Op: "merge", Data: map[string]any{"into": atoi(main)}, Rev: -1},
	}); err != nil {
		t.Fatal(err)
	}
	if body := get(t, h, "/a/"+main).Body.String(); !strings.Contains(body, "als eigenes Objekt abtrennen") || !strings.Contains(body, `name="fname_`) {
		t.Errorf("edit form without the files")
	}
	it, _ := a.loadItem(atoi(main))
	n := map[string]string{}
	for _, f := range it.Files {
		n[f.Name] = strconv.Itoa(f.N)
	}
	form := url.Values{"rev": {strconv.Itoa(it.Rev)}, "reason": {"doppelt"},
		"fname_" + n["sirius1.pdf"]:               {"Handbuch.pdf"},
		"fact_" + n["sirius1_service_manual.pdf"]: {"split"},
		"fact_" + n["prospekt ä.pdf"]:             {"remove"}}
	if w := postForm(t, h, "/a/"+main+"/bearbeiten", form, ""); w.Code != 303 {
		t.Fatalf("edit: %d %s", w.Code, w.Body.String())
	}
	where := func(path string) (string, string, string) {
		var item, name, state string
		a.cat.QueryRow("SELECT f.item, f.name, i.state FROM item_files f JOIN items i ON i.id = f.item WHERE f.path = ?", sirius+path).Scan(&item, &name, &state)
		return item, name, state
	}
	if item, name, _ := where("sirius1.pdf"); item != main || name != "Handbuch.pdf" {
		t.Errorf("renamed: %s %s", item, name)
	}
	split, _, state := where("sirius1_service_manual.pdf")
	if split == main || state != "live" {
		t.Errorf("split: %s %s", split, state)
	}
	removed, _, state := where("prospekt ä.pdf")
	if removed == main || state != "withdrawn" {
		t.Errorf("removed: %s %s", removed, state)
	}
	for from, to := range map[string]string{
		"/a/" + main + "/sirius1.pdf":                "/a/" + main + "/Handbuch.pdf",
		"/a/" + main + "/sirius1_service_manual.pdf": "/a/" + split + "/sirius1_service_manual.pdf",
	} {
		if w := get(t, h, from); w.Code != 301 || w.Header().Get("Location") != to {
			t.Errorf("%s: %d %s", from, w.Code, w.Header().Get("Location"))
		}
	}
	var hist string
	a.cat.QueryRow("SELECT group_concat(op) FROM revisions WHERE target = ? AND op LIKE '%-file'", "item:"+main).Scan(&hist)
	if hist != "rename-file,split-file,remove-file" {
		t.Errorf("history: %q", hist)
	}
	before := 0
	a.cat.QueryRow("SELECT COUNT(*) FROM items").Scan(&before)
	if err := (&Syncer{cfg: a.cfg, c: a.cat, root: a.cfg.ArchiveRoot, jobs: 2, quiet: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	after := 0
	a.cat.QueryRow("SELECT COUNT(*) FROM items").Scan(&after)
	if item, _, state := where("prospekt ä.pdf"); after != before || item != removed || state != "withdrawn" {
		t.Errorf("after sync: %d items (were %d), removed file in %s %s", after, before, item, state)
	}
}

// A tag merged into another is found by its old name as that one.
func TestResolveMergedTag(t *testing.T) {
	a := siteApp(t)
	plus, err := a.createTagPath("1", "Rechner: Victor › Sirius 1 Plus")
	if err != nil {
		t.Fatal(err)
	}
	sirius, _ := a.resolveTag("Rechner: Victor › Sirius 1")
	if _, err := a.cat.Apply("1", []Change{{Target: targetOf("tag", plus), Op: "merge-tag", Data: map[string]any{"into": sirius}, Rev: -1}}); err != nil {
		t.Fatal(err)
	}
	if got, err := a.resolveTag("Rechner: Victor › Sirius 1 Plus"); err != nil || got != sirius {
		t.Errorf("merged tag resolves to %d %v, want %d", got, err, sirius)
	}
}
