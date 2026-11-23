package archiv

import (
	"database/sql"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// Tags for the views: their names, paths and URLs, finding them by path,
// and where a tag went that was renamed, moved or merged.

var facetLabels = map[string]string{
	"machine": "Rechner", "kind": "Art", "publication": "Zeitschrift", "event": "Treffen", "system": "System",
	"language": "Sprache", "period": "Zeit", "source": "Quelle", "group": "Gruppe", "keyword": "Stichwort", "location": "Ordner",
}

var facetPrefix = map[string]string{"machine": "r", "publication": "z", "event": "treffen"}

// tagRef describes a tag with its path and URL.
func (a *App) tagRef(id int64) *TagRef {
	var facet, name, slug string
	var parent sql.NullInt64
	var p sql.NullString
	if err := a.cat.QueryRow("SELECT facet, name, slug, parent, path FROM tags WHERE id = ?", id).Scan(&facet, &name, &slug, &parent, &p); err != nil {
		return nil
	}
	ref := &TagRef{ID: id, Facet: facet, Name: name}
	if facet == "location" {
		ref.Path = p.String
		ref.Href = href(p.String) + "/"
		return ref
	}
	names, slugs := []string{name}, []string{slug}
	for parent.Valid && len(names) < 10 {
		var n, s string
		var pp sql.NullInt64
		if a.cat.QueryRow("SELECT name, slug, parent FROM tags WHERE id = ?", parent.Int64).Scan(&n, &s, &pp) != nil {
			break
		}
		names = append([]string{n}, names...)
		slugs = append([]string{s}, slugs...)
		parent = pp
	}
	ref.Path = strings.Join(names, " › ")
	if pre, ok := facetPrefix[facet]; ok {
		ref.Href = "/" + pre + "/" + strings.Join(slugs, "/")
	} else {
		ref.Href = "/t/" + facet + "/" + strings.Join(slugs, "/")
	}
	return ref
}

// tagByPath finds a tag by the slugs of its path.
func (a *App) tagByPath(facet string, slugs []string) (int64, bool) {
	var id int64
	for _, s := range slugs {
		if s == "" {
			continue
		}
		var next int64
		err := a.cat.QueryRow("SELECT id FROM tags WHERE facet = ? AND COALESCE(parent, 0) = ? AND slug = ? AND merged_into IS NULL",
			facet, id, s).Scan(&next)
		if err != nil {
			return 0, false
		}
		id = next
	}
	return id, id != 0
}

type ItemRow struct {
	ID         int64
	Title      string
	Descr      string
	Name, Path string
	Size       int64
	Type       string
	Files      int
	Href       string
	Thumb      string
	Folder     string
	FolderHref string
	FileHref   string // a single file: straight to it
	Preview    string
	IsDoc      bool
}

// TagView is what the view of a single tag shows besides its items: its
// description and page, for reading and editing, and what was said about it.
type TagView struct {
	Ref        TagRef
	Descr      string
	Page       string
	Name       string
	Rev        int
	ParentPath string
	Comments   []*CommentView
	History    bool
}

// tagMoved sends the old URL of a renamed, moved or merged tag to where it
// is now; a deleted tag falls back to the nearest level above that exists.
func (a *App) tagMoved(w http.ResponseWriter, r *http.Request, facet string, slugs []string) {
	for n := len(slugs); n > 0; n-- {
		var id int64
		key := facet + "/" + strings.Join(slugs[:n], "/")
		if a.cat.QueryRow("SELECT tag FROM tag_redirects WHERE old_slug_path = ?", key).Scan(&id) == nil {
			for range 10 { // a merged tag may have been merged again
				var into sql.NullInt64
				if a.cat.QueryRow("SELECT merged_into FROM tags WHERE id = ?", id).Scan(&into) != nil || !into.Valid {
					break
				}
				id = into.Int64
			}
			if ref := a.tagRef(id); ref != nil {
				http.Redirect(w, r, ref.Href, http.StatusMovedPermanently)
				return
			}
		}
		if n < len(slugs) {
			if id, ok := a.tagByPath(facet, slugs[:n]); ok {
				http.Redirect(w, r, a.tagRef(id).Href, http.StatusFound)
				return
			}
		}
	}
	a.notFound(w, r, "Dieses Schlagwort gibt es nicht.")
}

// ancestors are the tags above ref, nearest first.
func (a *App) ancestors(ref *TagRef) []TagRef {
	var out []TagRef
	var parent sql.NullInt64
	a.cat.QueryRow("SELECT parent FROM tags WHERE id = ?", ref.ID).Scan(&parent)
	for parent.Valid && len(out) < 10 {
		if t := a.tagRef(parent.Int64); t != nil {
			out = append(out, *t)
		}
		var pp sql.NullInt64
		a.cat.QueryRow("SELECT parent FROM tags WHERE id = ?", parent.Int64).Scan(&pp)
		parent = pp
	}
	return out
}

func inList(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func int64Args(ids []int64) []any {
	out := make([]any, len(ids))
	for i, x := range ids {
		out[i] = x
	}
	return out
}

// itemRows lists the live items matching where, by title.
func (a *App) itemRows(where string, args []any) []ItemRow {
	rows, err := a.cat.Query(`SELECT i.id, i.title, i.descr, i.root,
	        (SELECT COUNT(*) FROM item_files f WHERE f.item = i.id),
	        (SELECT f.path FROM item_files f WHERE f.item = i.id ORDER BY f.ord, f.name LIMIT 1),
	        (SELECT COALESCE(SUM(fl.size), 0) FROM item_files f JOIN files fl ON fl.sha256 = f.sha256 WHERE f.item = i.id)
	    FROM items i WHERE i.state = 'live' AND `+where+`
	    ORDER BY lower(i.title), i.id`, args...)
	if err != nil {
		logf("items with tags: %v", err)
		return nil
	}
	defer rows.Close()
	var out []ItemRow
	for rows.Next() {
		var it ItemRow
		var root, first sql.NullString
		rows.Scan(&it.ID, &it.Title, &it.Descr, &root, &it.Files, &first, &it.Size)
		it.Path = first.String
		if root.Valid {
			it.Path = root.String
		}
		it.Name = path.Base(it.Path)
		if it.Files == 1 {
			it.Type = typeHint(it.Name)
		}
		it.Href = "/a/" + strconv.FormatInt(it.ID, 10)
		if it.Descr == it.Title {
			it.Descr = ""
		}
		it.Folder = parentDir(it.Path)
		it.FolderHref = href(it.Folder) + "/"
		if it.Files == 1 && !root.Valid {
			it.FileHref = href(it.Path)
		}
		out = append(out, it)
	}
	return out
}

type kindTag struct {
	ID   int64
	Name string
}

// kindChoices are the kinds the AI chooses from: those shown as sections
// under a machine, and those of magazines, books and collections.
func (a *App) kindChoices() []string {
	var out []string
	for _, k := range a.sectionKinds() {
		out = append(out, k.Name)
	}
	return append(out, "Zeitschrift", "Datenträger zum Heft", "Werbung", "Buch", "CD-ROM/Sammlung")
}

// sectionKinds are the kinds shown as sections, in their order.
func (a *App) sectionKinds() []kindTag {
	rows, err := a.cat.Query("SELECT id, name FROM tags WHERE facet = 'kind' AND section = 1 AND merged_into IS NULL ORDER BY sort, name")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []kindTag
	for rows.Next() {
		var k kindTag
		rows.Scan(&k.ID, &k.Name)
		out = append(out, k)
	}
	return out
}

// naturalLess orders names the way people read them: PDP-3 before PDP-10,
// case ignored.
func naturalLess(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	for a != "" && b != "" {
		da, db := digitsPrefix(a), digitsPrefix(b)
		if da != "" && db != "" {
			na, nb := strings.TrimLeft(da, "0"), strings.TrimLeft(db, "0")
			if len(na) != len(nb) {
				return len(na) < len(nb)
			}
			if na != nb {
				return na < nb
			}
			a, b = a[len(da):], b[len(db):]
			continue
		}
		if a[0] != b[0] {
			return a[0] < b[0]
		}
		a, b = a[1:], b[1:]
	}
	return len(a) < len(b)
}

func digitsPrefix(s string) string {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return s[:i]
}
