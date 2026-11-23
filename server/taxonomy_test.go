package archiv

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestParseRulesAndMatch(t *testing.T) {
	f := filepath.Join(t.TempDir(), "map.conf")
	os.WriteFile(f, []byte(`# comment
public/pub/rechner/**/manuals   kind=Hersteller-Dokumentation
*.pdf under public/pub/rechner/**   kind=Firmware / EPROM-Images machine=DEC > VAX > VAX-11/780 weak
Zeitschriften/_*/*   kind=Zeitschrift publication=$1 > $2
`), 0o644)
	rules, err := parseRules(f)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 3 {
		t.Fatalf("%d rules", len(rules))
	}
	r := rules[1]
	want := []RuleTag{{"kind", "Firmware / EPROM-Images"}, {"machine", "DEC > VAX > VAX-11/780"}}
	if !r.Weak || r.Glob != "*.pdf" || !reflect.DeepEqual(r.Tags, want) {
		t.Errorf("rule %+v", r)
	}
	for _, c := range []struct {
		rule      int
		dir, name string
		ok        bool
		caps      []string
	}{
		{0, "public/pub/rechner/dec/manuals", "x.pdf", true, []string{}},
		{0, "public/pub/rechner/dec/vax/Manuals/sub", "x.pdf", true, []string{}},
		{0, "public/pub/rechner/dec", "x.pdf", false, nil},
		{1, "public/pub/rechner", "X.PDF", true, []string{}},
		{1, "public/pub/rechner/dec", "x.img", false, nil},
		{2, "Zeitschriften/_Commodore/64er/1984", "a.pdf", true, []string{"Zeitschriften/_Commodore", "Zeitschriften/_Commodore/64er"}},
		{2, "Zeitschriften/ACE", "a.pdf", false, nil},
	} {
		caps, _, ok := rules[c.rule].matchAt(c.dir, c.name)
		if ok != c.ok || (ok && !reflect.DeepEqual(caps, c.caps)) {
			t.Errorf("rule %d on %s/%s: %v %q", c.rule, c.dir, c.name, ok, caps)
		}
	}
}

func TestParseTaxonomy(t *testing.T) {
	es, err := parseTaxonomy("testdata/conf/taxonomy.conf")
	if err != nil {
		t.Fatal(err)
	}
	var sirius TaxEntry
	for _, e := range es {
		if e.Facet == "machine" && len(e.Path) == 2 && e.Path[1] == "Sirius 1" {
			sirius = e
		}
	}
	if !reflect.DeepEqual(sirius.Path, []string{"Victor", "Sirius 1"}) {
		t.Errorf("sirius %+v", sirius)
	}
	if es[2].Path[0] != "Firmware / EPROM-Images" || !es[2].Section || es[2].Sort != 30 {
		t.Errorf("entry %+v", es[2])
	}
}

func TestSlugify(t *testing.T) {
	for in, want := range map[string]string{"Sirius 1": "sirius-1", "Sekundärliteratur": "sekundaerliteratur",
		"Firmware / EPROM-Images": "firmware-eprom-images", "VAX-11/780": "vax-11-780", "64'er": "64er", "Café": "cafe"} {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNaturalLess(t *testing.T) {
	names := []string{"PDP-10", "PDP-3", "pdp-11", "DECmate", "PDP-1"}
	sort.Slice(names, func(i, j int) bool { return naturalLess(names[i], names[j]) })
	if strings.Join(names, ",") != "DECmate,PDP-1,PDP-3,PDP-10,pdp-11" {
		t.Errorf("%v", names)
	}
}

func TestExpandBraces(t *testing.T) {
	got := expandBraces("*.{jpg,png} under a/**/{soft,tools}   kind=Fotos")
	want := []string{"*.jpg under a/**/soft   kind=Fotos", "*.jpg under a/**/tools   kind=Fotos",
		"*.png under a/**/soft   kind=Fotos", "*.png under a/**/tools   kind=Fotos"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("%q", got)
	}
}
