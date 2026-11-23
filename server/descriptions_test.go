package archiv

import (
	"os"
	"reflect"
	"sort"
	"testing"
)

func TestParseDescriptions(t *testing.T) {
	raw := []byte("Title= Victor Sirius 1\r\nboot disk 1.img Startdiskette\r\n\r\ntitle= zweiter Titel\r\nprospekt \xc4.pdf Prospekt \xe4\r\n")
	title, lines := parseDescriptions(raw)
	if title != "Victor Sirius 1" {
		t.Errorf("title %q", title)
	}
	want := []string{"boot disk 1.img Startdiskette", "title= zweiter Titel", "prospekt Ä.pdf Prospekt ä"}
	if !reflect.DeepEqual(lines, want) {
		t.Errorf("lines %q, want %q", lines, want)
	}
}

func TestMatchEntriesTestdata(t *testing.T) {
	dir := "testdata/archive/public/pub/rechner/victor/sirius1"
	title, lines := readDescriptions(dir)
	if title != "Victor Sirius 1" {
		t.Errorf("title %q", title)
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	got := matchEntries(lines, names)
	want := map[string]string{
		"sirius1_service_manual.pdf": "Service-Handbuch, 1983",
		"sirius1.pdf":                "Kurzbeschreibung",
		"boot disk 1.img":            "Startdiskette Nr. 1",
		"cpm86":                      "CP/M-86 für den Sirius",
		"prospekt ä.pdf":             "Werbeprospekt mit Umlaut",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestMatchEntriesLongestName(t *testing.T) {
	got := matchEntries([]string{"disk 1 2.img zwei Wörter", "DISK 1 erste", "disk1x nichts"},
		[]string{"disk 1", "disk 1 2.img", "disk1"})
	want := map[string]string{"disk 1 2.img": "zwei Wörter", "disk 1": "erste"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestMatchEntriesDitto(t *testing.T) {
	got := matchEntries([]string{"a.lbr Z3COM Teil 1", "b.lbr \"", "c.lbr  \"\"", "d.lbr eigen", "e.arc Small C compiler Part 1/3", "f.arc \" \" \" Part 2/3"},
		[]string{"a.lbr", "b.lbr", "c.lbr", "d.lbr", "e.arc", "f.arc"})
	want := map[string]string{"a.lbr": "Z3COM Teil 1", "b.lbr": "Z3COM Teil 1", "c.lbr": "Z3COM Teil 1", "d.lbr": "eigen",
		"e.arc": "Small C compiler Part 1/3", "f.arc": "Small C compiler Part 2/3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q", got)
	}
}
