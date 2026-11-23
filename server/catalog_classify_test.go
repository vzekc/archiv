package archiv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestClassifyChanges(t *testing.T) {
	a := siteApp(t)
	folders, err := a.foldersToClassify("", 10)
	if err != nil {
		t.Fatal(err)
	}
	// the PDFs below rechner carry only a guessed kind; the magazines and
	// photos are fully tagged
	if strings.Join(folders, ",") != "public/pub/rechner/victor/sirius1" {
		t.Errorf("folders: %q", folders)
	}
	var id int64
	a.cat.QueryRow("SELECT item FROM item_files WHERE path = ?", sirius+"prospekt ä.pdf").Scan(&id)
	var ans classifyAnswer
	ans.Items = append(ans.Items, struct {
		ID         int64   `json:"id"`
		Machine    string  `json:"machine"`
		NewMachine bool    `json:"new_machine"`
		Kind       string  `json:"kind"`
		System     string  `json:"system"`
		Language   string  `json:"language"`
		Confidence float64 `json:"confidence"`
		Note       string  `json:"note"`
	}{ID: id, Machine: "Victor > Sirius 1 > Sirius 1 Plus", NewMachine: true, Kind: "Sekundärliteratur", Language: "de", Confidence: 0.8, Note: "Werbung"},
		struct {
			ID         int64   `json:"id"`
			Machine    string  `json:"machine"`
			NewMachine bool    `json:"new_machine"`
			Kind       string  `json:"kind"`
			System     string  `json:"system"`
			Language   string  `json:"language"`
			Confidence float64 `json:"confidence"`
			Note       string  `json:"note"`
		}{ID: 999999, Machine: "DEC", Confidence: 0.9}, // not asked about
	)
	changes := a.classifyChanges("ai:test", []classifyItem{{ID: id}}, ans)
	var ops []string
	for _, c := range changes {
		ops = append(ops, c.Op+" "+str(c.Data, "path")+str(c.Data, "tag"))
	}
	got := strings.Join(ops, "\n")
	for _, want := range []string{"add-tag-path Victor > Sirius 1 > Sirius 1 Plus", "add-tag ", "remove-tag ", "add-tag-path de"} {
		if !strings.Contains(got, want) {
			t.Errorf("%q missing in\n%s", want, got)
		}
	}
	if _, err := a.cat.Propose("ai:test", changes); err != nil {
		t.Fatal(err)
	}
	rows, _ := a.cat.Query("SELECT id FROM revisions WHERE state = 'proposed'")
	var ids []int64
	for rows.Next() {
		var x int64
		rows.Scan(&x)
		ids = append(ids, x)
	}
	rows.Close()
	if err := a.cat.Decide("1", ids, true); err != nil {
		t.Fatal(err)
	}
	var n int
	a.cat.QueryRow(`SELECT COUNT(*) FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = ? AND t.name IN ('Sirius 1 Plus', 'Sekundärliteratur', 'de')`, id).Scan(&n)
	if n != 3 {
		t.Errorf("%d of 3 tags after accepting", n)
	}
	a.cat.QueryRow(`SELECT COUNT(*) FROM item_tags WHERE item = ? AND added_by = 'import:weak'`, id).Scan(&n)
	if n != 0 {
		t.Errorf("the guessed kind stayed")
	}
	// accepted, the new tag can be undone like any other
	var rev int64
	a.cat.QueryRow("SELECT id FROM revisions WHERE op = 'add-tag-path' AND data LIKE '%Plus%'").Scan(&rev)
	if err := a.cat.Revert("1", []int64{rev}, ""); err != nil {
		t.Errorf("revert: %v", err)
	}
}

func TestProposeRelations(t *testing.T) {
	a := siteApp(t)
	root := a.cfg.ArchiveRoot
	d := filepath.Join(root, "public/pub/rechner/victor/sirius1")
	os.WriteFile(filepath.Join(d, "cpm_disk1.img"), []byte("eins"), 0o644)
	os.WriteFile(filepath.Join(d, "cpm_disk2.img"), []byte("zwei"), 0o644)
	b, _ := os.ReadFile(filepath.Join(d, "sirius1.pdf"))
	os.WriteFile(filepath.Join(root, "public/pub/manuals/sirius1-kopie.pdf"), b, 0o644)
	s := &Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}
	if err := s.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := a.proposeRelations("ai:rel", "")
	if err != nil || n != 2 {
		t.Fatalf("%d proposals, %v", n, err)
	}
	var ops string
	a.cat.QueryRow("SELECT group_concat(op, ',') FROM (SELECT op FROM revisions WHERE state = 'proposed' ORDER BY op)").Scan(&ops)
	if ops != "add-rel,merge" {
		t.Errorf("ops %q", ops)
	}
}

