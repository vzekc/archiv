package archiv

import (
	"net/http"
	"strconv"
	"strings"
)

// The import report: what the catalog holds after the last sync, and what
// still needs a hand.

type ReportFacet struct {
	Facet       string
	Tags, Items int
}

type ReportItem struct {
	ID    int64
	Title string
	Path  string
}

type ReportDup struct {
	Sha   string
	Size  int64
	Paths []string
}

type ReportRule struct {
	Line, Hits int
	Text       string
}

type ImportReport struct {
	LastSync                      int64
	Items, Missing, Merged, Files int
	Paths, Vanished, Bytes        int64
	Facets                        []ReportFacet
	Untagged                      int
	UntaggedSample                []ReportItem
	Dups                          int
	DupSample                     []ReportDup
	Rules                         []ReportRule
	Weak                          int
}

func (a *App) importReport(w http.ResponseWriter, r *http.Request) {
	c := a.cat
	rep := &ImportReport{}
	rep.LastSync, _ = strconv.ParseInt(c.meta("last_sync"), 10, 64)
	c.QueryRow(`SELECT COUNT(*) FILTER (WHERE state = 'live'), COUNT(*) FILTER (WHERE state = 'missing'),
	    COUNT(*) FILTER (WHERE state = 'merged') FROM items`).Scan(&rep.Items, &rep.Missing, &rep.Merged)
	c.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files").Scan(&rep.Files, &rep.Bytes)
	c.QueryRow("SELECT COUNT(*), COUNT(*) FILTER (WHERE seen < ?) FROM file_paths", rep.LastSync).Scan(&rep.Paths, &rep.Vanished)

	rows, _ := c.Query(`SELECT t.facet, COUNT(DISTINCT t.id), COUNT(DISTINCT it.item)
	    FROM tags t LEFT JOIN item_tags it ON it.tag = t.id GROUP BY t.facet ORDER BY t.facet`)
	for rows != nil && rows.Next() {
		var f ReportFacet
		rows.Scan(&f.Facet, &f.Tags, &f.Items)
		rep.Facets = append(rep.Facets, f)
	}
	if rows != nil {
		rows.Close()
	}

	// lacking a kind, or a machine where one belongs
	untagged := "FROM items i WHERE " + needsTagsWhere
	c.QueryRow("SELECT COUNT(*) " + untagged).Scan(&rep.Untagged)
	rows, _ = c.Query(`SELECT i.id, i.title, (SELECT MIN(path) FROM item_files WHERE item = i.id) ` + untagged + ` ORDER BY 3 LIMIT 100`)
	for rows != nil && rows.Next() {
		var it ReportItem
		rows.Scan(&it.ID, &it.Title, &it.Path)
		rep.UntaggedSample = append(rep.UntaggedSample, it)
	}
	if rows != nil {
		rows.Close()
	}

	c.QueryRow(`SELECT COUNT(*) FROM (SELECT sha256 FROM file_paths WHERE seen >= ? GROUP BY sha256 HAVING COUNT(*) > 1)`, rep.LastSync).Scan(&rep.Dups)
	rows, _ = c.Query(`SELECT p.sha256, f.size, group_concat(p.path, char(10)) FROM file_paths p JOIN files f ON f.sha256 = p.sha256
	    WHERE p.seen >= ? GROUP BY p.sha256 HAVING COUNT(*) > 1 ORDER BY f.size DESC LIMIT 100`, rep.LastSync)
	for rows != nil && rows.Next() {
		var d ReportDup
		var paths string
		rows.Scan(&d.Sha, &d.Size, &paths)
		d.Paths = strings.Split(paths, "\n")
		rep.DupSample = append(rep.DupSample, d)
	}
	if rows != nil {
		rows.Close()
	}

	for _, l := range strings.Split(c.meta("rule_hits"), "\n") {
		parts := strings.SplitN(l, "\t", 3)
		if len(parts) == 3 {
			line, _ := strconv.Atoi(parts[0])
			hits, _ := strconv.Atoi(parts[1])
			rep.Rules = append(rep.Rules, ReportRule{Line: line, Hits: hits, Text: parts[2]})
		}
	}
	c.QueryRow("SELECT COUNT(DISTINCT item) FROM item_tags WHERE added_by = 'import:weak'").Scan(&rep.Weak)
	a.render(w, r, "import-report", "Katalog", rep)
}
