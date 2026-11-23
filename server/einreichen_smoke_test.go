package archiv

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// testApp is a service on scratch databases, logged in as a moderator.
func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := defaultConfig()
	cfg.Home = dir
	cfg.DBPath = filepath.Join(dir, "einreichen.sqlite")
	cfg.StagingDir = filepath.Join(dir, "eingang")
	cfg.ArchiveRoot, _ = filepath.Abs("testdata/archive")
	cfg.CollectionDB = filepath.Join(dir, "collection.sqlite")
	cfg.CatalogDB = filepath.Join(dir, "catalog.sqlite")
	cfg.LLMKeyFile = filepath.Join(dir, "no-key")
	cfg.DevUser = "1:hans:Vereinsmitglieder,Moderatoren"
	cfg.resolve()
	a, err := newApp(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.db.Close(); a.cat.Close() })
	return a
}

func get(t *testing.T, h http.Handler, path string, hdr ...string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestEinreichenRoutes(t *testing.T) {
	h := testApp(t).routes()
	for _, c := range []struct {
		path, want string
		code       int
	}{
		{"/einreichen/", "Einreichungen", 200},
		{"/einreichen/liste", "", 200},
		{"/einreichen/hilfe", "", 200},
		{"/einreichen/moderation", "", 200},
		{"/einreichen/kontingente", "", 200},
		{"/einreichen/static/einreichen.css", "", 200},
		{"/einreichen/e/999", "", 404},
	} {
		w := get(t, h, c.path)
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.path, w.Code, c.code)
		}
		if !strings.Contains(w.Body.String(), c.want) {
			t.Errorf("%s: %q missing", c.path, c.want)
		}
	}
	// members outside the moderators' group do not get the moderation pages
	if w := get(t, h, "/einreichen/moderation", "X-Dev-User", "2:gast:Vereinsmitglieder"); w.Code == 200 {
		t.Errorf("moderation open to a member: %d", w.Code)
	}
}

// The owner of a draft removes every file that exists already elsewhere in
// one go; the other files stay, and nobody else can do it.
func TestRemoveDuplicates(t *testing.T) {
	a := testApp(t)
	h := a.routes()
	res, err := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated) VALUES ('1', 'Disketten', ?, 1, 1)", StDraft)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := res.LastInsertId()
	for _, f := range []struct{ name, status, dups string }{
		{"neu.img", FReady, ""},
		{"alt.img", FReady, `[{"where":"archive","path":"pub/alt.img"}]`},
		{"anderswo.img", FReady, `[{"where":"pending","path":"anderswo.img","submission":7}]`},
		{"laeuft.img", FProcessing, `[{"where":"archive","path":"pub/laeuft.img"}]`},
	} {
		if _, err := a.db.Exec(`INSERT INTO files (submission, name, size, status, duplicates, created) VALUES (?, ?, 1, ?, ?, 1)`,
			sub, f.name, f.status, f.dups); err != nil {
			t.Fatal(err)
		}
	}
	page := fmt.Sprintf("/einreichen/e/%d", sub)
	if body := get(t, h, page, "X-Dev-User", "1:hans:Vereinsmitglieder").Body.String(); !strings.Contains(body, "2 Dateien gibt</span> es schon") {
		t.Errorf("the page does not offer to remove the two duplicates")
	}
	if w := postForm(t, h, page+"/doubletten-entfernen", url.Values{}, "2:gast:Vereinsmitglieder"); w.Code == http.StatusSeeOther {
		t.Errorf("someone else removed the duplicates")
	}
	if w := postForm(t, h, page+"/doubletten-entfernen", url.Values{}, ""); w.Code != http.StatusSeeOther {
		t.Fatalf("status %d", w.Code)
	}
	var names []string
	rows, _ := a.db.Query("SELECT name FROM files WHERE submission = ? ORDER BY name", sub)
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	if strings.Join(names, " ") != "laeuft.img neu.img" {
		t.Errorf("files left: %v", names)
	}
}
