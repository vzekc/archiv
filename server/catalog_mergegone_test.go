package archiv

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMergeGone(t *testing.T) {
	st := newSyncTest(t)
	st.sync()
	removed, kept := sirius+"prospekt ä.pdf", sirius+"sirius1.pdf"
	from := st.one("SELECT item FROM item_files WHERE path = ?", removed)
	into := st.one("SELECT item FROM item_files WHERE path = ?", kept)

	// a removed file that is still on disk is refused, and nothing changes
	if err := mergeGone(st.c, st.root, "7", "Dublette", [][2]string{{removed, kept}}); err == nil || !strings.Contains(err.Error(), "still on disk") {
		t.Fatalf("err %v", err)
	}
	if err := os.Remove(filepath.Join(st.root, removed)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1100 * time.Millisecond) // file_paths.seen has whole seconds
	st.sync()
	if got := st.one("SELECT state FROM items WHERE id = ?", from); got != "missing" {
		t.Fatalf("state after sync %q", got)
	}
	// an unknown kept path rolls back the whole map
	bad := [][2]string{{removed, kept}, {sirius + "nicht da.pdf", kept}}
	if err := mergeGone(st.c, st.root, "7", "Dublette", bad); err == nil {
		t.Fatal("bad map accepted")
	}
	if got := st.one("SELECT state FROM items WHERE id = ?", from); got != "missing" {
		t.Fatalf("state after failed map %q", got)
	}

	if err := mergeGone(st.c, st.root, "7", "Dublette", [][2]string{{removed, kept}}); err != nil {
		t.Fatal(err)
	}
	if got := st.one("SELECT state || ' ' || merged_into FROM items WHERE id = ?", from); got != "merged "+into {
		t.Errorf("removed item %q", got)
	}
	if got := st.one("SELECT item || ' ' || name FROM redirects WHERE old_path = ?", removed); got != into+" sirius1.pdf" {
		t.Errorf("redirect %q", got)
	}
	if got := st.one("SELECT COUNT(*) FROM item_files WHERE item = ?", from); got != "0" {
		t.Errorf("removed item still has %s files", got)
	}
	if got := st.one("SELECT COUNT(*) FROM item_files WHERE item = ?", into); got != "1" {
		t.Errorf("kept item has %s files", got)
	}
	if got := st.one("SELECT rev FROM items WHERE id = ?", into); got != "0" {
		t.Errorf("kept item rev %s: it would no longer follow the descriptions", got)
	}
	if got := st.one("SELECT op || ' ' || user FROM revisions WHERE target = ?", "item:"+from); got != "merge 7" {
		t.Errorf("history %q", got)
	}

	// the next sync leaves the merge alone
	time.Sleep(1100 * time.Millisecond)
	st.sync()
	if got := st.one("SELECT state FROM items WHERE id = ?", from); got != "merged" {
		t.Errorf("state after the next sync %q", got)
	}
	if got := st.one("SELECT state FROM items WHERE id = ?", into); got != "live" {
		t.Errorf("kept item state %q", got)
	}
}