// The pages of an issue scanned to one image each are proposed as one
// item; accepted, the item holds the pages in order under the folder's
// name. Photos are left alone.
func TestProposePages(t *testing.T) {
	a := testApp(t)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.CopyFS(root, os.DirFS("testdata/archive")); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"Zeitschriften/_Commodore/64er/heft5", "CC-Bilder_und_Videos/2019-Treffen/album"} {
		dir := filepath.Join(root, d)
		os.MkdirAll(dir, 0o755)
		for i := 10; i >= 1; i-- {
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("p%d.jpg", i)), []byte(d+strconv.Itoa(i)), 0o644)
		}
		os.WriteFile(filepath.Join(dir, "Thumbs.db"), []byte("cache"), 0o644)
	}
	// a cover tape: its image and the pictures of its inlay
	tape := filepath.Join(root, "Zeitschriften/_Commodore/64er/Tapes/tape01")
	os.MkdirAll(tape, 0o755)
	for _, n := range []string{"inlay-b.jpg", "game.t64", "inlay-a.jpg"} {
		os.WriteFile(filepath.Join(tape, n), []byte(n), 0o644)
	}
	// a disk image with a photo of its label of the same name, in a folder
	// of several
	disks := filepath.Join(root, "public/pub/rechner/victor/sirius1/disks")
	os.MkdirAll(disks, 0o755)
	for _, n := range []string{"boot.img", "boot.imd", "boot.jpg", "util.img", "util.img.txt", "other.jpg"} {
		os.WriteFile(filepath.Join(disks, n), []byte(n), 0o644)
	}
	rules, _ := os.ReadFile("testdata/conf/legacy-map.conf")
	lm := filepath.Join(t.TempDir(), "legacy-map.conf")
	os.WriteFile(lm, append(rules, []byte("Zeitschriften/_*/*/**/*tape*   kind=Datenträger zum Heft\n"+
		"public/pub/rechner/**/disks   kind=Anwendungsprogramme\n*.{jpg,png} under public/pub/rechner/**/disks   kind=Fotos\n")...), 0o644)
	a.cfg.ArchiveRoot = root
	a.cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	a.cfg.LegacyMap = lm
	a.cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	if err := (&Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := a.proposePages("ai:test", "")
	if err != nil || n != 3 {
		t.Fatalf("proposed %d folders: %v", n, err)
	}
	// the moderators see what each would make; the pictures are found by
	// the files' hashes, in the catalog
	a.cfg.Editing = true
	a.cfg.ThumbsByHash = true
	queue := get(t, a.routes(), "/moderation").Body.String()
	for _, want := range []string{"automatischer Vorschlag", "Übernehmen macht daraus <b>ein Objekt</b> „heft5“", "mit 10 Dateien in dieser Reihenfolge",
		"Die anderen 9 Objekte gehen darin auf", "die Bilder werden sein Titelbild", `<span class="role">Titelbild</span>`, `<span class="ext">T64</span>`} {
		if !strings.Contains(queue, want) {
			t.Errorf("queue: %q missing", want)
		}
	}
	// a second run proposes nothing while these are open
	if n, err := a.proposePages("ai:test2", ""); err != nil || n != 0 {
		t.Fatalf("proposed again: %d %v", n, err)
	}
	var ids []int64
	rows, _ := a.cat.Query("SELECT id FROM revisions WHERE state = 'proposed' AND op = 'gather'")
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if err := a.cat.Decide("1", ids, true); err != nil {
		t.Fatal(err)
	}
	var item int64
	var title string
	a.cat.QueryRow("SELECT i.id, i.title FROM items i JOIN item_files f ON f.item = i.id WHERE f.path LIKE '%heft5/p1.jpg' AND i.state = 'live'").Scan(&item, &title)
	if title != "heft5" {
		t.Errorf("title %q", title)
	}
	var names []string
	rows, _ = a.cat.Query("SELECT name FROM item_files WHERE item = ? ORDER BY ord", item)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		names = append(names, s)
	}
	rows.Close()
	if strings.Join(names, " ") != "p1.jpg p2.jpg p3.jpg p4.jpg p5.jpg p6.jpg p7.jpg p8.jpg p9.jpg p10.jpg" {
		t.Errorf("pages %q", names)
	}
	var merged int
	a.cat.QueryRow("SELECT COUNT(*) FROM items WHERE state = 'merged' AND merged_into = ?", item).Scan(&merged)
	if merged != 9 {
		t.Errorf("%d merged", merged)
	}
	// the tape first, its inlay the cover
	var tapeFiles []string
	rows, _ = a.cat.Query(`SELECT name || ':' || role FROM item_files WHERE item =
	    (SELECT item FROM item_files WHERE path LIKE '%tape01/game.t64') ORDER BY ord`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		tapeFiles = append(tapeFiles, s)
	}
	rows.Close()
	if strings.Join(tapeFiles, " ") != "game.t64:main inlay-a.jpg:cover inlay-b.jpg:cover" {
		t.Errorf("tape: %q", tapeFiles)
	}
	var boot []string
	rows, _ = a.cat.Query(`SELECT i.title || ' ' || f.name || ':' || f.role FROM item_files f JOIN items i ON i.id = f.item
	    WHERE f.item = (SELECT item FROM item_files WHERE path LIKE '%disks/boot.img') ORDER BY f.ord`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		boot = append(boot, s)
	}
	rows.Close()
	if strings.Join(boot, ", ") != "boot.imd boot.imd:main, boot.imd boot.img:part, boot.imd boot.jpg:cover" {
		t.Errorf("disk with its label: %q", boot)
	}
	var kinds []string
	rows, _ = a.cat.Query(`SELECT t.name FROM item_tags it JOIN tags t ON t.id = it.tag WHERE t.facet = 'kind'
	    AND it.item = (SELECT item FROM item_files WHERE path LIKE '%disks/boot.img')`)
	for rows.Next() {
		var s string
		rows.Scan(&s)
		kinds = append(kinds, s)
	}
	rows.Close()
	if strings.Join(kinds, ",") != "Anwendungsprogramme" {
		t.Errorf("kinds of the disk with its label: %q", kinds)
	}
	var bootItem string
	a.cat.QueryRow("SELECT item FROM item_files WHERE path LIKE '%disks/boot.img'").Scan(&bootItem)
	if panel := get(t, a.routes(), "/a/"+bootItem+"?teil=panel").Body.String(); !strings.Contains(panel, "boot.jpg") || !strings.Contains(panel, "<img") {
		t.Errorf("the panel does not show the label")
	}
	// the next sync keeps the item together
	if err := (&Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	var files int
	a.cat.QueryRow("SELECT COUNT(*) FROM item_files WHERE item = ?", item).Scan(&files)
	if files != 10 {
		t.Errorf("after sync: %d files", files)
	}
}

