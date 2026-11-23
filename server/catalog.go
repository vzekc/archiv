package archiv

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// The catalog: every artifact of the archive as an item with a stable id,
// its files by SHA-256, tags in facets, relations, history and feedback.
// The service is its only writer; the Python jobs read it.

const catalogSchemaVersion = 3

// migrations bring an older catalog up to the schema above; index i takes
// version i+1 to i+2.
var migrations = []string{
	`ALTER TABLE item_files ADD COLUMN descr TEXT NOT NULL DEFAULT ''`,
	// tag names from folder names, readable (see folderTitle)
	`UPDATE tags SET name = trim(replace(name, '_', ' ')) WHERE rev = 0 AND name LIKE '%\_%' ESCAPE '\' AND trim(replace(name, '_', ' ')) != ''`,
}

const catalogSchema = `
PRAGMA journal_mode = WAL;

CREATE TABLE IF NOT EXISTS files (
    sha256   TEXT PRIMARY KEY,
    size     INTEGER NOT NULL,
    kind     TEXT NOT NULL DEFAULT '',   -- pdf, image, archive, disk, iso, text, other
    pages    INTEGER,                    -- PDFs
    added    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS file_paths (  -- where the bytes lie; several = duplicates
    path     TEXT PRIMARY KEY,           -- relative to the archive root
    sha256   TEXT NOT NULL REFERENCES files,
    size     INTEGER NOT NULL,
    mtime    INTEGER NOT NULL,           -- with size: "unchanged, no need to hash again"
    seen     INTEGER NOT NULL            -- last sync that found it
);
CREATE INDEX IF NOT EXISTS file_paths_sha ON file_paths(sha256);

CREATE TABLE IF NOT EXISTS items (
    id       INTEGER PRIMARY KEY,        -- the citable number, never reused
    title    TEXT NOT NULL,
    descr    TEXT NOT NULL DEFAULT '',
    year     TEXT NOT NULL DEFAULT '',   -- free form: 1983, 1983-85, ca. 1980
    state    TEXT NOT NULL DEFAULT 'live', -- live, missing (files gone from disk), withdrawn, merged
    merged_into INTEGER REFERENCES items,
    withdrawn_reason TEXT NOT NULL DEFAULT '',
    root     TEXT,                       -- folder of a multi-file item (mirror, disk set), relative path
    created  INTEGER NOT NULL,
    updated  INTEGER NOT NULL,
    rev      INTEGER NOT NULL DEFAULT 0  -- bumped on every edit; 0 = as imported
);
CREATE INDEX IF NOT EXISTS items_root ON items(root);
CREATE TABLE IF NOT EXISTS item_files (
    item     INTEGER NOT NULL REFERENCES items,
    sha256   TEXT NOT NULL REFERENCES files,
    name     TEXT NOT NULL,              -- name within the item, may contain '/'
    path     TEXT NOT NULL,              -- where it lies, relative to the archive root
    ord      INTEGER NOT NULL,
    role     TEXT NOT NULL DEFAULT 'main', -- main, part, cover, extra
    descr    TEXT NOT NULL DEFAULT '',   -- what this file of the item is (disk 3 of 5, label ...)
    PRIMARY KEY (item, name)
);
CREATE INDEX IF NOT EXISTS item_files_sha ON item_files(sha256);
CREATE UNIQUE INDEX IF NOT EXISTS item_files_path ON item_files(path);

CREATE TABLE IF NOT EXISTS tags (
    id       INTEGER PRIMARY KEY,
    facet    TEXT NOT NULL,              -- machine, kind, publication, event, system,
                                         -- language, period, source, group, location
    parent   INTEGER REFERENCES tags,
    scope    INTEGER REFERENCES tags,    -- group tags only: the machine they belong to
    name     TEXT NOT NULL,
    slug     TEXT NOT NULL,
    sort     INTEGER NOT NULL DEFAULT 0, -- explicit order (sections); else by name
    section  INTEGER NOT NULL DEFAULT 0, -- kind tags shown as sections under machines
    descr    TEXT NOT NULL DEFAULT '',
    path     TEXT,                       -- location tags: the folder, relative to the archive root
    merged_into INTEGER REFERENCES tags,
    rev      INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS tags_slug ON tags(facet, COALESCE(parent, 0), slug);
CREATE UNIQUE INDEX IF NOT EXISTS tags_path ON tags(path);
CREATE INDEX IF NOT EXISTS tags_parent ON tags(parent);
CREATE TABLE IF NOT EXISTS item_tags (
    item     INTEGER NOT NULL REFERENCES items,
    tag      INTEGER NOT NULL REFERENCES tags,
    added_by TEXT NOT NULL,              -- user id, 'import', 'import:weak', 'ai:<run>'
    added_at INTEGER NOT NULL,
    PRIMARY KEY (item, tag)
);
CREATE INDEX IF NOT EXISTS item_tags_tag ON item_tags(tag);

CREATE TABLE IF NOT EXISTS item_rel (
    a INTEGER NOT NULL REFERENCES items,
    kind TEXT NOT NULL,                  -- part-of, same-as, newer-version-of, translation-of, documents
    b INTEGER NOT NULL REFERENCES items,
    PRIMARY KEY (a, kind, b)
);

CREATE TABLE IF NOT EXISTS collections (id INTEGER PRIMARY KEY, title TEXT NOT NULL, intro TEXT NOT NULL DEFAULT '',
    owner TEXT NOT NULL, created INTEGER NOT NULL, rev INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS coll_items (coll INTEGER NOT NULL REFERENCES collections, item INTEGER NOT NULL REFERENCES items,
    ord INTEGER NOT NULL, note TEXT NOT NULL DEFAULT '', PRIMARY KEY (coll, item));

CREATE TABLE IF NOT EXISTS tag_pages (   -- page text of a machine, publication, event
    tag INTEGER PRIMARY KEY REFERENCES tags,
    body TEXT NOT NULL,                  -- Markdown
    updated INTEGER NOT NULL,
    rev INTEGER NOT NULL DEFAULT 0
);

-- Every change to items, tags, relations, collections and pages, applied or proposed.
CREATE TABLE IF NOT EXISTS revisions (
    id       INTEGER PRIMARY KEY,
    target   TEXT NOT NULL,              -- 'item:12345', 'tag:17', 'page:17', 'coll:3'
    op       TEXT NOT NULL,              -- set-field, add-tag, remove-tag, add-rel, merge, move-file, ...
    data     TEXT NOT NULL,              -- json: old and new values
    user     TEXT NOT NULL,
    at       INTEGER NOT NULL,
    reason   TEXT NOT NULL DEFAULT '',
    state    TEXT NOT NULL,              -- applied, proposed, rejected, reverted
    batch    TEXT,                       -- one user action touching many targets
    decided_by TEXT, decided_at INTEGER
);
CREATE INDEX IF NOT EXISTS revisions_target ON revisions(target, at);
CREATE INDEX IF NOT EXISTS revisions_state ON revisions(state, at);

CREATE TABLE IF NOT EXISTS comments (
    id       INTEGER PRIMARY KEY,
    target   TEXT NOT NULL,              -- 'item:12345', 'tag:17'
    parent   INTEGER REFERENCES comments,
    user     TEXT NOT NULL,
    at       INTEGER NOT NULL,
    kind     TEXT NOT NULL,              -- comment, broken, missing-pages, wrong-descr, duplicate,
                                         -- elsewhere, copyright
    body     TEXT NOT NULL,
    state    TEXT NOT NULL DEFAULT 'open', -- reports: open, done, dismissed
    decided_by TEXT, decided_at INTEGER
);

CREATE TABLE IF NOT EXISTS redirects (   -- old URL path -> where it went
    old_path TEXT PRIMARY KEY,
    item     INTEGER REFERENCES items,
    name     TEXT,                       -- file within the item, or NULL for the item page
    at       INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tag_redirects (old_slug_path TEXT PRIMARY KEY, tag INTEGER NOT NULL REFERENCES tags);

-- full text over the catalog, rebuilt by sync and kept up to date by edits
CREATE VIRTUAL TABLE IF NOT EXISTS items_fts USING fts5(title, descr, tags, names,
    tokenize = 'unicode61 remove_diacritics 2');

-- notifications for the Messenger extension (catalog_notify.go)
CREATE TABLE IF NOT EXISTS notify_jobs (
    id       INTEGER PRIMARY KEY,
    user     TEXT NOT NULL,              -- user id; '' = all moderators
    subject  TEXT NOT NULL,
    html     TEXT NOT NULL,
    tries    INTEGER NOT NULL DEFAULT 0,
    next_try INTEGER NOT NULL DEFAULT 0,
    error    TEXT NOT NULL DEFAULT ''
);

-- every tag with each tag above it and itself: "DEC" covers "DEC > VAX";
-- rebuilt whenever the tree changes (rebuildClosure)
CREATE TABLE IF NOT EXISTS tag_closure (
    tag      INTEGER NOT NULL,
    ancestor INTEGER NOT NULL,
    depth    INTEGER NOT NULL,           -- 0 = the tag itself
    PRIMARY KEY (ancestor, tag)
);
CREATE INDEX IF NOT EXISTS tag_closure_tag ON tag_closure(tag);

CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT);
`

