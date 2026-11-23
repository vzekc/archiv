package archiv

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// Changes to the catalog. Every change is a revision: who, when, what before
// and after, why. Members with the right to edit apply changes directly;
// everyone else proposes them, and a moderator accepts or rejects them. A
// change carries the revision of its target it was based on, so that two
// people editing the same item cannot overwrite each other unnoticed.

// Change is one edit of one target.
type Change struct {
	Target string         // "item:12", "tag:5", "page:5"; "tag:0" for a new tag
	Op     string         // see applyChange
	Data   map[string]any // the operation's arguments, with the old values
	Rev    int            // revision of the target it was based on; -1 = do not check
	Reason string
}

var (
	errConflict = errors.New("conflict")
	errInvalid  = errors.New("invalid")
)

// ConflictError names who changed the target in the meantime.
type ConflictError struct {
	User string
	At   int64
}

func (e *ConflictError) Error() string { return "changed in the meantime" }
func (e *ConflictError) Unwrap() error { return errConflict }

func targetOf(kind string, id int64) string { return kind + ":" + strconv.FormatInt(id, 10) }

func splitTarget(t string) (string, int64) {
	k, v, _ := strings.Cut(t, ":")
	id, _ := strconv.ParseInt(v, 10, 64)
	return k, id
}

func str(d map[string]any, k string) string {
	switch v := d[k].(type) {
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	case int64:
		return strconv.FormatInt(v, 10)
	case int:
		return strconv.Itoa(v)
	case nil:
		return ""
	}
	return fmt.Sprint(d[k])
}

// likePrefix is a LIKE pattern for the strings starting with p.
func likePrefix(p string) string {
	return strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(p) + "%"
}

// nums is a list of numbers in a change's data.
func nums(d map[string]any, k string) []int64 {
	var out []int64
	switch v := d[k].(type) {
	case []any:
		for _, x := range v {
			switch n := x.(type) {
			case float64:
				out = append(out, int64(n))
			case int64:
				out = append(out, n)
			}
		}
	case []int64:
		out = v
	}
	return out
}

func num64(d map[string]any, k string) int64 {
	n, _ := strconv.ParseInt(str(d, k), 10, 64)
	return n
}

// revOf is the current revision of a target.
func revOf(q interface {
	QueryRow(string, ...any) *sql.Row
}, target string) (int, error) {
	kind, id := splitTarget(target)
	table := map[string]string{"item": "items", "tag": "tags", "page": "tag_pages", "coll": "collections"}[kind]
	if table == "" {
		return 0, errInvalid
	}
	col := "id"
	if kind == "page" {
		col = "tag"
	}
	var rev int
	err := q.QueryRow("SELECT rev FROM "+table+" WHERE "+col+" = ?", id).Scan(&rev)
	if err == sql.ErrNoRows && kind == "page" {
		return 0, nil
	}
	return rev, err
}

// Apply makes the changes at once, as one batch; a stale revision refuses
// the whole batch.
func (c *Catalog) Apply(user string, changes []Change) (batch string, err error) {
	tx, err := c.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	batch = newBatchID()
	for _, ch := range changes {
		if err := c.applyIn(tx, user, batch, ch); err != nil {
			return "", err
		}
	}
	if touchesTree(changes) {
		if err := rebuildClosure(tx); err != nil {
			return "", err
		}
	}
	return batch, tx.Commit()
}

func (c *Catalog) applyIn(tx *sql.Tx, user, batch string, ch Change) error {
	if ch.Op == "create-tag" || ch.Op == "create-coll" {
		ch.Rev = -1
	}
	if ch.Rev >= 0 {
		rev, err := revOf(tx, ch.Target)
		if err != nil {
			return err
		}
		if rev != ch.Rev {
			ce := &ConflictError{}
			tx.QueryRow("SELECT user, at FROM revisions WHERE target = ? AND state = 'applied' ORDER BY id DESC LIMIT 1", ch.Target).Scan(&ce.User, &ce.At)
			return ce
		}
	}
	if err := applyChange(tx, ch); err != nil {
		return err
	}
	switch ch.Op {
	case "create-tag":
		ch.Target = targetOf("tag", num64(ch.Data, "id"))
	case "create-coll":
		ch.Target = targetOf("coll", num64(ch.Data, "id"))
	}
	if err := bumpRev(tx, ch.Target); err != nil {
		return err
	}
	data, _ := json.Marshal(ch.Data)
	_, err := tx.Exec(`INSERT INTO revisions (target, op, data, user, at, reason, state, batch, decided_by, decided_at)
	    VALUES (?, ?, ?, ?, ?, ?, 'applied', ?, ?, ?)`, ch.Target, ch.Op, string(data), user, now(), ch.Reason, batch, user, now())
	return err
}

