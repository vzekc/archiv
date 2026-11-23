package archiv

import (
	"bytes"
	"flag"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/text/encoding/charmap"
)

// The catalog written back as descriptions files, in the old format: a
// "Title=" line, then "<name> <description>" per entry, CRLF, cp1252 where
// the text allows it. The ZIP downloads carry one per folder, and
// "archiv export-descriptions" writes them into a tree, so the share
// stays readable without the database.

// descriptionsFor is the descriptions file of a folder of the old tree, or
// nil if there is nothing to say.
func (a *App) descriptionsFor(rel string) []byte {
	return a.cat.descriptionsFor(rel)
}

func (c *Catalog) descriptionsFor(rel string) []byte {
	var lines []string
	if rel != "" {
		var title string
		c.QueryRow("SELECT name FROM tags WHERE facet = 'location' AND path = ?", rel).Scan(&title)
		if title != "" && title != path.Base(rel) {
			lines = append(lines, "Title= "+title)
		}
	}
	entries := map[string]string{}
	rows, err := c.Query(`SELECT path, descr FROM tags WHERE facet = 'location' AND descr != ''
	    AND parent = (SELECT id FROM tags WHERE facet = 'location' AND path = ?)`, rel)
	if err == nil {
		for rows.Next() {
			var p, d string
			rows.Scan(&p, &d)
			entries[path.Base(p)] = d
		}
		rows.Close()
	}
	lo, hi := pathRange(rel)
	rows, err = c.Query(`SELECT f.path, i.title, i.descr FROM item_files f JOIN items i ON i.id = f.item
	    WHERE f.path >= ? AND f.path < ? AND instr(substr(f.path, ?), '/') = 0 AND i.root IS NULL
	      AND i.state IN ('live', 'missing') AND (SELECT COUNT(*) FROM item_files g WHERE g.item = i.id) = 1`, lo, hi, len(lo)+1)
	if err == nil {
		for rows.Next() {
			var p, title, d string
			rows.Scan(&p, &title, &d)
			name := path.Base(p)
			if d == "" && title != name {
				d = title
			}
			if d != "" {
				entries[name] = strings.ReplaceAll(d, "\n", " ")
			}
		}
		rows.Close()
	}
	if len(entries) == 0 && len(lines) == 0 {
		return nil
	}
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sortFold(names)
	for _, n := range names {
		lines = append(lines, n+" "+entries[n])
	}
	text := strings.Join(lines, "\r\n") + "\r\n"
	if enc, err := charmap.Windows1252.NewEncoder().String(text); err == nil {
		return []byte(enc)
	}
	return []byte(text)
}

func sortFold(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.ToLower(s[j]) < strings.ToLower(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func exportDescriptionsCmd(args []string) {
	fs := flag.NewFlagSet("export-descriptions", flag.ExitOnError)
	catalog := fs.String("catalog", "", "catalog database")
	only := fs.String("only", "", "only folders below this path")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		log.Fatal(err)
	}
	if fs.NArg() != 1 {
		log.Fatal("usage: archiv export-descriptions [-only folder] <target dir>")
	}
	if *catalog != "" {
		cfg.CatalogDB = *catalog
	}
	c, err := openCatalog(cfg.CatalogDB)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	out := fs.Arg(0)
	rows, err := c.Query("SELECT path FROM tags WHERE facet = 'location' AND path IS NOT NULL ORDER BY path")
	if err != nil {
		log.Fatal(err)
	}
	var dirs []string
	for rows.Next() {
		var p string
		rows.Scan(&p)
		dirs = append(dirs, p)
	}
	rows.Close()
	n := 0
	for _, d := range append([]string{""}, dirs...) {
		if *only != "" && d != *only && !strings.HasPrefix(d, *only+"/") {
			continue
		}
		data := c.descriptionsFor(d)
		if data == nil {
			continue
		}
		dir := filepath.Join(out, filepath.FromSlash(d))
		if err := os.MkdirAll(dir, 0o775); err != nil {
			log.Fatal(err)
		}
		if old, err := os.ReadFile(filepath.Join(dir, "descriptions")); err == nil && bytes.Equal(old, data) {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "descriptions"), data, 0o664); err != nil {
			log.Fatal(err)
		}
		n++
	}
	fmt.Printf("wrote %d descriptions files below %s\n", n, out)
}
