package archiv

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

// A moderator takes a submission in with tags: the group's tags, or a
// file's own; a tag that does not exist needs its facet.
func TestAcceptWithTags(t *testing.T) {
	a := siteApp(t)
	h := a.routes()
	res, err := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated, claimed_by, claimed_at) VALUES ('2', 'Sirius', ?, 1, 1, '1', 1)", StModeration)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := res.LastInsertId()
	sugg := `{"group": "Systemdisketten", "tags": ["Rechner: Victor › Sirius 1", "Art: Betriebssysteme", "Rechner: Victor › Sirius 1 › Sirius 1 Plus"],
		"new_tags": ["Rechner: Victor › Sirius 1 › Sirius 1 Plus"], "description": "Diskette"}`
	var ids []int64
	for _, n := range []string{"a.img", "b.img"} {
		r, err := a.db.Exec(`INSERT INTO files (submission, name, size, path, sha256, kind, status, suggestion, created)
		    VALUES (?, ?, 1, ?, ?, 'disk', ?, ?, 1)`, sub, n, "files/s/"+n, n, FReady, sugg)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := r.LastInsertId()
		ids = append(ids, id)
	}
	page := fmt.Sprintf("/einreichen/e/%d", sub)
	body := get(t, h, page).Body.String()
	if strings.Index(body, `<h2>Moderation</h2>`) > strings.Index(body, `Die Dateien im Einzelnen`) || !strings.Contains(body, `href="#g0">Schlagworte und Beschreibungen bearbeiten`) {
		t.Errorf("the moderator who took it over does not get the form first")
	}
	for _, want := range []string{`name="gtag_0" value="Rechner: Victor › Sirius 1"`, `Sirius 1 Plus <i>neu</i>`, `<datalist id="tags">`} {
		if !strings.Contains(body, want) {
			t.Errorf("form: %q missing", want)
		}
	}
	form := func(extra url.Values) url.Values {
		v := url.Values{}
		for _, id := range ids {
			k := fmt.Sprint(id)
			v.Set("take_"+k, "1")
			v.Set("grp_"+k, "0")
		}
		v["gtag_0"] = []string{"Rechner: Victor › Sirius 1", "Art: Betriebssysteme", ""}
		for k, x := range extra {
			v[k] = x
		}
		return v
	}

	// a tag without its facet that does not exist is refused
	w := postForm(t, h, page+"/aufnehmen", form(url.Values{"ftag_" + fmt.Sprint(ids[1]): {"Gibtsnicht"}}), "")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "Gibtsnicht") {
		t.Errorf("unknown tag: %d", w.Code)
	}
	var state string
	a.db.QueryRow("SELECT state FROM submissions WHERE id = ?", sub).Scan(&state)
	if state != StModeration {
		t.Fatalf("state after a refused form: %s", state)
	}

	// the second file gets tags of its own, one of them new
	w = postForm(t, h, page+"/aufnehmen", form(url.Values{"ftag_" + fmt.Sprint(ids[1]): {"Rechner: Victor > Sirius 1 > Sirius 1 Plus", " "}}), "")
	if w.Code != 303 {
		t.Fatalf("accept: %d %s", w.Code, w.Body.String())
	}
	got := map[int64]string{}
	for _, id := range ids {
		var tags string
		a.db.QueryRow("SELECT tags FROM files WHERE id = ?", id).Scan(&tags)
		got[id] = tags
	}
	if got[ids[0]] != "Rechner: Victor › Sirius 1\nArt: Betriebssysteme" {
		t.Errorf("tags of the group: %q", got[ids[0]])
	}
	if got[ids[1]] != "Rechner: Victor › Sirius 1 › Sirius 1 Plus" {
		t.Errorf("tags of its own: %q", got[ids[1]])
	}
	a.db.QueryRow("SELECT state FROM submissions WHERE id = ?", sub).Scan(&state)
	if state != StIntake {
		t.Errorf("state: %s", state)
	}
}