// Propose stores the changes for a moderator to decide.
func (c *Catalog) Propose(user string, changes []Change) (string, error) {
	tx, err := c.Begin()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	batch := newBatchID()
	for _, ch := range changes {
		if err := validateChange(tx, ch); err != nil {
			return "", err
		}
		d := ch.Data
		if d == nil {
			d = map[string]any{}
		}
		d["base_rev"] = ch.Rev
		data, _ := json.Marshal(d)
		if _, err := tx.Exec(`INSERT INTO revisions (target, op, data, user, at, reason, state, batch) VALUES (?, ?, ?, ?, ?, ?, 'proposed', ?)`,
			ch.Target, ch.Op, string(data), user, now(), ch.Reason, batch); err != nil {
			return "", err
		}
	}
	return batch, tx.Commit()
}

// Decide accepts or rejects proposed revisions. An accepted proposal is
// applied as it was proposed; one whose old values no longer hold fails
// and stays open.
func (c *Catalog) Decide(user string, ids []int64, accept bool) error {
	tx, err := c.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		var ch Change
		var data, state string
		if err := tx.QueryRow("SELECT target, op, data, reason, state FROM revisions WHERE id = ?", id).Scan(&ch.Target, &ch.Op, &data, &ch.Reason, &state); err != nil {
			return err
		}
		if state != "proposed" {
			continue
		}
		json.Unmarshal([]byte(data), &ch.Data)
		if accept {
			delete(ch.Data, "base_rev")
			ch.Rev = -1 // the old values in the data are checked instead
			if err := applyChange(tx, ch); err != nil {
				return fmt.Errorf("Vorschlag %d: %w", id, err)
			}
			switch ch.Op {
			case "create-tag":
				ch.Target = targetOf("tag", num64(ch.Data, "id"))
				fallthrough
			case "add-tag-path":
				// the new tag's id, so that the revision can be undone
				data, _ := json.Marshal(ch.Data)
				tx.Exec("UPDATE revisions SET target = ?, data = ? WHERE id = ?", ch.Target, string(data), id)
			}
			if err := bumpRev(tx, ch.Target); err != nil {
				return err
			}
		}
		st := "rejected"
		if accept {
			st = "applied"
		}
		if _, err := tx.Exec("UPDATE revisions SET state = ?, decided_by = ?, decided_at = ? WHERE id = ?", st, user, now(), id); err != nil {
			return err
		}
	}
	if err := rebuildClosure(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Revert undoes applied revisions with their opposite, newest first.
func (c *Catalog) Revert(user string, ids []int64, reason string) error {
	tx, err := c.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	batch := newBatchID()
	for i := len(ids) - 1; i >= 0; i-- {
		var ch Change
		var data, state string
		if err := tx.QueryRow("SELECT target, op, data, state FROM revisions WHERE id = ?", ids[i]).Scan(&ch.Target, &ch.Op, &data, &state); err != nil {
			return err
		}
		if state != "applied" {
			continue
		}
		json.Unmarshal([]byte(data), &ch.Data)
		inv, err := inverse(ch)
		if err != nil {
			return err
		}
		inv.Rev = -1
		inv.Reason = reason
		if err := c.applyIn(tx, user, batch, inv); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE revisions SET state = 'reverted' WHERE id = ?", ids[i]); err != nil {
			return err
		}
	}
	if err := rebuildClosure(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func bumpRev(tx *sql.Tx, target string) error {
	kind, id := splitTarget(target)
	var err error
	switch kind {
	case "item":
		_, err = tx.Exec("UPDATE items SET rev = rev + 1, updated = ? WHERE id = ?", now(), id)
	case "tag":
		_, err = tx.Exec("UPDATE tags SET rev = rev + 1 WHERE id = ?", id)
	case "page":
		_, err = tx.Exec("UPDATE tag_pages SET rev = rev + 1, updated = ? WHERE tag = ?", now(), id)
	case "coll":
		_, err = tx.Exec("UPDATE collections SET rev = rev + 1 WHERE id = ?", id)
	}
	return err
}

var itemFields = map[string]bool{"title": true, "descr": true, "year": true}
var tagFields = map[string]bool{"name": true, "descr": true}
var relKinds = map[string]string{
	"part-of": "ist Teil von", "same-as": "ist dasselbe wie", "newer-version-of": "ist eine neuere Fassung von",
	"translation-of": "ist eine Übersetzung von", "documents": "beschreibt",
}

// relInverse names a relation from the other side.
var relInverse = map[string]string{
	"part-of": "hat als Teil", "same-as": "ist dasselbe wie", "newer-version-of": "hat als neuere Fassung",
	"translation-of": "hat als Übersetzung", "documents": "wird beschrieben von",
}
var fileRoles = map[string]bool{"main": true, "part": true, "cover": true, "extra": true}

// validateChange checks a change without applying it.
func validateChange(tx *sql.Tx, ch Change) error {
	kind, id := splitTarget(ch.Target)
	d := ch.Data
	switch ch.Op {
	case "set-field":
		f := str(d, "field")
		if kind == "item" && !itemFields[f] || kind == "tag" && !tagFields[f] {
			return fmt.Errorf("%w: field %q", errInvalid, f)
		}
		if f == "title" || f == "name" {
			if strings.TrimSpace(str(d, "new")) == "" {
				return fmt.Errorf("%w: ein leerer Titel geht nicht", errInvalid)
			}
		}
	case "add-tag", "remove-tag":
		var n int
		tx.QueryRow("SELECT COUNT(*) FROM tags WHERE id = ? AND facet != 'location'", num64(d, "tag")).Scan(&n)
		if n == 0 {
			return fmt.Errorf("%w: unknown tag", errInvalid)
		}
	case "add-rel", "remove-rel":
		if relKinds[str(d, "kind")] == "" {
			return fmt.Errorf("%w: relation %q", errInvalid, str(d, "kind"))
		}
		var n int
		tx.QueryRow("SELECT COUNT(*) FROM items WHERE id = ?", num64(d, "item")).Scan(&n)
		if n == 0 || num64(d, "item") == id {
			return fmt.Errorf("%w: Objekt A%d gibt es nicht", errInvalid, num64(d, "item"))
		}
	case "rename-file":
		n := strings.TrimSpace(str(d, "new"))
		if n == "" || strings.HasPrefix(n, "/") || strings.Contains(n, "..") {
			return fmt.Errorf("%w: ungültiger Dateiname „%s“", errInvalid, n)
		}
		var c int
		tx.QueryRow("SELECT COUNT(*) FROM item_files WHERE item = ? AND name = ?", id, n).Scan(&c)
		if c > 0 {
			return fmt.Errorf("%w: das Objekt hat schon eine Datei „%s“", errInvalid, n)
		}
	case "split-file", "remove-file":
		var has, all int
		tx.QueryRow("SELECT COUNT(*) FILTER (WHERE name = ?), COUNT(*) FROM item_files WHERE item = ?", str(d, "name"), id).Scan(&has, &all)
		if has == 0 {
			return fmt.Errorf("%w: das Objekt hat keine Datei „%s“", errInvalid, str(d, "name"))
		}
		if all < 2 {
			return fmt.Errorf("%w: das ist die einzige Datei des Objekts; ganz zurückziehen geht mit „zurückziehen“", errInvalid)
		}
		if into := num64(d, "into"); into != 0 {
			var st string
			if tx.QueryRow("SELECT state FROM items WHERE id = ?", into).Scan(&st) != nil || st != "live" || into == id {
				return fmt.Errorf("%w: Objekt A%d gibt es nicht", errInvalid, into)
			}
		}
		if ch.Op == "remove-file" && strings.TrimSpace(str(d, "reason")) == "" {
			return fmt.Errorf("%w: zum Entfernen einer Datei gehört ein Grund", errInvalid)
		}
	case "set-role":
		if !fileRoles[str(d, "new")] {
			return fmt.Errorf("%w: role", errInvalid)
		}
	case "merge":
		into := num64(d, "into")
		var st string
		if tx.QueryRow("SELECT state FROM items WHERE id = ?", into).Scan(&st) != nil || st != "live" || into == id {
			return fmt.Errorf("%w: Objekt A%d gibt es nicht", errInvalid, into)
		}
	case "gather":
		items := nums(d, "items")
		if len(items) == 0 || slices.Contains(items, id) {
			return fmt.Errorf("%w: keine anderen Objekte", errInvalid)
		}
		for _, x := range append([]int64{id}, items...) {
			var st string
			if tx.QueryRow("SELECT state FROM items WHERE id = ?", x).Scan(&st) != nil || st != "live" {
				return fmt.Errorf("%w: Objekt A%d gibt es nicht", errInvalid, x)
			}
		}
	case "withdraw", "restore":
		if ch.Op == "withdraw" && strings.TrimSpace(str(d, "reason")) == "" {
			return fmt.Errorf("%w: ohne Grund geht das nicht", errInvalid)
		}
	case "create-tag":
		if !isFacet(str(d, "facet")) || str(d, "facet") == "location" || strings.TrimSpace(str(d, "name")) == "" {
			return fmt.Errorf("%w: tag", errInvalid)
		}
	case "add-tag-path":
		if !isFacet(str(d, "facet")) || str(d, "facet") == "location" || strings.TrimSpace(str(d, "path")) == "" {
			return fmt.Errorf("%w: tag", errInvalid)
		}
	case "create-coll":
		if strings.TrimSpace(str(d, "title")) == "" {
			return fmt.Errorf("%w: die Sammlung braucht einen Namen", errInvalid)
		}
	case "coll-add", "coll-remove":
		var n int
		tx.QueryRow("SELECT COUNT(*) FROM items WHERE id = ?", num64(d, "item")).Scan(&n)
		if n == 0 {
			return fmt.Errorf("%w: Objekt A%d gibt es nicht", errInvalid, num64(d, "item"))
		}
	case "set-coll":
		if f := str(d, "field"); f != "title" && f != "intro" {
			return fmt.Errorf("%w: field %q", errInvalid, f)
		}
	case "move-tag", "merge-tag", "set-page":
	default:
		return fmt.Errorf("%w: op %q", errInvalid, ch.Op)
	}
	return nil
}

// slugPath is "facet/slug/slug" of a tag, the key of tag_redirects.
func slugPath(tx *sql.Tx, id int64) string {
	var facet string
	var slugs []string
	for p := id; p != 0 && len(slugs) < 10; {
		var slug string
		var parent sql.NullInt64
		if tx.QueryRow("SELECT facet, slug, parent FROM tags WHERE id = ?", p).Scan(&facet, &slug, &parent) != nil {
			break
		}
		slugs = append([]string{slug}, slugs...)
		p = parent.Int64
	}
	return facet + "/" + strings.Join(slugs, "/")
}

// keepPath remembers where a tag and those below it were, before a rename,
// move or merge; to is where the old paths lead.
func keepPath(tx *sql.Tx, id, to int64) {
	rows, err := tx.Query(`WITH RECURSIVE sub(id) AS (SELECT ? UNION SELECT t.id FROM tags t JOIN sub ON t.parent = sub.id) SELECT id FROM sub`, id)
	if err != nil {
		return
	}
	var ids []int64
	for rows.Next() {
		var x int64
		rows.Scan(&x)
		ids = append(ids, x)
	}
	rows.Close()
	for _, x := range ids {
		dest := x
		if x == id {
			dest = to
		}
		tx.Exec("INSERT OR REPLACE INTO tag_redirects (old_slug_path, tag) VALUES (?, ?)", slugPath(tx, x), dest)
	}
}

// applyChange carries a change out. Old values in the data must still hold.
func applyChange(tx *sql.Tx, ch Change) error {
	if err := validateChange(tx, ch); err != nil {
		return err
	}
	kind, id := splitTarget(ch.Target)
	switch {
	case kind == "tag" && ch.Op == "set-field" && str(ch.Data, "field") == "name", ch.Op == "move-tag":
		keepPath(tx, id, id)
	case ch.Op == "merge-tag":
		keepPath(tx, id, num64(ch.Data, "into"))
	}
	d := ch.Data
	stale := func(what string) error { return fmt.Errorf("%w: %s", errConflict, what) }
	switch ch.Op {
	case "set-field":
		f := str(d, "field")
		table := map[string]string{"item": "items", "tag": "tags"}[kind]
		var cur string
		if err := tx.QueryRow("SELECT "+f+" FROM "+table+" WHERE id = ?", id).Scan(&cur); err != nil {
			return err
		}
		if old, ok := d["old"]; ok && fmt.Sprint(old) != cur {
			return stale(f)
		}
		if f == "name" {
			// a folder keeps its slug, the name of the folder on disk
			var facet, oldSlug string
			tx.QueryRow("SELECT facet, slug FROM tags WHERE id = ?", id).Scan(&facet, &oldSlug)
			if facet == "location" {
				_, err := tx.Exec("UPDATE tags SET name = ? WHERE id = ?", str(d, "new"), id)
				return err
			}
			slug, err := freeSlugExcept(tx, id, slugify(str(d, "new")))
			if err != nil {
				return err
			}
			if slug != oldSlug {
				// the old URL leads to the renamed tag
				if _, err := tx.Exec("INSERT OR REPLACE INTO tag_redirects (old_slug_path, tag) VALUES (?, ?)", slugPath(tx, id), id); err != nil {
					return err
				}
			}
			_, err = tx.Exec("UPDATE tags SET name = ?, slug = ? WHERE id = ?", str(d, "new"), slug, id)
			return err
		}
		_, err := tx.Exec("UPDATE "+table+" SET "+f+" = ? WHERE id = ?", str(d, "new"), id)
		return err
	case "add-tag":
		_, err := tx.Exec("INSERT OR IGNORE INTO item_tags (item, tag, added_by, added_at) VALUES (?, ?, ?, ?)", id, num64(d, "tag"), str(d, "by"), now())
		return err
	case "add-tag-path":
		// a tag given by its path, created where it does not exist yet
		var parent int64
		for _, n := range splitTagPath(str(d, "path")) {
			var tid int64
			err := tx.QueryRow("SELECT id FROM tags WHERE facet = ? AND COALESCE(parent, 0) = ? AND merged_into IS NULL AND lower(name) = lower(?)",
				str(d, "facet"), parent, n).Scan(&tid)
			if err == sql.ErrNoRows {
				slug, err := freeSlug(tx, str(d, "facet"), parent, slugify(n))
				if err != nil {
					return err
				}
				res, err := tx.Exec("INSERT INTO tags (facet, parent, name, slug) VALUES (?, NULLIF(?, 0), ?, ?)", str(d, "facet"), parent, n, slug)
				if err != nil {
					return err
				}
				tid, _ = res.LastInsertId()
			} else if err != nil {
				return err
			}
			parent = tid
		}
		d["tag"] = parent
		_, err := tx.Exec("INSERT OR IGNORE INTO item_tags (item, tag, added_by, added_at) VALUES (?, ?, ?, ?)", id, parent, str(d, "by"), now())
		return err
	case "remove-tag":
		_, err := tx.Exec("DELETE FROM item_tags WHERE item = ? AND tag = ?", id, num64(d, "tag"))
		return err
	case "add-rel":
		_, err := tx.Exec("INSERT OR IGNORE INTO item_rel (a, kind, b) VALUES (?, ?, ?)", id, str(d, "kind"), num64(d, "item"))
		return err
	case "remove-rel":
		_, err := tx.Exec("DELETE FROM item_rel WHERE a = ? AND kind = ? AND b = ?", id, str(d, "kind"), num64(d, "item"))
		return err
	case "set-role":
		var cur string
		if err := tx.QueryRow("SELECT role FROM item_files WHERE item = ? AND name = ?", id, str(d, "name")).Scan(&cur); err != nil {
			return err
		}
		if old, ok := d["old"]; ok && fmt.Sprint(old) != cur {
			return stale("role")
		}
		_, err := tx.Exec("UPDATE item_files SET role = ? WHERE item = ? AND name = ?", str(d, "new"), id, str(d, "name"))
		return err
	case "merge":
		return mergeItems(tx, id, num64(d, "into"))
	case "rename-file":
		old, nw := str(d, "old"), strings.TrimSpace(str(d, "new"))
		res, err := tx.Exec("UPDATE item_files SET name = ? WHERE item = ? AND name = ?", nw, id, old)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return stale("file " + old)
		}
		// the old address of the file leads to it
		_, err = tx.Exec("INSERT OR REPLACE INTO redirects (old_path, item, name, at) VALUES (?, ?, ?, ?)",
			fmt.Sprintf("a/%d/%s", id, old), id, nw, now())
		return err
	case "split-file", "remove-file":
		return splitFile(tx, id, d, ch.Op == "remove-file")
	case "gather":
		// the items' files follow the target's, in the order given
		// pictures that become covers bring no kind: the item keeps its
		// own kinds, or has none
		kinds := []int64{-1}
		if d["covers"] == true {
			rows, err := tx.Query("SELECT it.tag FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = ? AND t.facet = 'kind'", id)
			if err != nil {
				return err
			}
			for rows.Next() {
				var k int64
				rows.Scan(&k)
				kinds = append(kinds, k)
			}
			rows.Close()
		}
		for _, x := range nums(d, "items") {
			if err := mergeItems(tx, x, id); err != nil {
				return err
			}
		}
		// files from below base keep their path from there as name
		if base := str(d, "base"); base != "" {
			if _, err := tx.Exec("UPDATE item_files SET name = substr(path, ?) WHERE item = ? AND path LIKE ? ESCAPE '\\'",
				len(base)+2, id, likePrefix(base+"/")); err != nil {
				return err
			}
		}
		if d["covers"] == true {
			if _, err := tx.Exec(`DELETE FROM item_tags WHERE item = ? AND tag IN (SELECT id FROM tags WHERE facet = 'kind')
			    AND tag NOT IN (`+inList(len(kinds))+`)`, append([]any{id}, int64Args(kinds)...)...); err != nil {
				return err
			}
		}
		// the pictures of a tape or disk are its inlay: the cover
		if d["covers"] == true {
			rows, err := tx.Query("SELECT name FROM item_files WHERE item = ?", id)
			if err != nil {
				return err
			}
			var pics []string
			for rows.Next() {
				var n string
				rows.Scan(&n)
				if imageExt.MatchString(n) {
					pics = append(pics, n)
				}
			}
			rows.Close()
			for _, n := range pics {
				if strings.Contains(n, "/") {
					continue // a picture inside an unpacked disk is its content
				}
				if _, err := tx.Exec("UPDATE item_files SET role = 'cover' WHERE item = ? AND name = ?", id, n); err != nil {
					return err
				}
			}
		}
		if t := strings.TrimSpace(str(d, "title")); t != "" {
			_, err := tx.Exec("UPDATE items SET title = ? WHERE id = ?", t, id)
			return err
		}
		return nil
	case "withdraw":
		_, err := tx.Exec("UPDATE items SET state = 'withdrawn', withdrawn_reason = ? WHERE id = ? AND state = 'live'", str(d, "reason"), id)
		return err
	case "restore":
		_, err := tx.Exec("UPDATE items SET state = 'live', withdrawn_reason = '' WHERE id = ? AND state = 'withdrawn'", id)
		return err
	case "create-tag":
		parent := num64(d, "parent")
		slug, err := freeSlug(tx, str(d, "facet"), parent, slugify(str(d, "name")))
		if err != nil {
			return err
		}
		res, err := tx.Exec("INSERT INTO tags (facet, parent, scope, name, slug) VALUES (?, NULLIF(?, 0), NULLIF(?, 0), ?, ?)",
			str(d, "facet"), parent, num64(d, "scope"), str(d, "name"), slug)
		if err != nil {
			return err
		}
		newID, _ := res.LastInsertId()
		d["id"] = newID
		return nil
	case "move-tag":
		var cur sql.NullInt64
		tx.QueryRow("SELECT parent FROM tags WHERE id = ?", id).Scan(&cur)
		if old, ok := d["old"]; ok && num64(d, "old") != cur.Int64 && old != nil {
			return stale("parent")
		}
		np := num64(d, "new")
		for p := np; p != 0; {
			if p == id {
				return fmt.Errorf("%w: ein Schlagwort kann nicht unter sich selbst liegen", errInvalid)
			}
			var pp sql.NullInt64
			tx.QueryRow("SELECT parent FROM tags WHERE id = ?", p).Scan(&pp)
			p = pp.Int64
		}
		_, err := tx.Exec("UPDATE tags SET parent = NULLIF(?, 0) WHERE id = ?", np, id)
		return err
	case "merge-tag":
		into := num64(d, "into")
		if _, err := tx.Exec("INSERT OR IGNORE INTO item_tags (item, tag, added_by, added_at) SELECT item, ?, added_by, added_at FROM item_tags WHERE tag = ?", into, id); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM item_tags WHERE tag = ?", id); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE tags SET parent = ? WHERE parent = ?", into, id); err != nil {
			return err
		}
		_, err := tx.Exec("UPDATE tags SET merged_into = ? WHERE id = ?", into, id)
		return err
	case "create-coll":
		res, err := tx.Exec("INSERT INTO collections (title, intro, owner, created) VALUES (?, ?, ?, ?)", str(d, "title"), str(d, "intro"), str(d, "owner"), now())
		if err != nil {
			return err
		}
		newID, _ := res.LastInsertId()
		d["id"] = newID
		return nil
	case "coll-add":
		var ord int
		tx.QueryRow("SELECT COALESCE(MAX(ord), 0) + 1 FROM coll_items WHERE coll = ?", id).Scan(&ord)
		if o := num64(d, "ord"); o > 0 {
			ord = int(o)
		}
		d["ord"] = ord
		_, err := tx.Exec("INSERT OR IGNORE INTO coll_items (coll, item, ord, note) VALUES (?, ?, ?, ?)", id, num64(d, "item"), ord, str(d, "note"))
		return err
	case "coll-remove":
		var ord int
		var note string
		tx.QueryRow("SELECT ord, note FROM coll_items WHERE coll = ? AND item = ?", id, num64(d, "item")).Scan(&ord, &note)
		d["ord"], d["note"] = ord, note
		_, err := tx.Exec("DELETE FROM coll_items WHERE coll = ? AND item = ?", id, num64(d, "item"))
		return err
	case "set-coll":
		f := str(d, "field")
		var cur string
		tx.QueryRow("SELECT "+f+" FROM collections WHERE id = ?", id).Scan(&cur)
		if old, ok := d["old"]; ok && fmt.Sprint(old) != cur {
			return stale(f)
		}
		_, err := tx.Exec("UPDATE collections SET "+f+" = ? WHERE id = ?", str(d, "new"), id)
		return err
	case "set-page":
		var cur string
		tx.QueryRow("SELECT body FROM tag_pages WHERE tag = ?", id).Scan(&cur)
		if old, ok := d["old"]; ok && fmt.Sprint(old) != cur {
			return stale("page")
		}
		_, err := tx.Exec("INSERT INTO tag_pages (tag, body, updated) VALUES (?, ?, ?) ON CONFLICT (tag) DO UPDATE SET body = excluded.body, updated = excluded.updated",
			id, str(d, "new"), now())
		return err
	}
	return fmt.Errorf("%w: op %q", errInvalid, ch.Op)
}