type Catalog struct{ *sql.DB }

func openCatalog(path string) (*Catalog, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(30000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(catalogSchema); err != nil {
		return nil, fmt.Errorf("catalog schema: %w", err)
	}
	var version int
	if err := db.QueryRow("SELECT CAST(value AS INTEGER) FROM meta WHERE key = 'schema_version'").Scan(&version); err == sql.ErrNoRows {
		version = catalogSchemaVersion // a new catalog has the current schema
	} else if err != nil {
		return nil, err
	}
	for ; version < catalogSchemaVersion; version++ {
		if _, err := db.Exec(migrations[version-1]); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return nil, fmt.Errorf("catalog migration to %d: %w", version+1, err)
		}
	}
	if _, err := db.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('schema_version', ?)", catalogSchemaVersion); err != nil {
		return nil, err
	}
	os.Chmod(path, 0o664)
	return &Catalog{db}, nil
}

// rebuildClosure writes tag_closure anew from the parents of the tags.
func rebuildClosure(q interface {
	Exec(string, ...any) (sql.Result, error)
}) error {
	if _, err := q.Exec("DELETE FROM tag_closure"); err != nil {
		return err
	}
	_, err := q.Exec(`INSERT INTO tag_closure (tag, ancestor, depth)
	    WITH RECURSIVE up(tag, ancestor, depth) AS (
	        SELECT id, id, 0 FROM tags
	        UNION ALL SELECT up.tag, t.parent, up.depth + 1 FROM up JOIN tags t ON t.id = up.ancestor WHERE t.parent IS NOT NULL AND up.depth < 12)
	    SELECT tag, ancestor, MIN(depth) FROM up GROUP BY tag, ancestor`)
	return err
}

func (c *Catalog) meta(key string) string {
	var v string
	c.QueryRow("SELECT value FROM meta WHERE key = ?", key).Scan(&v)
	return v
}

func (c *Catalog) setMeta(key string, v any) {
	c.Exec("INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value", key, fmt.Sprint(v))
}

// slugify turns a name into the URL part of a tag: lower case ASCII,
// umlauts spelled out, anything else a hyphen.
func slugify(name string) string {
	repl := strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "Ä", "ae", "Ö", "oe", "Ü", "ue", "&", " und ", "+", " plus ")
	s := norm.NFC.String(name)
	s = repl.Replace(s)
	var b strings.Builder
	dash := false
	for _, r := range norm.NFD.String(strings.ToLower(s)) {
		switch {
		case unicode.Is(unicode.Mn, r), r == '\'', r == '’':
			// accents and apostrophes go, the letter stays
		case r < 128 && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case r == '.' || r == '_':
			b.WriteRune(r)
			dash = false
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		out = "x"
	}
	return out
}
