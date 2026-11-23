package archiv

import (
	"bufio"
	"database/sql"
	"fmt"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// Facets of the tag vocabulary. location is generated from the folder tree.
var facets = []string{"machine", "kind", "publication", "event", "system", "language", "period", "source", "group", "keyword", "location"}

func isFacet(s string) bool {
	for _, f := range facets {
		if f == s {
			return true
		}
	}
	return false
}

// tagSep separates the levels of a tag path in the configuration files:
// "DEC > VAX > VAX-11/780". Names may contain slashes.
const tagSep = " > "

// TaxEntry is one tag of taxonomy.conf.
type TaxEntry struct {
	Facet   string
	Path    []string // names from the top level down
	Section bool
	Sort    int
}

// parseTaxonomy reads taxonomy.conf: a [facet] header, then one tag per
// line, indented by two blanks per level below its parent; after the name
// and at least two blanks, the flags "section" and "sort=N".
func parseTaxonomy(file string) ([]TaxEntry, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []TaxEntry
	facet := ""
	var stack []string
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		raw := strings.TrimRight(sc.Text(), " \t\r")
		trimmed := strings.TrimLeft(raw, " ")
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(raw, "[") && strings.HasSuffix(raw, "]") {
			facet = raw[1 : len(raw)-1]
			if !isFacet(facet) {
				return nil, fmt.Errorf("%s:%d: unknown facet %q", file, n, facet)
			}
			stack = nil
			continue
		}
		if facet == "" {
			return nil, fmt.Errorf("%s:%d: tag before the first [facet]", file, n)
		}
		indent := len(raw) - len(trimmed)
		if indent%2 != 0 || indent/2 > len(stack) {
			return nil, fmt.Errorf("%s:%d: indent by two blanks per level", file, n)
		}
		stack = stack[:indent/2]
		parts := multiBlank.Split(trimmed, -1)
		e := TaxEntry{Facet: facet}
		for _, flag := range parts[1:] {
			for _, fl := range strings.Fields(flag) {
				switch {
				case fl == "section":
					e.Section = true
				case strings.HasPrefix(fl, "sort="):
					if e.Sort, err = strconv.Atoi(fl[5:]); err != nil {
						return nil, fmt.Errorf("%s:%d: %v", file, n, err)
					}
				default:
					return nil, fmt.Errorf("%s:%d: unknown flag %q", file, n, fl)
				}
			}
		}
		stack = append(stack, parts[0])
		e.Path = append([]string(nil), stack...)
		out = append(out, e)
	}
	return out, sc.Err()
}

var multiBlank = regexp.MustCompile(`\s{2,}|\t+`)

// Rule is one line of legacy-map.conf: the tags it gives to every item
// below a folder.
type Rule struct {
	Line    int
	Text    string
	Pattern []string // folder path segments; "*" is one segment, "**" any number
	Glob    string   // optional: only files whose name matches
	Tags    []RuleTag
	Weak    bool // a guess from the folder alone, the AI may replace it
}

type RuleTag struct {
	Facet string
	Path  string // "DEC > VAX", may refer to $1, $2 ... (the "*" segments)
}

var assignRe = regexp.MustCompile(`(?:^|\s)(` + strings.Join(facets, "|") + `)=`)

