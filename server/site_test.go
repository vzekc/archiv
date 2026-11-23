package archiv

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// siteApp is a service over a copy of the test archive, synced.
func siteApp(t *testing.T) *App {
	t.Helper()
	a := testApp(t)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.CopyFS(root, os.DirFS("testdata/archive")); err != nil {
		t.Fatal(err)
	}
	a.cfg.ArchiveRoot = root
	a.cfg.HomeTitle = "Archiv klassischer Computer"
	a.cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	a.cfg.LegacyMap = "testdata/conf/legacy-map.conf"
	a.cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	a.cfg.PagesDB = filepath.Join(t.TempDir(), "pages.sqlite")
	s := &Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestSitePages(t *testing.T) {
	a := siteApp(t)
	h := a.routes()
	id := func(p string) string {
		var s string
		a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", p).Scan(&s)
		return s
	}
	manual := id(sirius + "sirius1_service_manual.pdf")
	var sha string
	a.cat.QueryRow("SELECT sha256 FROM item_files WHERE path = ?", sirius+"sirius1_service_manual.pdf").Scan(&sha)

	for _, c := range []struct {
		path string
		code int
		want []string
	}{
		{"/", 200, []string{"Archiv klassischer Computer", "Rechner", "Zeitschriften", "Neu im Archiv"}},
		{"/public/pub/rechner/victor/sirius1/", 200, []string{"Victor Sirius 1", "Startdiskette Nr. 1", "boot%20disk%201.img", "cpm86/", "/a/" + manual, "Als ZIP herunterladen", "übergeordnetes Verzeichnis"}},
		{"/public/pub/rechner/victor/sirius1", 301, nil},
		{"/public/saved_mirrors/www.prof80.de/", 200, []string{`<a href="sub/page.html">Unterseite</a>`}},
		{"/public/saved_mirrors/", 200, []string{"www.prof80.de.zip", "Gesicherte Websites"}},
		{"/Zeitschriften/_Commodore/64er/", 200, []string{"Ausgabe 1/1984", "64er_1984_01.pdf"}},
		{"/gibtsnicht/", 404, nil},
		{"/public/pub/../../../etc/passwd", 307, nil}, // cleaned by the mux, then not found
		{"/suche", 200, []string{"Was möchtest du finden?"}},
		{"/suche?q=startdiskette", 200, []string{"1 Treffer", "Im Titel", "Startdiskette Nr. 1"}},
		{"/suche?q=sirius", 200, []string{"Sirius 1", `class="tile f-machine"`, "Im Titel"}},
		{"/suche?q=sirius&in=Zeitschriften", 302, nil},
		{"/suche?q=sirius+1", 200, []string{`<section class="shelf">`, `href="/r/victor/sirius-1">Sirius 1</a>`}},
		{"/suche?q=sirius", 200, []string{"nur Titel, Schlagworte und Dateinamen"}},
		{"/suche?q=sirius&volltext=nein", 200, []string{"in Titeln, Schlagworten und Dateinamen", "auch im Text der Dokumente suchen"}},
		{"/suche?q=sirius&volltext=nein&in=Zeitschriften", 302, nil},
		{"/suche?q=einplatinenrechner", 200, []string{"1 Treffer"}},
		{"/suche?q=einplatinenrechner&volltext=nein", 200, []string{"0 Treffer"}},
		{"/suche?q=victor+sirius", 200, []string{`<section class="shelf">`, `href="/r/victor/sirius-1">Sirius 1</a>`}},
		{"/suche?q=sirius&rechner=alle", 200, []string{"in Rechner", "Sirius 1"}},
		{"/suche?q=sirius&zeitschrift=alle", 200, []string{"0 Treffer", "in Zeitschriften"}},
		{"/suche?q=sirius&rechner=victor/sirius-1", 200, []string{"in Sirius 1", `value="tags:`}},
		{"/z/", 200, []string{`value="tags:publication" selected`}},
		{"/search.php?q=kaypro&in=public", 301, nil},
		{"/a/" + manual, 200, []string{"Service-Handbuch, 1983", "Rechner: Victor › Sirius 1", "Art: Hersteller-Dokumentation", "PDF öffnen", "Mehr zu Sirius 1", `value="tags:`}},
		{"/a/" + manual + "?teil=panel", 200, []string{"Service-Handbuch, 1983", "Ganze Seite"}},
		{"/a/" + manual + "/sirius1_service_manual.pdf", 200, []string{"%PDF"}},
		{"/a/" + manual + "/datei/1", 200, []string{"%PDF"}},
		{"/a/" + manual + "/service-handbuch-1983", 302, nil},
		{"/a/999999", 404, nil},
		{"/h/" + sha, 200, []string{"%PDF"}},
		{"/r/", 200, []string{"Rechner", "Victor", "DEC", `href="/r/victor"`}},
		{"/r/victor", 200, []string{"Victor", "Service-Handbuch"}},
		{"/r/victor/sirius-1", 200, []string{"Sirius 1", "Service-Handbuch, 1983", `class="x"`}},
		{"/r/victor/sirius-1?art=betriebssysteme", 200, []string{"cpm86", "Betriebssysteme"}},
		{"/v?ordner=public/pub/rechner/victor/sirius1", 200, []string{"Service-Handbuch, 1983", "Ordner"}},
		{"/r/gibtsnicht", 404, nil},
		{"/z/", 200, []string{"Zeitschriften", "Commodore"}},
		{"/z/commodore/64er", 200, []string{"Ausgabe 1/1984", "Ausgabe 2/1984"}},
		{"/m/", 301, nil},
		{"/v?art=mirror", 200, []string{"www.prof80.de"}},
		{"/treffen/", 200, []string{"2019-Treffen"}},
		{"/v?sprache=de", 200, nil},
		{"/mirrors.html", 301, nil},
		{"/public/pub/rechner/victor/index.html", 301, nil},
		{"/public/saved_mirrors/www.prof80.de/index.html", 200, []string{"Unterseite"}},
	} {
		w := get(t, h, c.path)
		if w.Code != c.code {
			t.Errorf("%s: status %d, want %d", c.path, w.Code, c.code)
			continue
		}
		for _, s := range c.want {
			if !strings.Contains(w.Body.String(), s) {
				t.Errorf("%s: %q missing", c.path, s)
			}
		}
	}
	for range 2 {
		if w := get(t, h, "/search.php?q=kaypro&in=public"); w.Header().Get("Location") != "/suche?q=kaypro&in=public" {
			t.Errorf("search.php redirect to %q", w.Header().Get("Location"))
		}
	}
	if w := get(t, h, "/etc/passwd"); w.Code != 404 {
		t.Errorf("/etc/passwd: %d", w.Code)
	}
}

func TestSiteMovedFile(t *testing.T) {
	a := siteApp(t)
	old := sirius + "sirius1.pdf"
	os.Rename(filepath.Join(a.cfg.ArchiveRoot, old), filepath.Join(a.cfg.ArchiveRoot, "public/pub/manuals/sirius1.pdf"))
	time.Sleep(1100 * time.Millisecond)
	s := &Syncer{cfg: a.cfg, c: a.cat, root: a.cfg.ArchiveRoot, jobs: 2, quiet: true}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := get(t, a.routes(), "/"+old)
	if w.Code != 301 || !strings.HasPrefix(w.Header().Get("Location"), "/a/") || !strings.HasSuffix(w.Header().Get("Location"), "/sirius1.pdf") {
		t.Errorf("moved file: %d %q", w.Code, w.Header().Get("Location"))
	}
}

func TestSiteZip(t *testing.T) {
	a := siteApp(t)
	w := get(t, a.routes(), "/zip?p=public/pub/rechner/victor")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
		if f.Name == "victor/sirius1/sirius1.pdf" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			if !bytes.HasPrefix(b, []byte("%PDF")) || f.Method != zip.Store {
				t.Errorf("sirius1.pdf: method %d, %q", f.Method, b[:8])
			}
		}
	}
	sort.Strings(names)
	got := strings.Join(names, "\n")
	for _, f := range zr.File {
		if f.Name == "victor/sirius1/descriptions" {
			rc, _ := f.Open()
			b, _ := io.ReadAll(rc)
			if !strings.Contains(string(b), "boot disk 1.img Startdiskette Nr. 1\r\n") {
				t.Errorf("descriptions in the ZIP: %q", b)
			}
		}
	}
	for _, want := range []string{"victor/", "victor/descriptions", "victor/sirius1/descriptions", "victor/sirius1/boot disk 1.img", "victor/sirius1/cpm86/readme.htm"} {
		if !strings.Contains(got+"\n", want+"\n") {
			t.Errorf("%q missing in\n%s", want, got)
		}
	}
	for _, not := range []string{"victor/index.html", "victor/sirius1/banner", "victor/sirius1/index.html"} {
		if strings.Contains(got+"\n", not+"\n") {
			t.Errorf("%q in the ZIP", not)
		}
	}
	if w := get(t, a.routes(), "/zip?p=../x"); w.Code != 404 {
		t.Errorf("zip outside: %d", w.Code)
	}
}