// mergeItems moves the files, tags and relations of item from into item
// into; from stays as a redirect.
func mergeItems(tx *sql.Tx, from, into int64) error {
	var ord int
	tx.QueryRow("SELECT COALESCE(MAX(ord), 0) FROM item_files WHERE item = ?", into).Scan(&ord)
	rows, err := tx.Query("SELECT name FROM item_files WHERE item = ? ORDER BY ord, name", from)
	if err != nil {
		return err
	}
	var names []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		names = append(names, n)
	}
	rows.Close()
	for _, n := range names {
		nn := n
		for i := 2; ; i++ {
			var c int
			tx.QueryRow("SELECT COUNT(*) FROM item_files WHERE item = ? AND name = ?", into, nn).Scan(&c)
			if c == 0 {
				break
			}
			nn = fmt.Sprintf("%d-%s", i, n)
		}
		ord++
		if _, err := tx.Exec("UPDATE item_files SET item = ?, name = ?, ord = ?, role = CASE WHEN role = 'main' THEN 'part' ELSE role END WHERE item = ? AND name = ?",
			into, nn, ord, from, n); err != nil {
			return err
		}
	}
	for _, q := range []string{
		"INSERT OR IGNORE INTO item_tags (item, tag, added_by, added_at) SELECT ?, tag, added_by, added_at FROM item_tags WHERE item = ?",
		"INSERT OR IGNORE INTO item_rel (a, kind, b) SELECT ?, kind, b FROM item_rel WHERE a = ?",
	} {
		if _, err := tx.Exec(q, into, from); err != nil {
			return err
		}
	}
	tx.Exec("DELETE FROM item_tags WHERE item = ?", from)
	tx.Exec("DELETE FROM item_rel WHERE a = ? OR b = ?", from, from)
	if _, err := tx.Exec("UPDATE items SET rev = rev + 1, updated = ? WHERE id = ?", now(), into); err != nil {
		return err
	}
	_, err = tx.Exec("UPDATE items SET state = 'merged', merged_into = ? WHERE id = ?", into, from)
	return err
}

