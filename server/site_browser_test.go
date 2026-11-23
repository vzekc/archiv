package archiv

import (
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func apiPostJSON(t *testing.T, a *App, path string, body any) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(body)
	tok := strings.Repeat("cd", 16)
	r := httptest.NewRequest("POST", path, strings.NewReader(string(b)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-CSRF-Token", tok)
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: tok})
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func apiGet(t *testing.T, a *App, path string, out any) {
	t.Helper()
	w := get(t, a.routes(), path)
	if w.Code != 200 {
		t.Fatalf("%s: %d %s", path, w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), out); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func TestBrowserAPI(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	var root []Node
	apiGet(t, a, "/api/tree?node=root", &root)
	if len(root) < 5 || root[0].ID != "f:machine" || root[len(root)-1].ID != "s:" {
		t.Fatalf("root: %+v", root)
	}
	var victor []Node
	apiGet(t, a, "/api/tree?node=f:machine", &victor)
	var vic, dec int64
	for _, n := range victor {
		switch n.Label {
		case "Victor":
			vic = n.Tag
		case "DEC":
			dec = n.Tag
		}
	}
	var sub []Node
	apiGet(t, a, "/api/tree?node=t:"+strconv.FormatInt(vic, 10), &sub)
	if len(sub) != 1 || sub[0].Label != "Sirius 1" || sub[0].Count != 6 {
		t.Fatalf("below Victor: %+v", sub)
	}
	sirius := sub[0].Tag
	var c NodeContent
	apiGet(t, a, "/api/node?node=t:"+strconv.FormatInt(sirius, 10), &c)
	if c.Total != 6 || len(c.Items) != 6 {
		t.Fatalf("node: %d items", c.Total)
	}
	it := c.Items[0]

	// move one item to DEC (same facet: the tag is replaced) and undo it
	code, res := apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{
		{"op": "move", "items": []int64{it.ID}, "revs": []int{it.Rev}, "from": sirius, "to": dec}}})
	if code != 200 || res["batch"] == "" {
		t.Fatalf("move: %d %v", code, res)
	}
	apiGet(t, a, "/api/node?node=t:"+strconv.FormatInt(sirius, 10), &c)
	if c.Total != 5 {
		t.Errorf("after move: %d", c.Total)
	}
	// the browser's revision is stale now
	code, _ = apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{
		{"op": "add", "items": []int64{it.ID}, "revs": []int{it.Rev}, "to": sirius}}})
	if code != 409 {
		t.Errorf("stale add: %d", code)
	}
	if code, _ := apiPostJSON(t, a, "/api/undo", map[string]any{"batch": res["batch"]}); code != 200 {
		t.Errorf("undo: %d", code)
	}
	apiGet(t, a, "/api/node?node=t:"+strconv.FormatInt(sirius, 10), &c)
	if c.Total != 6 {
		t.Errorf("after undo: %d", c.Total)
	}

	// a new folder below Sirius 1, and a column pasted into the titles
	code, _ = apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "new-folder", "to": sirius, "name": "Sirius 1 Plus"}}})
	apiGet(t, a, "/api/tree?node=t:"+strconv.FormatInt(sirius, 10), &sub)
	if code != 200 || len(sub) != 1 || sub[0].Label != "Sirius 1 Plus" {
		t.Errorf("new folder: %d %+v", code, sub)
	}
	ids := []int64{c.Items[0].ID, c.Items[1].ID}
	revs := []int{c.Items[0].Rev, c.Items[1].Rev}
	apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "set", "field": "year", "items": ids, "revs": revs, "values": []string{"1982", "1983"}}}})
	var one BrowserItem
	apiGet(t, a, "/api/item?id="+strconv.FormatInt(ids[1], 10), &one)
	if one.Year != "1983" {
		t.Errorf("pasted column: %q", one.Year)
	}

	// read-only while the old site runs
	a.cfg.Editing = false
	if code, _ := apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "remove", "items": ids, "from": sirius}}}); code != 403 {
		t.Errorf("batch while read-only: %d", code)
	}
}