func TestSiteSearchPages(t *testing.T) {
	a := siteApp(t)
	db, err := sql.Open("sqlite", a.cfg.PagesDB)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		"CREATE TABLE pages (id INTEGER PRIMARY KEY, path TEXT NOT NULL, page INTEGER NOT NULL, body TEXT NOT NULL)",
		"CREATE VIRTUAL TABLE pages_fts USING fts5(body, content = 'pages', content_rowid = 'id')",
		"CREATE TABLE docs (path TEXT PRIMARY KEY, state TEXT)",
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for i, body := range []string{"Einleitung", "Der Floppy-Controller FD1797 liest", "Noch einmal der FD1797"} {
		db.Exec("INSERT INTO pages (path, page, body) VALUES (?, ?, ?)", "Zeitschriften/_Commodore/64er/64er_1984_02.pdf", i+1, body)
	}
	db.Exec("INSERT INTO pages_fts (pages_fts) VALUES ('rebuild')")
	db.Close()

	w := get(t, a.routes(), "/suche?q=fd1797")
	body := w.Body.String()
	for _, want := range []string{"1 Treffer", "lesen?pfad=Zeitschriften/_Commodore/64er/64er_1984_02.pdf", "2 Seiten:", "<mark>FD1797</mark>", "seite=2", "seite=3"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q missing", want)
		}
	}
	var id string
	a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", "Zeitschriften/_Commodore/64er/64er_1984_02.pdf").Scan(&id)
	if w := get(t, a.routes(), "/a/"+id+"?q=fd1797"); !strings.Contains(w.Body.String(), "Fundstellen für") ||
		!strings.Contains(w.Body.String(), "/a/"+id+"/lesen?pfad=Zeitschriften/_Commodore/64er/64er_1984_02.pdf&amp;q=fd1797&amp;seite=2") {
		t.Errorf("finds missing: %d", w.Code)
	}
	w = get(t, a.routes(), "/a/"+id+"/lesen?q=fd1797")
	if b := w.Body.String(); w.Code != 200 || !strings.Contains(b, "data-reader=") || !strings.Contains(b, "&#34;hits&#34;:[2,3]") ||
		!strings.Contains(b, "&#34;page&#34;:2") {
		t.Errorf("reader: %d %s", w.Code, b[:min(len(b), 2000)])
	}
	if w := get(t, a.routes(), "/suche?q=fd1797"); !strings.Contains(w.Body.String(), "/lesen?pfad=") {
		t.Errorf("search hits do not lead to the reader")
	}
	if w := get(t, a.routes(), "/suche?q=fd1797&in=public"); w.Header().Get("Location") != "/suche?ordner=public&q=fd1797" {
		t.Errorf("folder scope: %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(t, a.routes(), "/suche?q=fd1797&ordner=public"); !strings.Contains(w.Body.String(), "0 Treffer") {
		t.Errorf("scope ignored")
	}
}

