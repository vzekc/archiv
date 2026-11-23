package archiv

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestIntakeCatalog(t *testing.T) {
	a := siteApp(t)
	a.cfg.BackupDir = t.TempDir()
	a.cfg.BinDir = t.TempDir() // no Python jobs in the test
	a.cfg.ReindexLock = filepath.Join(t.TempDir(), "lock")
	db := a.db
	res, err := db.Exec("INSERT INTO submissions (owner, title, state, created, updated, credit) VALUES ('2', 'Sirius', ?, 1, 1, 1)", StIntake)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := res.LastInsertId()
	a.names.remember("2", "toshi")
	stage := filepath.Join(a.cfg.StagingDir, "files", "s")
	os.MkdirAll(stage, 0o755)
	add := func(name, kind, tags, descr, sugg string, body []byte, unpack bool) {
		os.WriteFile(filepath.Join(stage, name), body, 0o644)
		sum, _ := fileSHA256(filepath.Join(stage, name))
		_, err := db.Exec(`INSERT INTO files (submission, name, size, path, sha256, kind, description, tags, unpack, status, suggestion, info, created)
		    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1)`, sub, name, len(body), "files/s/"+name, sum, kind, descr, tags, unpack, FReady, sugg, "{}")
		if err != nil {
			t.Fatal(err)
		}
	}
	db.Exec("UPDATE submissions SET decided_by = '1' WHERE id = ?", sub)
	// a new machine below an existing one
	plus := "Rechner: Victor › Sirius 1 › Sirius 1 Plus\nArt: Betriebssysteme"
	disk := `{"group": "Sirius 1 Plus Systemdisketten", "tags": ["Rechner: Victor › Sirius 1 › Sirius 1 Plus", "Art: Betriebssysteme"], "reason": "drei Disketten eines Satzes"}`
	for i, n := range []string{"plus1.img", "plus2.img", "plus3.img"} {
		add(n, "disk", plus, "Diskette "+string(rune('1'+i)), disk, []byte(strings.Repeat(n, 100)), false)
	}
	add("handbuch.pdf", "pdf", "Rechner: DEC › PDP-11\nArt: Hersteller-Dokumentation", "PDP-11 Handbuch", `{"language": "de"}`, []byte("%PDF-1.4 handbuch"), false)
	// an archive that is unpacked
	tmp := t.TempDir()
	os.MkdirAll(filepath.Join(tmp, "src", "tools"), 0o755)
	os.WriteFile(filepath.Join(tmp, "src", "tools", "a.com"), []byte("AAA"), 0o644)
	os.WriteFile(filepath.Join(tmp, "src", "liesmich.txt"), []byte("hallo"), 0o644)
	if out, err := exec.Command("tar", "-cf", filepath.Join(stage, "tools.tar"), "-C", filepath.Join(tmp, "src"), ".").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	tarBody, _ := os.ReadFile(filepath.Join(stage, "tools.tar"))
	add("tools.tar", "archive", "Rechner: Victor › Sirius 1\nArt: Betriebssysteme", "Werkzeuge", "", tarBody, true)

	s, err := db.submission(sub)
	if err != nil {
		t.Fatal(err)
	}
	if fs, err := a.db.files("submission = ? AND status = ? ORDER BY id", sub, FReady); err != nil || len(fs) != 5 {
		t.Fatalf("staged files: %d %v", len(fs), err)
	}
	if err := a.doIntakeCatalog(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	q := func(query string, args ...any) []string {
		rows, err := a.cat.Query(query, args...)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var x string
			rows.Scan(&x)
			out = append(out, x)
		}
		return out
	}
	items := q("SELECT id || ' ' || title FROM items WHERE id IN (SELECT item FROM item_tags WHERE added_by LIKE 'einreichen:%') ORDER BY id")
	if len(items) != 3 {
		t.Fatalf("items: %q", items)
	}
	set := strings.SplitN(items[0], " ", 2)
	if set[1] != "Sirius 1 Plus Systemdisketten" {
		t.Errorf("disk set: %q", items[0])
	}
	files := q("SELECT path || ' | ' || descr FROM item_files WHERE item = ? ORDER BY ord", set[0])
	want := []string{storeDir(atoi(set[0])) + "/plus1.img | Diskette 1", storeDir(atoi(set[0])) + "/plus2.img | Diskette 2", storeDir(atoi(set[0])) + "/plus3.img | Diskette 3"}
	if strings.Join(files, "\n") != strings.Join(want, "\n") {
		t.Errorf("files of the set:\n%s", strings.Join(files, "\n"))
	}
	tagsOf := func(id string) string {
		ts := q("SELECT t.facet || ':' || t.name FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = ? ORDER BY 1", id)
		return strings.Join(ts, ", ")
	}
	if got := tagsOf(set[0]); got != "kind:Betriebssysteme, machine:Sirius 1 Plus, source:Einreichung" {
		t.Errorf("tags of the set: %q", got)
	}
	if got := q("SELECT p.name FROM tags t JOIN tags p ON p.id = t.parent WHERE t.name = 'Sirius 1 Plus'"); len(got) != 1 || got[0] != "Sirius 1" {
		t.Errorf("new tag below %q", got)
	}
	manual := strings.SplitN(items[1], " ", 2)
	if got := tagsOf(manual[0]); got != "kind:Hersteller-Dokumentation, language:de, machine:PDP-11, source:Einreichung" {
		t.Errorf("tags of the manual: %q", got)
	}
	if got := q("SELECT descr FROM items WHERE id = ?", manual[0]); got[0] != "PDP-11 Handbuch. eingereicht von toshi" {
		t.Errorf("descr %q", got)
	}
	tools := strings.SplitN(items[2], " ", 2)
	names := q("SELECT name FROM item_files WHERE item = ?", tools[0])
	sort.Strings(names)
	if strings.Join(names, ",") != "liesmich.txt,tools/a.com" {
		t.Errorf("unpacked: %q", names)
	}
	if got := tagsOf(tools[0]); got != "kind:Betriebssysteme, machine:Sirius 1, source:Einreichung" {
		t.Errorf("tags of the tools: %q", got)
	}
	// the new machine is recorded as made by the moderator
	if got := q("SELECT user FROM revisions WHERE op = 'create-tag' AND data LIKE '%Sirius 1 Plus%'"); len(got) != 1 || got[0] != "1" {
		t.Errorf("creation of the new tag: %q", got)
	}
	// the next sync leaves it all alone
	before := q("SELECT COUNT(*) FROM items")[0]
	syncer := &Syncer{cfg: a.cfg, c: a.cat, root: a.cfg.ArchiveRoot, jobs: 2, quiet: true}
	if err := syncer.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := q("SELECT COUNT(*) FROM items")[0]; after != before {
		t.Errorf("sync made %s items out of %s", after, before)
	}
	// the submission's page leads to the items
	db.Exec("UPDATE submissions SET state = ? WHERE id = ?", StDone, sub)
	page := get(t, a.routes(), "/einreichen/e/"+strconv.FormatInt(sub, 10)).Body.String()
	for _, want := range []string{`Diese Einreichung ist im Archiv: <a href="/a/`, `<a href="/a/` + set[0] + `">A` + set[0] + ` Sirius 1 Plus Systemdisketten</a>`, `→ <a href="/a/` + manual[0] + `">Objekt A` + manual[0] + `</a>`} {
		if !strings.Contains(page, want) {
			i := strings.Index(page, "im Archiv")
			t.Errorf("submission page: %q missing in %q", want, page[max(0, i-50):min(len(page), i+300)])
		}
	}
	var taken int
	a.db.QueryRow("SELECT COUNT(*) FROM files WHERE submission = ? AND status = ?", sub, FTaken).Scan(&taken)
	if taken != 5 {
		t.Errorf("%d files taken in", taken)
	}
}