func TestStoreItem(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	var id int64
	old := sirius + "boot disk 1.img"
	a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", old).Scan(&id)
	if code, res := apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "store", "items": []int64{id}}}}); code != 200 {
		t.Fatalf("store: %d %v", code, res)
	}
	want := storeDir(id) + "/boot disk 1.img"
	if _, err := os.Stat(filepath.Join(a.cfg.ArchiveRoot, want)); err != nil {
		t.Fatalf("not on disk: %v", err)
	}
	if w := get(t, a.routes(), href(old)); w.Code != 301 || !strings.HasPrefix(w.Header().Get("Location"), "/a/") {
		t.Errorf("old path: %d %q", w.Code, w.Header().Get("Location"))
	}
	// the mirror moves as a whole
	var mid int64
	a.cat.QueryRow("SELECT id FROM items WHERE root IS NOT NULL").Scan(&mid)
	if code, res := apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "store", "items": []int64{mid}}}}); code != 200 {
		t.Fatalf("store mirror: %d %v", code, res)
	}
	if _, err := os.Stat(filepath.Join(a.cfg.ArchiveRoot, storeDir(mid), "www.prof80.de/sub/page.html")); err != nil {
		t.Errorf("mirror not moved: %v", err)
	}
	// a sync afterwards changes nothing
	time.Sleep(1100 * time.Millisecond)
	s := &Syncer{cfg: a.cfg, c: a.cat, root: a.cfg.ArchiveRoot, jobs: 2, quiet: true}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var n, items int
	a.cat.QueryRow("SELECT COUNT(*) FROM item_files WHERE path = ? AND item = ?", want, id).Scan(&n)
	a.cat.QueryRow("SELECT COUNT(*) FROM items WHERE state = 'live'").Scan(&items)
	if n != 1 || items != 11 {
		t.Errorf("after sync: %d, %d items", n, items)
	}
	var locs int
	a.cat.QueryRow("SELECT COUNT(*) FROM tags WHERE facet = 'location' AND path LIKE 'objekte%'").Scan(&locs)
	if locs != 0 {
		t.Errorf("%d location tags below objekte/", locs)
	}
}

func TestBrowserUpload(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	a.cfg.BinDir = t.TempDir()
	a.cfg.ReindexLock = filepath.Join(t.TempDir(), "lock")
	var tag int64
	a.cat.QueryRow("SELECT id FROM tags WHERE facet = 'machine' AND name = 'Sirius 1'").Scan(&tag)
	var body strings.Builder
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("file", "Sirius Prospekt 1983.pdf")
	fw.Write([]byte("%PDF-1.4 prospekt"))
	mw.Close()
	tok := strings.Repeat("ef", 16)
	r := httptest.NewRequest("POST", "/api/upload?node=t:"+strconv.FormatInt(tag, 10), strings.NewReader(body.String()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("X-CSRF-Token", tok)
	r.AddCookie(&http.Cookie{Name: csrfCookie, Value: tok})
	w := httptest.NewRecorder()
	a.routes().ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	var res struct{ Items []int64 }
	json.Unmarshal(w.Body.Bytes(), &res)
	if len(res.Items) != 1 {
		t.Fatalf("items %v", res.Items)
	}
	id := res.Items[0]
	if _, err := os.Stat(filepath.Join(a.cfg.ArchiveRoot, storeDir(id), "Sirius Prospekt 1983.pdf")); err != nil {
		t.Errorf("file: %v", err)
	}
	var title string
	var n int
	a.cat.QueryRow("SELECT title FROM items WHERE id = ? AND state = 'live'", id).Scan(&title)
	a.cat.QueryRow("SELECT COUNT(*) FROM item_tags WHERE item = ? AND tag = ?", id, tag).Scan(&n)
	if title != "Sirius Prospekt 1983" || n != 1 {
		t.Errorf("item %q, tagged %d", title, n)
	}
	time.Sleep(200 * time.Millisecond) // the background refresh
}

func TestBrowserCollections(t *testing.T) {
	a := siteApp(t)
	a.cfg.Editing = true
	code, _ := apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "new-collection", "name": "Einstieg Sirius"}}})
	var cs []Node
	apiGet(t, a, "/api/tree?node=coll", &cs)
	if code != 200 || len(cs) != 1 || cs[0].Label != "Einstieg Sirius" {
		t.Fatalf("collection: %d %+v", code, cs)
	}
	var ids []int64
	for _, p := range []string{sirius + "sirius1_service_manual.pdf", sirius + "sirius1.pdf"} {
		var id int64
		a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", p).Scan(&id)
		ids = append(ids, id)
	}
	apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "collect", "to": cs[0].Tag, "items": ids}}})
	var c NodeContent
	apiGet(t, a, "/api/node?node="+cs[0].ID, &c)
	if c.Total != 2 || c.Items[0].ID != ids[0] {
		t.Errorf("collection content: %+v", c)
	}
	body := get(t, a.routes(), "/s/"+strconv.FormatInt(cs[0].Tag, 10)).Body.String()
	if !strings.Contains(body, "Einstieg Sirius") || strings.Index(body, "Service-Handbuch") > strings.Index(body, "Kurzbeschreibung") {
		t.Errorf("page: order or title wrong")
	}
	_, res := apiPostJSON(t, a, "/api/batch", map[string]any{"ops": []map[string]any{{"op": "uncollect", "to": cs[0].Tag, "items": ids[:1]}}})
	apiPostJSON(t, a, "/api/undo", map[string]any{"batch": res["batch"]})
	apiGet(t, a, "/api/node?node="+cs[0].ID, &c)
	if c.Total != 2 || c.Items[0].ID != ids[0] {
		t.Errorf("after undo of the removal: %+v", c.Items)
	}
}