// parseRules reads legacy-map.conf. A line is
//
//	[<glob> under ]<folder pattern>  facet=Tag > Path [facet=...] [weak]
//
// where {a,b} stands for alternatives, each a rule of its own.
// Rules are applied in order and all matching rules apply, but for the
// kind, where the rule naming the deepest folder wins (applier.tagItem).
// $1, $2 ... in a tag stand for the folders matched by the wildcard
// segments, by their title.
func parseRules(file string) ([]Rule, error) {
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []Rule
	sc := bufio.NewScanner(f)
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for _, one := range expandBraces(line) {
			r, err := parseRule(file, n, line, one)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	return out, sc.Err()
}

// expandBraces writes out the alternatives of a line: "*.{jpg,png}" is two
// lines, "*.jpg" and "*.png".
func expandBraces(s string) []string {
	open := strings.IndexByte(s, '{')
	end := strings.IndexByte(s, '}')
	if open < 0 || end < open {
		return []string{s}
	}
	var out []string
	for _, alt := range strings.Split(s[open+1:end], ",") {
		out = append(out, expandBraces(s[:open]+alt+s[end+1:])...)
	}
	return out
}

// parseRule reads one rule of legacy-map.conf, its braces written out.
func parseRule(file string, n int, text, line string) (Rule, error) {
	r := Rule{Line: n, Text: text}
	if rest, ok := strings.CutSuffix(line, " weak"); ok {
		r.Weak = true
		line = strings.TrimSpace(rest)
	}
	loc := assignRe.FindAllStringSubmatchIndex(line, -1)
	if len(loc) == 0 {
		return r, fmt.Errorf("%s:%d: no facet=tag", file, n)
	}
	head := strings.TrimSpace(line[:loc[0][0]])
	for i, m := range loc {
		end := len(line)
		if i+1 < len(loc) {
			end = loc[i+1][0]
		}
		val := strings.TrimSpace(line[m[1]:end])
		if val == "" {
			return r, fmt.Errorf("%s:%d: empty %s", file, n, line[m[2]:m[3]])
		}
		r.Tags = append(r.Tags, RuleTag{Facet: line[m[2]:m[3]], Path: val})
	}
	if g, p, ok := strings.Cut(head, " under "); ok {
		r.Glob, head = strings.TrimSpace(g), strings.TrimSpace(p)
		if _, err := path.Match(r.Glob, ""); err != nil {
			return r, fmt.Errorf("%s:%d: %v", file, n, err)
		}
	}
	if head == "" {
		return r, fmt.Errorf("%s:%d: no folder", file, n)
	}
	r.Pattern = strings.Split(strings.Trim(head, "/"), "/")
	return r, nil
}

// matchAt tells whether the rule applies to a file in folder dir, and
// returns the folders the wildcard segments stood for, as paths, and the
// folder the pattern ended at: the folder the rule names, of which dir is
// or lies below.
func (r *Rule) matchAt(dir, name string) ([]string, string, bool) {
	if r.Glob != "" {
		if ok, _ := path.Match(strings.ToLower(r.Glob), strings.ToLower(name)); !ok {
			return nil, "", false
		}
	}
	var segs []string
	if dir != "" {
		segs = strings.Split(dir, "/")
	}
	idx, end, ok := matchSegs(r.Pattern, segs, 0, nil)
	if !ok {
		return nil, "", false
	}
	caps := make([]string, len(idx))
	for i, n := range idx {
		caps[i] = strings.Join(segs[:n+1], "/")
	}
	return caps, strings.Join(segs[:end], "/"), true
}

// matchSegs matches the pattern against a prefix of segs[at:]; a segment
// with a wildcard ("*", "_*") matches one folder name, case-insensitively,
// and its position is captured; "**" matches any number of folders. It
// returns the captures and where the matched prefix ends.
func matchSegs(pat, segs []string, at int, caps []int) ([]int, int, bool) {
	if len(pat) == 0 {
		return caps, at, true
	}
	if pat[0] == "**" {
		for i := at; i <= len(segs); i++ {
			if c, end, ok := matchSegs(pat[1:], segs, i, caps); ok {
				return c, end, true
			}
		}
		return nil, 0, false
	}
	if at >= len(segs) {
		return nil, 0, false
	}
	if strings.ContainsAny(pat[0], "*?[") {
		if ok, _ := path.Match(strings.ToLower(pat[0]), strings.ToLower(segs[at])); !ok {
			return nil, 0, false
		}
		return matchSegs(pat[1:], segs, at+1, append(caps, at))
	}
	if !strings.EqualFold(pat[0], segs[at]) {
		return nil, 0, false
	}
	return matchSegs(pat[1:], segs, at+1, caps)
}

// expand puts the names of the captured folders in place of $1, $2 ...
func (t RuleTag) expand(caps []string, label func(dir string) string) string {
	s := t.Path
	for i := len(caps); i >= 1; i-- {
		s = strings.ReplaceAll(s, "$"+strconv.Itoa(i), label(caps[i-1]))
	}
	return s
}

// Tags is the tag table with lookup and creation by path.
type Tags struct {
	c     *Catalog
	byKey map[string]int64 // facet + "\x00" + parent id + "\x00" + lower name
}

func newTags(c *Catalog) (*Tags, error) {
	t := &Tags{c: c, byKey: map[string]int64{}}
	rows, err := c.Query("SELECT id, facet, COALESCE(parent, 0), name FROM tags WHERE facet != 'location' AND merged_into IS NULL")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent int64
		var facet, name string
		rows.Scan(&id, &facet, &parent, &name)
		t.byKey[tagKey(facet, parent, name)] = id
	}
	return t, rows.Err()
}

func tagKey(facet string, parent int64, name string) string {
	return facet + "\x00" + strconv.FormatInt(parent, 10) + "\x00" + strings.ToLower(name)
}

// ensure returns the tag at the path ("DEC > VAX") in facet, creating the
// missing levels.
func (t *Tags) ensure(tx *sql.Tx, facet, p string) (int64, error) {
	var parent int64
	for _, name := range strings.Split(p, tagSep) {
		name = strings.TrimSpace(name)
		if name == "" {
			return 0, fmt.Errorf("empty level in %q", p)
		}
		k := tagKey(facet, parent, name)
		if id, ok := t.byKey[k]; ok {
			parent = id
			continue
		}
		slug, err := freeSlug(tx, facet, parent, slugify(name))
		if err != nil {
			return 0, err
		}
		res, err := tx.Exec("INSERT INTO tags (facet, parent, name, slug) VALUES (?, NULLIF(?, 0), ?, ?)", facet, parent, name, slug)
		if err != nil {
			return 0, err
		}
		id, _ := res.LastInsertId()
		t.byKey[k] = id
		parent = id
	}
	return parent, nil
}

// freeSlug returns slug, or slug-2, slug-3 ... if a sibling has it already.
func freeSlug(tx *sql.Tx, facet string, parent int64, slug string) (string, error) {
	for i := 1; ; i++ {
		s := slug
		if i > 1 {
			s = fmt.Sprintf("%s-%d", slug, i)
		}
		var n int
		if err := tx.QueryRow("SELECT COUNT(*) FROM tags WHERE facet = ? AND COALESCE(parent, 0) = ? AND slug = ?", facet, parent, s).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return s, nil
		}
	}
}

// loadTaxonomy creates the tags of taxonomy.conf and sets their flags.
func (t *Tags) loadTaxonomy(tx *sql.Tx, entries []TaxEntry) error {
	for _, e := range entries {
		id, err := t.ensure(tx, e.Facet, strings.Join(e.Path, tagSep))
		if err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE tags SET section = ?, sort = ? WHERE id = ?", e.Section, e.Sort, id); err != nil {
			return err
		}
	}
	return nil
}
