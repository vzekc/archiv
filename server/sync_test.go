package archiv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// syncTest runs a sync over a copy of the test archive.
type syncTest struct {
	t    *testing.T
	root string
	cfg  *Config
	c    *Catalog
}

func newSyncTest(t *testing.T) *syncTest {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "archive")
	if err := os.CopyFS(root, os.DirFS("testdata/archive")); err != nil {
		t.Fatal(err)
	}
	cfg := defaultConfig()
	cfg.ArchiveRoot = root
	cfg.CatalogDB = filepath.Join(dir, "catalog.sqlite")
	cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	cfg.LegacyMap = "testdata/conf/legacy-map.conf"
	cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	c, err := openCatalog(cfg.CatalogDB)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return &syncTest{t: t, root: root, cfg: cfg, c: c}
}

func (st *syncTest) sync() {
	st.t.Helper()
	s := &Syncer{cfg: st.cfg, c: st.c, root: st.root, jobs: 2, quiet: true}
	if err := s.run(context.Background()); err != nil {
		st.t.Fatal(err)
	}
}

func (st *syncTest) strings(q string, args ...any) []string {
	st.t.Helper()
	rows, err := st.c.Query(q, args...)
	if err != nil {
		st.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (st *syncTest) one(q string, args ...any) string {
	st.t.Helper()
	r := st.strings(q, args...)
	if len(r) != 1 {
		st.t.Fatalf("%s: %d rows %q", q, len(r), r)
	}
	return r[0]
}

// itemTags lists "facet:name" of the item holding path.
func (st *syncTest) itemTags(path string) string {
	t := st.strings(`SELECT t.facet || ':' || t.name || CASE WHEN it.added_by = 'import:weak' THEN '?' ELSE '' END
	    FROM item_files f JOIN item_tags it ON it.item = f.item JOIN tags t ON t.id = it.tag
	    WHERE f.path = ? ORDER BY 1`, path)
	return strings.Join(t, ", ")
}

const sirius = "public/pub/rechner/victor/sirius1/"

func TestSyncImport(t *testing.T) {
	st := newSyncTest(t)
	st.sync()

	// every content file is in exactly one item, the furniture in none
	paths := st.strings("SELECT path FROM item_files ORDER BY path")
	want := []string{
		"CC-Bilder_und_Videos/2019-Treffen/bild1.jpg",
		"Zeitschriften/_Commodore/64er/64er_1984_01.pdf",
		"Zeitschriften/_Commodore/64er/64er_1984_02.pdf",
		"public/pub/manuals/dec/pdp11_handbook.pdf",
		sirius + "boot disk 1.img",
		sirius + "cpm86/cpm86.img",
		sirius + "cpm86/readme.htm",
		sirius + "prospekt ä.pdf",
		sirius + "sirius1.pdf",
		sirius + "sirius1_service_manual.pdf",
		"public/saved_mirrors/www.prof80.de.zip",
		"public/saved_mirrors/www.prof80.de/index.html",
		"public/saved_mirrors/www.prof80.de/sub/page.html",
	}
	sort.Strings(want)
	if strings.Join(paths, "\n") != strings.Join(want, "\n") {
		t.Errorf("paths:\n%s\nwant:\n%s", strings.Join(paths, "\n"), strings.Join(want, "\n"))
	}

	// titles from descriptions, else the name
	if got := st.one("SELECT i.title FROM items i JOIN item_files f ON f.item = i.id WHERE f.path = ?", sirius+"boot disk 1.img"); got != "Startdiskette Nr. 1" {
		t.Errorf("title %q", got)
	}
	if got := st.one("SELECT i.title FROM items i JOIN item_files f ON f.item = i.id WHERE f.path = ?", "public/pub/manuals/dec/pdp11_handbook.pdf"); got != "pdp11_handbook.pdf" {
		t.Errorf("title %q", got)
	}

	// the mirror is one item with both copies
	mid := st.one("SELECT item FROM item_files WHERE path = 'public/saved_mirrors/www.prof80.de.zip'")
	if n := st.one("SELECT COUNT(*) FROM item_files WHERE item = ?", mid); n != "3" {
		t.Errorf("mirror has %s files", n)
	}
	if got := st.one("SELECT title || ' | ' || descr || ' | ' || root FROM items WHERE id = ?", mid); got != "www.prof80.de | PROF-80 und PROF-180: Z80- bzw. Z180-Einplatinenrechner für den ECB-Bus. | public/saved_mirrors/www.prof80.de" {
		t.Errorf("mirror %q", got)
	}

	// tags from the rules and the folder
	for p, want := range map[string]string{
		sirius + "sirius1.pdf":                           "kind:Hersteller-Dokumentation?, location:Victor Sirius 1, machine:Sirius 1",
		sirius + "cpm86/cpm86.img":                       "kind:Betriebssysteme, location:cpm86, machine:Sirius 1",
		"public/pub/manuals/dec/pdp11_handbook.pdf":      "kind:Hersteller-Dokumentation, location:dec, machine:PDP-11",
		"Zeitschriften/_Commodore/64er/64er_1984_01.pdf": "kind:Zeitschrift, location:64'er, publication:64'er",
		"CC-Bilder_und_Videos/2019-Treffen/bild1.jpg":    "event:2019-Treffen, kind:Fotos, location:2019-Treffen",
		"public/saved_mirrors/www.prof80.de.zip":         "kind:Mirror, location:Gesicherte Websites",
	} {
		if got := st.itemTags(p); got != want {
			t.Errorf("%s: tags %q, want %q", p, got, want)
		}
	}
	if got := st.one("SELECT p.name || ' > ' || t.name FROM tags t JOIN tags p ON p.id = t.parent WHERE t.facet = 'publication'"); got != "Commodore > 64'er" {
		t.Errorf("publication %q", got)
	}
	if got := st.one("SELECT sort FROM tags WHERE facet = 'kind' AND name = 'Fotos' AND section = 1"); got != "70" {
		t.Errorf("sort %q", got)
	}
	if got := st.one("SELECT COUNT(*) FROM items_fts WHERE items_fts MATCH 'startdiskette'"); got != "1" {
		t.Errorf("fts %q", got)
	}
}

func TestSyncIdempotentMovesAndMissing(t *testing.T) {
	st := newSyncTest(t)
	st.sync()
	ids := st.strings("SELECT item || ' ' || path FROM item_files ORDER BY path")
	nTags := st.one("SELECT COUNT(*) FROM item_tags")
	time.Sleep(1100 * time.Millisecond) // the stamp is in seconds
	st.sync()
	if again := st.strings("SELECT item || ' ' || path FROM item_files ORDER BY path"); strings.Join(again, "\n") != strings.Join(ids, "\n") {
		t.Fatalf("second sync changed the items:\n%q\n%q", ids, again)
	}
	if n := st.one("SELECT COUNT(*) FROM item_tags"); n != nTags {
		t.Errorf("tags %s, then %s", nTags, n)
	}

	// a move keeps the item and leaves a redirect; a deletion marks it missing
	id := st.one("SELECT item FROM item_files WHERE path = ?", sirius+"sirius1.pdf")
	if err := os.Rename(filepath.Join(st.root, sirius+"sirius1.pdf"), filepath.Join(st.root, "public/pub/manuals/sirius1.pdf")); err != nil {
		t.Fatal(err)
	}
	gone := st.one("SELECT item FROM item_files WHERE path = ?", sirius+"boot disk 1.img")
	os.Remove(filepath.Join(st.root, sirius+"boot disk 1.img"))
	// a changed file keeps its item
	changed := st.one("SELECT item FROM item_files WHERE path = ?", sirius+"sirius1_service_manual.pdf")
	os.WriteFile(filepath.Join(st.root, sirius+"sirius1_service_manual.pdf"), []byte("%PDF-1.4 neu"), 0o644)
	time.Sleep(1100 * time.Millisecond)
	st.sync()

	if got := st.one("SELECT item FROM item_files WHERE path = 'public/pub/manuals/sirius1.pdf'"); got != id {
		t.Errorf("moved file: item %s, want %s", got, id)
	}
	if got := st.one("SELECT item || ' ' || name FROM redirects WHERE old_path = ?", sirius+"sirius1.pdf"); got != id+" sirius1.pdf" {
		t.Errorf("redirect %q", got)
	}
	if got := st.itemTags("public/pub/manuals/sirius1.pdf"); got != "kind:Hersteller-Dokumentation, location:manuals" {
		t.Errorf("moved file tags %q", got)
	}
	if got := st.one("SELECT state FROM items WHERE id = ?", gone); got != "missing" {
		t.Errorf("deleted file: state %q", got)
	}
	if got := st.one("SELECT item FROM item_files WHERE path = ?", sirius+"sirius1_service_manual.pdf"); got != changed {
		t.Errorf("changed file: item %s, want %s", got, changed)
	}
	if got := st.one("SELECT COUNT(*) FROM items_fts WHERE items_fts MATCH 'startdiskette'"); got != "0" {
		t.Errorf("missing item still in the full text")
	}
}

func TestImportReport(t *testing.T) {
	a := testApp(t)
	a.cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	a.cfg.LegacyMap = "testdata/conf/legacy-map.conf"
	a.cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	s := &Syncer{cfg: a.cfg, c: a.cat, root: a.cfg.ArchiveRoot, jobs: 2, quiet: true}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := get(t, a.routes(), "/moderation/import")
	body := w.Body.String()
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, body)
	}
	for _, want := range []string{"Stand des Imports", "machine", "Unsortiert: ", "Zeitschriften/_*/*"} {
		if !strings.Contains(body, want) {
			t.Errorf("%q missing", want)
		}
	}
	if w := get(t, a.routes(), "/moderation/import", "X-Dev-User", "2:gast:Vereinsmitglieder"); w.Code != 403 {
		t.Errorf("report open to members: %d", w.Code)
	}
}