func atoi(s string) int64 {
	var n int64
	for _, c := range s {
		n = n*10 + int64(c-'0')
	}
	return n
}

// An archive split at intake: each top folder and loose file is an item of
// its own, with the tags of the file and its contents below it.
func TestIntakeSplit(t *testing.T) {
	a := siteApp(t)
	a.cfg.BackupDir = t.TempDir()
	a.cfg.BinDir = t.TempDir()
	a.cfg.ReindexLock = filepath.Join(t.TempDir(), "lock")
	res, _ := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated, decided_by) VALUES ('2', 'Pro', ?, 1, 1, '1')", StIntake)
	sub, _ := res.LastInsertId()
	stage := filepath.Join(a.cfg.StagingDir, "files", "p")
	os.MkdirAll(stage, 0o755)
	tmp := t.TempDir()
	for _, n := range []string{"sammlung/basic/disk1.dsk", "sammlung/basic/disk2.dsk", "sammlung/logo/logo.dsk", "sammlung/liesmich.txt"} {
		os.MkdirAll(filepath.Join(tmp, filepath.Dir(n)), 0o755)
		os.WriteFile(filepath.Join(tmp, n), []byte(n), 0o644)
	}
	if out, err := exec.Command("tar", "-cf", filepath.Join(stage, "pro.tar"), "-C", tmp, "sammlung").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	body, _ := os.ReadFile(filepath.Join(stage, "pro.tar"))
	sum, _ := fileSHA256(filepath.Join(stage, "pro.tar"))
	if _, err := a.db.Exec(`INSERT INTO files (submission, name, size, path, sha256, kind, tags, unpack, split, status, info, created)
	    VALUES (?, 'pro.tar', ?, 'files/p/pro.tar', ?, 'archive', 'Rechner: Victor › Sirius 1', 1, 1, ?, '{"single_root": "sammlung"}', 1)`, sub, len(body), sum, FReady); err != nil {
		t.Fatal(err)
	}
	s, _ := a.db.submission(sub)
	if err := a.doIntakeCatalog(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	rows, _ := a.cat.Query(`SELECT i.title || ': ' || group_concat(f.name, ',') FROM items i JOIN item_files f ON f.item = i.id
	    WHERE i.id IN (SELECT item FROM item_tags WHERE added_by LIKE 'einreichen:%') GROUP BY i.id ORDER BY i.title`)
	var got []string
	for rows.Next() {
		var x string
		rows.Scan(&x)
		got = append(got, x)
	}
	rows.Close()
	want := "basic: disk1.dsk,disk2.dsk | liesmich.txt: liesmich.txt | logo: logo.dsk"
	if strings.Join(got, " | ") != want {
		t.Errorf("items: %q", got)
	}
	var tagged int
	a.cat.QueryRow(`SELECT COUNT(DISTINCT it.item) FROM item_tags it JOIN tags t ON t.id = it.tag WHERE t.name = 'Sirius 1' AND it.added_by LIKE 'einreichen:%'`).Scan(&tagged)
	if tagged != 3 {
		t.Errorf("%d items tagged", tagged)
	}
}

