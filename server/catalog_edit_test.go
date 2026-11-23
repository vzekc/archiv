package archiv

import (
	"errors"
	"testing"
)

func editTest(t *testing.T) (*syncTest, int64, int64) {
	st := newSyncTest(t)
	st.sync()
	var a, b int64
	st.c.QueryRow("SELECT item FROM item_files WHERE path = ?", sirius+"sirius1.pdf").Scan(&a)
	st.c.QueryRow("SELECT item FROM item_files WHERE path = ?", sirius+"sirius1_service_manual.pdf").Scan(&b)
	return st, a, b
}

func TestEditSetFieldAndConflict(t *testing.T) {
	st, a, _ := editTest(t)
	c := st.c
	ch := Change{Target: targetOf("item", a), Op: "set-field", Data: map[string]any{"field": "title", "old": "Kurzbeschreibung", "new": "Victor Sirius 1, Kurzbeschreibung"}, Rev: 0}
	if _, err := c.Apply("7", []Change{ch}); err != nil {
		t.Fatal(err)
	}
	if got := st.one("SELECT title || '|' || rev FROM items WHERE id = ?", a); got != "Victor Sirius 1, Kurzbeschreibung|1" {
		t.Errorf("after edit: %q", got)
	}
	// a second edit based on revision 0 is refused, and says by whom
	ch.Data = map[string]any{"field": "descr", "new": "x"}
	_, err := c.Apply("8", []Change{ch})
	var ce *ConflictError
	if !errors.As(err, &ce) || ce.User != "7" {
		t.Errorf("stale edit: %v", err)
	}
	// a sync leaves the edited title alone
	st.sync()
	if got := st.one("SELECT title FROM items WHERE id = ?", a); got != "Victor Sirius 1, Kurzbeschreibung" {
		t.Errorf("sync overwrote the edit: %q", got)
	}
	// and revert brings the old one back
	id := st.one("SELECT id FROM revisions WHERE target = ? AND state = 'applied'", targetOf("item", a))
	var rid int64
	st.c.QueryRow("SELECT ?", id).Scan(&rid)
	if err := c.Revert("9", []int64{rid}, "versehentlich"); err != nil {
		t.Fatal(err)
	}
	if got := st.one("SELECT title FROM items WHERE id = ?", a); got != "Kurzbeschreibung" {
		t.Errorf("after revert: %q", got)
	}
	if got := st.one("SELECT state FROM revisions WHERE id = ?", rid); got != "reverted" {
		t.Errorf("reverted revision: %q", got)
	}
}

func TestEditProposeDecide(t *testing.T) {
	st, a, _ := editTest(t)
	c := st.c
	var dec int64
	c.QueryRow("SELECT id FROM tags WHERE facet = 'machine' AND name = 'PDP-11'").Scan(&dec)
	if _, err := c.Propose("12", []Change{
		{Target: targetOf("item", a), Op: "add-tag", Data: map[string]any{"tag": dec, "by": "12"}, Rev: 0},
		{Target: targetOf("item", a), Op: "set-field", Data: map[string]any{"field": "year", "old": "", "new": "1983"}, Rev: 0},
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.one("SELECT COUNT(*) FROM revisions WHERE state = 'proposed'"); got != "2" {
		t.Fatalf("proposals: %s", got)
	}
	if got := st.one("SELECT year FROM items WHERE id = ?", a); got != "" {
		t.Errorf("a proposal changed the item")
	}
	ids := st.strings("SELECT id FROM revisions WHERE state = 'proposed' ORDER BY id")
	var first, second int64
	c.QueryRow("SELECT ?", ids[0]).Scan(&first)
	c.QueryRow("SELECT ?", ids[1]).Scan(&second)
	if err := c.Decide("1", []int64{first}, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Decide("1", []int64{second}, true); err != nil {
		t.Fatal(err)
	}
	if got := st.one("SELECT year FROM items WHERE id = ?", a); got != "1983" {
		t.Errorf("accepted proposal: year %q", got)
	}
	if got := st.one("SELECT COUNT(*) FROM item_tags WHERE item = ? AND tag = ?", a, dec); got != "0" {
		t.Errorf("rejected proposal applied")
	}
	// a proposal whose old value no longer holds stays open
	c.Propose("12", []Change{{Target: targetOf("item", a), Op: "set-field", Data: map[string]any{"field": "year", "old": "", "new": "1984"}, Rev: 1}})
	var third int64
	c.QueryRow("SELECT id FROM revisions WHERE state = 'proposed'").Scan(&third)
	if err := c.Decide("1", []int64{third}, true); !errors.Is(err, errConflict) {
		t.Errorf("stale proposal: %v", err)
	}
}

func TestEditMergeAndTags(t *testing.T) {
	st, a, b := editTest(t)
	c := st.c
	if _, err := c.Apply("1", []Change{{Target: targetOf("tag", 0), Op: "create-tag", Data: map[string]any{"facet": "system", "name": "CP/M-86"}, Rev: -1}}); err != nil {
		t.Fatal(err)
	}
	var cpm int64
	c.QueryRow("SELECT id FROM tags WHERE facet = 'system' AND name = 'CP/M-86'").Scan(&cpm)
	if cpm == 0 || st.one("SELECT target FROM revisions WHERE op = 'create-tag'") != targetOf("tag", cpm) {
		t.Fatalf("create-tag: %d", cpm)
	}
	if _, err := c.Apply("1", []Change{
		{Target: targetOf("item", a), Op: "add-tag", Data: map[string]any{"tag": cpm, "by": "1"}, Rev: 0},
		{Target: targetOf("item", a), Op: "merge", Data: map[string]any{"into": b}, Rev: -1},
	}); err != nil {
		t.Fatal(err)
	}
	if got := st.one("SELECT state || ' ' || merged_into FROM items WHERE id = ?", a); got != "merged "+st.one("SELECT ?", b) {
		t.Errorf("merged item: %q", got)
	}
	if got := st.one("SELECT COUNT(*) FROM item_files WHERE item = ?", b); got != "2" {
		t.Errorf("files after merge: %s", got)
	}
	if got := st.one("SELECT COUNT(*) FROM item_tags WHERE item = ? AND tag = ?", b, cpm); got != "1" {
		t.Errorf("tag not carried over")
	}
	// the next sync keeps the file with the item it was merged into
	st.sync()
	if got := st.one("SELECT item FROM item_files WHERE path = ?", sirius+"sirius1.pdf"); got != st.one("SELECT ?", b) {
		t.Errorf("after sync the file is in item %s", got)
	}
	// a tag renamed gets a new slug
	if _, err := c.Apply("1", []Change{{Target: targetOf("tag", cpm), Op: "set-field", Data: map[string]any{"field": "name", "old": "CP/M-86", "new": "CP/M-86 1.1"}, Rev: 1}}); err != nil {
		t.Fatal(err)
	}
	if got := st.one("SELECT slug FROM tags WHERE id = ?", cpm); got != "cp-m-86-1.1" {
		t.Errorf("slug %q", got)
	}
}