// The old folder tree filters what an old link names, but the views offer
// it neither as a row to narrow down nor as shelves, and the start page
// has no way into it.
func TestViewWithoutFolders(t *testing.T) {
	a := testApp(t)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.CopyFS(root, os.DirFS("testdata/archive")); err != nil {
		t.Fatal(err)
	}
	// enough items in two folders below one machine that its page shows
	// shelves
	for _, d := range []string{"teil-a", "teil-b"} {
		dir := filepath.Join(root, "public/pub/rechner/victor/sirius1", d)
		os.MkdirAll(dir, 0o755)
		for i := range 35 {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("blatt%02d.txt", i)), []byte(d+strconv.Itoa(i)), 0o644)
		}
	}
	a.cfg.ArchiveRoot = root
	a.cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	a.cfg.LegacyMap = "testdata/conf/legacy-map.conf"
	a.cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	a.cfg.PagesDB = filepath.Join(t.TempDir(), "pages.sqlite")
	s := &Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	h := a.routes()
	var item string
	a.cat.QueryRow("SELECT item FROM item_files WHERE path LIKE '%teil-a/blatt00.txt'").Scan(&item)
	for _, p := range []string{"/", "/r/", "/r/victor", "/r/victor/sirius-1", "/suche?q=blatt", "/a/" + item} {
		body := get(t, h, p).Body.String()
		if strings.Contains(body, `href="/public/pub/"`) {
			t.Errorf("%s: link into the folder tree", p)
		}
		if strings.Contains(body, "ordner=") || strings.Contains(body, "Ordner:") || strings.Contains(body, "Im selben Ordner") {
			t.Errorf("%s: link to a folder view", p)
		}
	}
	if w := get(t, h, "/v?ordner=public/pub/rechner/victor/sirius1/teil-a"); w.Code != 200 || !strings.Contains(w.Body.String(), "teil-a") {
		t.Errorf("an old folder link does not filter: %d", w.Code)
	}

	// a search with many hits leads with what it is about
	if body := get(t, h, "/suche?q=blatt").Body.String(); !strings.Contains(body, ">Im Titel, in Schlagworten oder Dateinamen</a>") {
		t.Errorf("search: no shelf of the hits in the title")
	}

	// the folders below the machine are its groups: a shelf each, and one
	// for the rest
	body := get(t, h, "/r/victor/sirius-1").Body.String()
	for _, want := range []string{`gruppe=teil-a">teil-a</a>`, `gruppe=teil-b">teil-b</a>`, `gruppe=-">Ohne Gruppe</a>`, "nach Gruppe"} {
		if !strings.Contains(body, want) {
			t.Errorf("machine page: %q missing", want)
		}
	}
	rest := get(t, h, "/r/victor/sirius-1?gruppe=-").Body.String()
	if !strings.Contains(rest, "Service-Handbuch, 1983") || strings.Contains(rest, "blatt00") || !strings.Contains(rest, "ohne Gruppe") {
		t.Errorf("the items without a group")
	}
}