// The AI's answer is checked against the vocabulary: existing tags pass, a
// new machine needs an existing parent, kinds come from the list.
func TestValidateTags(t *testing.T) {
	a := siteApp(t)
	files := []*File{{Name: "a.pdf"}, {Name: "b.img"}}
	answer := func(group string) string {
		return `{"summary": "", "notes_for_moderator": "", "groups": [` + group + `]}`
	}
	g := func(machine string, isNew bool, kind string, files string) string {
		return fmt.Sprintf(`{"label": "x", "machine": %q, "new_machine": %v, "kind": %q, "publication": "", "system": "",
			"confidence": 0.9, "reason": "", "notes_for_moderator": "", "unpack": false, "files": [%s]}`, machine, isNew, kind, files)
	}
	both := `{"file": "a.pdf", "description": "", "language": "de", "note": ""}, {"file": "b.img", "description": "", "language": "", "note": ""}`

	an, problems := a.ai.validate(answer(g("Victor > Sirius 1", false, "Hersteller-Dokumentation", both)), files, "m")
	if len(problems) > 0 {
		t.Fatalf("problems: %q", problems)
	}
	if fmt.Sprint(an.Files[0].Tags) != "[Rechner: Victor › Sirius 1 Art: Hersteller-Dokumentation]" || an.Files[0].Language != "de" || len(an.Files[0].NewTags) != 0 {
		t.Errorf("suggestion: %+v", an.Files[0])
	}

	an, problems = a.ai.validate(answer(g("Victor > Sirius 1 > Sirius 1 Plus", true, "", both)), files, "m")
	if len(problems) > 0 || fmt.Sprint(an.Files[1].NewTags) != "[Rechner: Victor › Sirius 1 › Sirius 1 Plus]" {
		t.Errorf("new machine: %q %+v", problems, an)
	}

	for _, c := range []struct{ machine, kind string }{
		{"Victor > Sirius 9", ""},      // not there, not marked new
		{"Kaypro > Kaypro II", ""},     // marked new, but no parent (a manufacturer alone may be new)
		{"Victor > Sirius 1", "Comic"}, // no such kind
	} {
		_, problems := a.ai.validate(answer(g(c.machine, c.machine == "Kaypro > Kaypro II", c.kind, both)), files, "m")
		if len(problems) == 0 {
			t.Errorf("%s / %s: accepted", c.machine, c.kind)
		}
	}
	if an, problems := a.ai.validate(answer(g("Commodore", true, "", both)), files, "m"); len(problems) > 0 || fmt.Sprint(an.Files[0].NewTags) != "[Rechner: Commodore]" {
		t.Errorf("new manufacturer: %q %+v", problems, an)
	}
	if _, problems := a.ai.validate(answer(g("Victor", false, "", `{"file": "a.pdf", "description": "", "language": "", "note": ""}`)), files, "m"); len(problems) != 1 || !strings.Contains(problems[0], "b.img") {
		t.Errorf("missing file: %q", problems)
	}
}

// A database of the folder era gets tags in place of folders.
func TestUpgradeDB(t *testing.T) {
	p := filepath.Join(t.TempDir(), "old.sqlite")
	old, err := sql.Open("sqlite", "file:"+p)
	if err != nil {
		t.Fatal(err)
	}
	// the schema as it was: a folder per file, an area per submission,
	// proposed folders
	folders := strings.NewReplacer(
		"tags         TEXT", "target       TEXT",
		"owner         TEXT NOT NULL,", "owner         TEXT NOT NULL, area_hint TEXT NOT NULL DEFAULT '',",
	).Replace(schema)
	if !strings.Contains(folders, "target ") || !strings.Contains(folders, "area_hint") {
		t.Fatal("old schema not built")
	}
	for _, q := range []string{
		folders,
		`CREATE TABLE proposals (id INTEGER PRIMARY KEY, submission INTEGER NOT NULL, parent TEXT NOT NULL, name TEXT NOT NULL)`,
		`INSERT INTO submissions (id, owner, area_hint, created, updated) VALUES (1, '2', 'public/pub', 1, 1)`,
		`INSERT INTO files (submission, name, size, target, created) VALUES (1, 'a.pdf', 1, 'public/pub/rechner', 1)`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	old.Close()
	db, err := openDB(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var tags string
	if err := db.QueryRow("SELECT tags FROM files").Scan(&tags); err != nil || tags != "" {
		t.Errorf("tags %q %v", tags, err)
	}
	var n int
	db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name = 'proposals'").Scan(&n)
	if n != 0 {
		t.Errorf("proposals still there")
	}
	db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('submissions') WHERE name = 'area_hint'").Scan(&n)
	if n != 0 {
		t.Errorf("area_hint still there")
	}
}

// Keywords of the AI become "Stichwort:" tags, new ones marked.
func TestValidateKeywords(t *testing.T) {
	a := siteApp(t)
	files := []*File{{Name: "a.pdf"}}
	an, problems := a.ai.validate(`{"summary": "", "notes_for_moderator": "", "groups": [{"label": "x", "machine": "Victor > Sirius 1", "new_machine": false,
		"kind": "", "publication": "", "system": "", "keywords": ["Z80", " Assembler "], "confidence": 0.9, "reason": "", "notes_for_moderator": "", "unpack": false,
		"files": [{"file": "a.pdf", "description": "", "language": "", "note": ""}]}]}`, files, "m")
	if len(problems) > 0 {
		t.Fatal(problems)
	}
	if got := fmt.Sprint(an.Files[0].Tags, an.Files[0].NewTags); got != "[Rechner: Victor › Sirius 1 Stichwort: Z80 Stichwort: Assembler] [Stichwort: Z80 Stichwort: Assembler]" {
		t.Errorf("%s", got)
	}
}