// A disk with its image in two formats, the log, the label and the
// folder it was unpacked into becomes one item; the unpacked files keep
// their path as name, and only the label is the cover.
func TestProposeDisks(t *testing.T) {
	a := testApp(t)
	root := filepath.Join(t.TempDir(), "archive")
	if err := os.CopyFS(root, os.DirFS("testdata/archive")); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "public/pub/rechner/victor/sirius1/images")
	for _, n := range []string{"soft.imd", "soft.img", "soft.log", "soft.jpg", "soft.imd.zip", "soft/!inhalt.txt", "soft/u1/a.com", "soft/u2/a.com", "soft/u2/bild.jpg", "other.imd"} {
		p := filepath.Join(dir, n)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(n), 0o644)
	}
	a.cfg.ArchiveRoot = root
	a.cfg.TaxonomyConf = "testdata/conf/taxonomy.conf"
	a.cfg.LegacyMap = "testdata/conf/legacy-map.conf"
	a.cfg.MirrorsConf = "testdata/conf/mirrors.conf"
	if err := (&Syncer{cfg: a.cfg, c: a.cat, root: root, jobs: 2, quiet: true}).run(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := a.proposeDisks("ai:test", "")
	if err != nil || n != 1 {
		t.Fatalf("proposed %d: %v", n, err)
	}
	if n, _ := a.proposeDisks("ai:test2", ""); n != 0 {
		t.Fatalf("proposed again: %d", n)
	}
	var ids []int64
	rows, _ := a.cat.Query("SELECT id FROM revisions WHERE state = 'proposed'")
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if err := a.cat.Decide("1", ids, true); err != nil {
		t.Fatal(err)
	}
	var got []string
	var title string
	rows, _ = a.cat.Query(`SELECT i.title, f.name || ':' || f.role FROM item_files f JOIN items i ON i.id = f.item
	    WHERE f.item = (SELECT item FROM item_files WHERE path LIKE '%images/soft.imd') ORDER BY f.name`)
	for rows.Next() {
		var s string
		rows.Scan(&title, &s)
		got = append(got, s)
	}
	rows.Close()
	sort.Strings(got)
	want := "soft.imd.zip:part soft.imd:main soft.img:part soft.jpg:cover soft.log:part soft/!inhalt.txt:part soft/u1/a.com:part soft/u2/a.com:part soft/u2/bild.jpg:part"
	if strings.Join(got, " ") != want || title != "soft" {
		t.Errorf("%q: %q", title, got)
	}
}