// A small page is divided too: a shelf for the part of a facet it has,
// one for the rest, and the rows that offer a choice.
func TestSmallViewShelves(t *testing.T) {
	a := testApp(t)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.CopyFS(root, os.DirFS("testdata/archive")); err != nil {
		t.Fatal(err)
	}
	for i := range 4 {
		os.WriteFile(filepath.Join(root, "public/pub/rechner/victor", fmt.Sprintf("allgemein%d.pdf", i)), []byte(strconv.Itoa(i)), 0o644)
	}
	a.cfg.ArchiveRoot = root
	a.cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	a.cfg.LegacyMap = "testdata/conf/legacy-map.conf"
	a.cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	a.cfg.PagesDB = filepath.Join(t.TempDir(), "pages.sqlite")
	if err := (&Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := get(t, a.routes(), "/r/victor").Body.String()
	for _, want := range []string{`href="/r/victor/sirius-1">Sirius 1</a>`, "<h2>Weitere", "Eingrenzen nach Rechner, Art"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q missing", want)
		}
	}
}

// A group holding nearly all of a view is no choice: the row and the
// shelves start at its children.
func TestViewDescendsIntoBigGroup(t *testing.T) {
	a := testApp(t)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.CopyFS(root, os.DirFS("testdata/archive")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"alpha", "beta"} {
		dir := filepath.Join(root, "public/pub/rechner/victor/sirius1/images", d)
		os.MkdirAll(dir, 0o755)
		for i := range 30 {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("f%02d.txt", i)), []byte(d+strconv.Itoa(i)), 0o644)
		}
	}
	a.cfg.ArchiveRoot = root
	a.cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	a.cfg.LegacyMap = "testdata/conf/legacy-map.conf"
	a.cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	a.cfg.PagesDB = filepath.Join(t.TempDir(), "pages.sqlite")
	if err := (&Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	body := get(t, a.routes(), "/r/victor/sirius-1").Body.String()
	for _, want := range []string{">alpha</a>", ">beta</a>"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q missing", want)
		}
	}
}

func TestTagWords(t *testing.T) {
	a := siteApp(t)
	idx := a.index()
	var manual int32 = -1
	for i, it := range idx.items {
		if strings.HasSuffix(it.Path, "pdp11_handbook.pdf") {
			manual = int32(i)
		}
	}
	if manual < 0 {
		t.Fatal("no manual")
	}
	if n := tagWords(idx, idx.items[manual].Tags, words("PDP-11/34")); n != 2 {
		t.Errorf("PDP-11/34: %d words in the tags", n)
	}
	if n := tagWords(idx, idx.items[manual].Tags, words("commodore")); n != 0 {
		t.Errorf("commodore: %d", n)
	}
}

func TestViewZip(t *testing.T) {
	a := siteApp(t)
	h := a.routes()
	body := get(t, h, "/r/victor/sirius-1").Body.String()
	if !strings.Contains(body, `href="/r/victor/sirius-1?als=zip"`) {
		t.Fatalf("no ZIP link on the view")
	}
	w := get(t, h, "/r/victor/sirius-1?als=zip")
	if w.Code != 200 || w.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("zip: %d %s", w.Code, w.Body.String())
	}
	zr, err := zip.NewReader(bytes.NewReader(w.Body.Bytes()), int64(w.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	pdf := false
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "sirius-1/") || strings.Count(f.Name, "/") < 2 {
			t.Errorf("entry outside an item folder: %s", f.Name)
		}
		if strings.HasSuffix(f.Name, "/sirius1.pdf") {
			pdf = true
		}
	}
	if !pdf {
		t.Errorf("sirius1.pdf missing")
	}
	if w := get(t, h, "/v?als=zip"); w.Code == 200 {
		t.Errorf("the whole archive as ZIP: %d", w.Code)
	}
}