func TestDescriptionsRoundTrip(t *testing.T) {
	st := newSyncTest(t)
	st.sync()
	dir := "public/pub/rechner/victor/sirius1"
	data := st.c.descriptionsFor(dir)
	if !strings.HasPrefix(string(data), "Title= Victor Sirius 1\r\n") || !strings.Contains(string(data), "prospekt \xe4.pdf Werbeprospekt mit Umlaut\r\n") {
		t.Errorf("descriptions: %q", data)
	}
	// read back as the import reads them, they give the same descriptions
	title, lines := parseDescriptions(data)
	_, orig := readDescriptions(filepath.Join(st.root, dir))
	names := []string{"sirius1_service_manual.pdf", "sirius1.pdf", "boot disk 1.img", "cpm86", "prospekt ä.pdf"}
	if title != "Victor Sirius 1" || fmt.Sprint(matchEntries(lines, names)) != fmt.Sprint(matchEntries(orig, names)) {
		t.Errorf("round trip: %q\n%v\n%v", title, matchEntries(lines, names), matchEntries(orig, names))
	}
}

// The folders below the one a rule names a magazine (or machine, meeting)
// by become groups: a tree, scoped to that magazine. A folder holding a
// single item, or named by a rule itself, is no group. Moderators may
// rename and merge groups, or give an item a group by hand; the sync
// keeps that.
func TestSyncGroups(t *testing.T) {
	st := newSyncTest(t)
	mag := filepath.Join(st.root, "Zeitschriften/_Commodore/64er")
	write := func(rel, body string) {
		p := filepath.Join(mag, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("Disks/disk1.d64", "d1")
	write("Disks/disk2.d64", "d2")
	write("Disks/Sonderhefte/s1.d64", "s1")
	write("Disks/Sonderhefte/s2.d64", "s2")
	write("Einzeln/nur.pdf", "nur")
	write("Einzeln/Thumbs.db", "cache")
	write("Extras/x1.prg", "x1")
	write("Extras/x2.prg", "x2")
	write("heft1.d64", "beilage")
	write("Werbung/ad1.pdf", "ad1")
	write("Werbung/ad2.pdf", "ad2")
	rules, _ := os.ReadFile(st.cfg.LegacyMap)
	lm := filepath.Join(t.TempDir(), "legacy-map.conf")
	os.WriteFile(lm, append(rules, []byte("Zeitschriften/_*/*/**/*disks*   kind=Datenträger zum Heft\n*.d64 under Zeitschriften/_*/*   kind=Datenträger zum Heft\n")...), 0o644)
	st.cfg.LegacyMap = lm
	st.sync()

	const z = "Zeitschriften/_Commodore/64er/"
	group := func(p string) string {
		return strings.Join(st.strings(`SELECT COALESCE(par.name || ' › ', '') || t.name FROM item_files f JOIN item_tags it ON it.item = f.item
		    JOIN tags t ON t.id = it.tag LEFT JOIN tags par ON par.id = t.parent WHERE f.path = ? AND t.facet = 'group'`, p), "; ")
	}
	for p, want := range map[string]string{
		z + "64er_1984_01.pdf":         "",
		z + "Disks/disk1.d64":          "", // the rule names it: its kind says it
		z + "Disks/Sonderhefte/s1.d64": "Sonderhefte",
		z + "Einzeln/nur.pdf":          "",
		z + "Extras/x1.prg":            "Extras",
		z + "Werbung/ad1.pdf":          "Werbung",
	} {
		if got := group(p); got != want {
			t.Errorf("%s: group %q, want %q", p, got, want)
		}
	}
	if got := st.one("SELECT s.name FROM tags t JOIN tags s ON s.id = t.scope WHERE t.facet = 'group' AND t.name = 'Extras'"); got != "64'er" {
		t.Errorf("scope %q", got)
	}
	// of the kinds, a rule naming files wins over one naming the same folder
	if got := st.itemTags(z + "heft1.d64"); !strings.Contains(got, "kind:Datenträger zum Heft") || strings.Contains(got, "kind:Zeitschrift") {
		t.Errorf("kind of a disk image beside the issues: %q", got)
	}
	// of the kinds, the rule naming the deeper folder wins
	if got := st.itemTags(z + "Disks/disk1.d64"); !strings.Contains(got, "kind:Datenträger zum Heft") || strings.Contains(got, "kind:Zeitschrift") {
		t.Errorf("kind of a disk: %q", got)
	}

	// a moderator renames one group and merges another into it; an item
	// gets a group by hand
	extras := st.one("SELECT id FROM tags WHERE facet = 'group' AND name = 'Extras'")
	ads := st.one("SELECT id FROM tags WHERE facet = 'group' AND name = 'Werbung'")
	special := st.one("SELECT id FROM tags WHERE facet = 'group' AND name = 'Sonderhefte'")
	if _, err := st.c.Apply("1", []Change{
		{Target: "tag:" + extras, Op: "set-field", Data: map[string]any{"field": "name", "old": "Extras", "new": "Beilagen"}, Rev: -1},
		{Target: "tag:" + ads, Op: "merge-tag", Data: map[string]any{"into": atoi(extras)}, Rev: -1},
	}); err != nil {
		t.Fatal(err)
	}
	other := st.one("SELECT item FROM item_files WHERE path = ?", z+"Extras/x2.prg")
	if _, err := st.c.Apply("1", []Change{
		{Target: "item:" + other, Op: "remove-tag", Data: map[string]any{"tag": atoi(extras)}, Rev: -1},
		{Target: "item:" + other, Op: "add-tag", Data: map[string]any{"tag": atoi(special), "by": "1"}, Rev: -1},
	}); err != nil {
		t.Fatal(err)
	}
	st.sync()
	if got := group(z + "Extras/x1.prg"); got != "Beilagen" {
		t.Errorf("renamed group: %q", got)
	}
	if got := group(z + "Werbung/ad2.pdf"); got != "Beilagen" {
		t.Errorf("merged group: %q", got)
	}
	if n := st.one("SELECT COUNT(*) FROM tags WHERE facet = 'group' AND merged_into IS NULL AND name IN ('Extras', 'Werbung')"); n != "0" {
		t.Errorf("groups made again: %s", n)
	}
	if got := group(z + "Extras/x2.prg"); got != "Sonderhefte" {
		t.Errorf("group by hand: %q", got)
	}
}

// A collection named by its source has groups, too, and the rule naming
// files wins over the folder's kind.
func TestSyncCollectionGroups(t *testing.T) {
	st := newSyncTest(t)
	coll := filepath.Join(st.root, "public/pub/cdrom/simtel")
	for _, n := range []string{"beehive/a.com", "beehive/b.com", "asmutl/x.com", "asmutl/y.doc", "single.com"} {
		p := filepath.Join(coll, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(n), 0o644)
	}
	rules, _ := os.ReadFile(st.cfg.LegacyMap)
	lm := filepath.Join(t.TempDir(), "legacy-map.conf")
	os.WriteFile(lm, append(rules, []byte("public/pub/cdrom   kind=CD-ROM/Sammlung source=Sammlungen\n"+
		"public/pub/cdrom/simtel   source=Sammlungen > Simtel kind=Anwendungsprogramme\n"+
		"*.doc under public/pub/cdrom/simtel   kind=Begleittext\n")...), 0o644)
	st.cfg.LegacyMap = lm
	st.sync()
	for p, want := range map[string]string{
		"public/pub/cdrom/simtel/beehive/a.com": "group:beehive, kind:Anwendungsprogramme, location:beehive, source:Simtel",
		"public/pub/cdrom/simtel/asmutl/y.doc":  "group:asmutl, kind:Begleittext, location:asmutl, source:Simtel",
		"public/pub/cdrom/simtel/single.com":    "kind:Anwendungsprogramme, location:simtel, source:Simtel",
	} {
		if got := st.itemTags(p); got != want {
			t.Errorf("%s: %q, want %q", p, got, want)
		}
	}
}