// splitFile takes a file out of an item: into another item (into), or
// into a new item with the item's tags, which a removal withdraws. The
// file's old address leads to where it went.
func splitFile(tx *sql.Tx, id int64, d map[string]any, remove bool) error {
	name := str(d, "name")
	to := num64(d, "into")
	if to == 0 {
		var title string
		tx.QueryRow("SELECT title FROM items WHERE id = ?", id).Scan(&title)
		state, reason := "live", ""
		if remove {
			state, reason = "withdrawn", str(d, "reason")
		}
		res, err := tx.Exec("INSERT INTO items (title, descr, state, withdrawn_reason, created, updated, rev) VALUES (?, ?, ?, ?, ?, ?, 1)",
			name, fmt.Sprintf("abgetrennt aus A%d %s", id, title), state, reason, now(), now())
		if err != nil {
			return err
		}
		to, _ = res.LastInsertId()
		d["id"] = to
		if _, err := tx.Exec("INSERT OR IGNORE INTO item_tags (item, tag, added_by, added_at) SELECT ?, tag, added_by, added_at FROM item_tags WHERE item = ?", to, id); err != nil {
			return err
		}
	}
	var ord int
	tx.QueryRow("SELECT COALESCE(MAX(ord), 0) + 1 FROM item_files WHERE item = ?", to).Scan(&ord)
	nn := name
	for i := 2; ; i++ {
		var c int
		tx.QueryRow("SELECT COUNT(*) FROM item_files WHERE item = ? AND name = ?", to, nn).Scan(&c)
		if c == 0 {
			break
		}
		nn = fmt.Sprintf("%d-%s", i, name)
	}
	role := "part"
	if num64(d, "into") == 0 {
		role = "main"
	}
	if _, err := tx.Exec("UPDATE item_files SET item = ?, name = ?, ord = ?, role = ? WHERE item = ? AND name = ?", to, nn, ord, role, id, name); err != nil {
		return err
	}
	if _, err := tx.Exec("INSERT OR REPLACE INTO redirects (old_path, item, name, at) VALUES (?, ?, ?, ?)", fmt.Sprintf("a/%d/%s", id, name), to, nn, now()); err != nil {
		return err
	}
	_, err := tx.Exec("UPDATE items SET rev = rev + 1, updated = ? WHERE id = ?", now(), to)
	return err
}