// An item that is one archive is split: an item per top folder, a loose
// file named like a folder goes with it, the other loose files are items
// of their own; the old item is withdrawn.
func TestSplitItem(t *testing.T) {
	a := siteApp(t)
	a.cfg.BackupDir = t.TempDir()
	a.cfg.BinDir = t.TempDir()
	a.cfg.ReindexLock = filepath.Join(t.TempDir(), "lock")
	res, _ := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated, decided_by) VALUES ('2', 'Pro', ?, 1, 1, '1')", StIntake)
	sub, _ := res.LastInsertId()
	stage := filepath.Join(a.cfg.StagingDir, "files", "q")
	os.MkdirAll(stage, 0o755)
	tmp := t.TempDir()
	for _, n := range []string{"basic/b1.dsk", "basic/b2.dsk", "logo/l.dsk", "basic.zip", "girls.zip"} {
		os.MkdirAll(filepath.Join(tmp, filepath.Dir(n)), 0o755)
		os.WriteFile(filepath.Join(tmp, n), []byte(n), 0o644)
	}
	if out, err := exec.Command("tar", "-cf", filepath.Join(stage, "pro.tar"), "-C", tmp, ".").CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	body, _ := os.ReadFile(filepath.Join(stage, "pro.tar"))
	sum, _ := fileSHA256(filepath.Join(stage, "pro.tar"))
	a.db.Exec(`INSERT INTO files (submission, name, size, path, sha256, kind, tags, status, info, created)
	    VALUES (?, 'pro.tar', ?, 'files/q/pro.tar', ?, 'archive', 'Rechner: Victor › Sirius 1', ?, '{}', 1)`, sub, len(body), sum, FReady)
	s, _ := a.db.submission(sub)
	if err := a.doIntakeCatalog(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	var old int64
	a.cat.QueryRow("SELECT item FROM item_files WHERE name = 'pro.tar'").Scan(&old)
	ids, err := a.splitItem(context.Background(), old, "1")
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, id := range ids {
		var x string
		a.cat.QueryRow(`SELECT i.title || ': ' || group_concat(f.name, ',') FROM items i JOIN item_files f ON f.item = i.id WHERE i.id = ? GROUP BY i.id`, id).Scan(&x)
		got = append(got, x)
	}
	sort.Strings(got)
	if strings.Join(got, " | ") != "basic: b1.dsk,b2.dsk,basic.zip | girls.zip: girls.zip | logo: l.dsk" {
		t.Errorf("items: %q", got)
	}
	var state string
	var tagged int
	a.cat.QueryRow("SELECT state FROM items WHERE id = ?", old).Scan(&state)
	a.cat.QueryRow("SELECT COUNT(*) FROM item_tags it JOIN tags t ON t.id = it.tag WHERE t.name = 'Sirius 1' AND it.item IN (?, ?, ?)", ids[0], ids[1], ids[2]).Scan(&tagged)
	if state != "withdrawn" || tagged != 3 {
		t.Errorf("old %s, %d tagged", state, tagged)
	}
	// the submission still tells what became of it
	a.db.Exec("UPDATE submissions SET state = ? WHERE id = ?", StDone, sub)
	page := get(t, a.routes(), "/einreichen/e/"+strconv.FormatInt(sub, 10)).Body.String()
	for _, want := range []string{"aufgeteilt in 3 Objekte", "Die 3 Objekte aus A", `<a href="/a/` + strconv.FormatInt(ids[0], 10) + `">`} {
		if !strings.Contains(page, want) {
			t.Errorf("submission page: %q missing", want)
		}
	}
}