func freeSlugExcept(tx *sql.Tx, id int64, slug string) (string, error) {
	var facet string
	var parent sql.NullInt64
	if err := tx.QueryRow("SELECT facet, parent FROM tags WHERE id = ?", id).Scan(&facet, &parent); err != nil {
		return "", err
	}
	for i := 1; ; i++ {
		s := slug
		if i > 1 {
			s = fmt.Sprintf("%s-%d", slug, i)
		}
		var n int
		tx.QueryRow("SELECT COUNT(*) FROM tags WHERE facet = ? AND COALESCE(parent, 0) = ? AND slug = ? AND id != ?", facet, parent.Int64, s, id).Scan(&n)
		if n == 0 {
			return s, nil
		}
	}
}

// inverse is the change that undoes ch.
func inverse(ch Change) (Change, error) {
	d := map[string]any{}
	for k, v := range ch.Data {
		d[k] = v
	}
	inv := Change{Target: ch.Target, Data: d}
	switch ch.Op {
	case "set-field", "set-role", "set-page", "move-tag", "rename-file":
		inv.Op = ch.Op
		d["old"], d["new"] = ch.Data["new"], ch.Data["old"]
	case "add-tag", "add-tag-path":
		inv.Op = "remove-tag"
	case "remove-tag":
		inv.Op = "add-tag"
	case "coll-add":
		inv.Op = "coll-remove"
	case "coll-remove":
		inv.Op = "coll-add"
	case "set-coll":
		inv.Op = ch.Op
		d["old"], d["new"] = ch.Data["new"], ch.Data["old"]
	case "add-rel":
		inv.Op = "remove-rel"
	case "remove-rel":
		inv.Op = "add-rel"
	case "withdraw":
		inv.Op = "restore"
	case "restore":
		inv.Op = "withdraw"
	default:
		return inv, fmt.Errorf("%w: %s lässt sich nicht automatisch rückgängig machen", errInvalid, ch.Op)
	}
	return inv, nil
}

func newBatchID() string {
	return strconv.FormatInt(now(), 36) + "-" + randomHex(4)
}

// touchesTree tells whether changes alter the tag tree.
func touchesTree(changes []Change) bool {
	for _, ch := range changes {
		switch ch.Op {
		case "create-tag", "move-tag", "merge-tag", "add-tag-path":
			return true
		}
	}
	return false
}
