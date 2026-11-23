#!/usr/bin/env python3
"""
Build the index of the archive's files (data/collection.sqlite).

Walks the document root, parses the per-directory `descriptions` files that
Filepage left behind (CRLF, Latin-1/cp1252, "<filename><space><description>",
with an optional "title=" header line), and writes an SQLite database with an
FTS5 index over file names, descriptions, directory titles and -- once
extract-text.py has run -- the full text of documents.

The database is written to a temporary file and moved into place at the end,
so the search page never sees a half-built index.

Usage:  index-collection.py [--root DIR] [--db FILE] [--quiet]
"""

import argparse
import os
import sqlite3
import sys
import tempfile
import time

# The installation: $ARCHIV_HOME, else the directory above bin/.
HOME = os.environ.get("ARCHIV_HOME") or os.path.dirname(os.path.dirname(os.path.abspath(sys.argv[0])))
DOCROOT = os.path.join(HOME, "html")
DBPATH = os.path.join(HOME, "data", "collection.sqlite")
CACHEPATH = os.path.join(HOME, "data", "pages.sqlite")   # page store, see textstore.py

# Filepage scaffolding and site furniture: present in almost every directory,
# never interesting as a search result.
SKIP_NAMES = {
    "descriptions", "banner", "adbanner", "adfooter", "index.html",
    "index.html.bak", "index.old", "favicon.ico", "robots.txt", "sitemap.xml",
    "filepage.cnf", "exclude.cnf", "extension.cnf", "descriptions.bak",
    "flag_de.gif", "alldir.txt", "search.php",
}
# Layout images and our own files, at the top level only -- the mirrors
# contain directories of the same name that ARE content.
SKIP_TOPLEVEL_DIRS = {"gif", "icon", "assets", "thumbs", "previews"}

# Human-facing grouping, derived from the first path segments below public/pub.
CATEGORY_LABELS = {
    "rechner": "Rechner",
    "manuals": "Handbücher",
    "cdrom": "CD-ROMs",
    "archiv": "Archiv",
    "mirror": "Mirrors",
    "hifi": "HiFi",
    "program": "Programme",
    "roms": "ROMs",
    "saved_mirrors": "Gesicherte Mirrors",
}


def decode_text(raw):
    """descriptions files are cp1252; fall back gracefully."""
    for enc in ("utf-8", "cp1252"):
        try:
            return raw.decode(enc)
        except UnicodeDecodeError:
            continue
    return raw.decode("cp1252", errors="replace")


def parse_descriptions(path):
    """
    Return (title, [(entry_text, description), ...]) for one descriptions file.

    Entries are returned verbatim; matching them to real directory entries is
    done by the caller, which knows what is actually on disk.
    """
    try:
        with open(path, "rb") as fh:
            raw = fh.read()
    except OSError:
        return None, []

    title = None
    lines = []
    for line in decode_text(raw).splitlines():
        line = line.rstrip("\r\n").rstrip()
        if not line:
            continue
        low = line.lower()
        if low.startswith("title=") and title is None:
            title = line.split("=", 1)[1].strip()
            continue
        lines.append(line)
    return title, lines


def match_entries(lines, names):
    """
    Map description lines onto real directory entries.

    A line is "<name><whitespace><description>", but names may themselves
    contain spaces, and the descriptions files preserve the original mixed
    case while everything on disk is lowercase. So: find the LONGEST entry
    name that the line starts with (case-insensitively) on a word boundary.
    """
    by_lower = {}
    for n in names:
        by_lower.setdefault(n.lower(), n)
    # longest first, so "sirius1_p1.bin" wins over a shorter prefix
    ordered = sorted(by_lower.keys(), key=len, reverse=True)

    out = {}
    for line in lines:
        low = line.lower()
        for cand in ordered:
            if not low.startswith(cand):
                continue
            rest = line[len(cand):]
            if rest and rest[0] not in " \t":
                continue  # matched a prefix of a longer name, not a name
            real = by_lower[cand]
            descr = rest.strip()
            # keep the first, most specific description we see
            if descr and real not in out:
                out[real] = descr
            break
    return out


SCHEMA = """
PRAGMA journal_mode = OFF;
PRAGMA synchronous = OFF;

CREATE TABLE dirs (
    id        INTEGER PRIMARY KEY,
    path      TEXT UNIQUE NOT NULL,   -- relative to docroot, '' is the root
    name      TEXT NOT NULL,
    parent    TEXT,
    title     TEXT,                   -- from the descriptions "title=" line
    descr     TEXT,                   -- description given by the PARENT dir
    category  TEXT,
    depth     INTEGER NOT NULL,
    n_files   INTEGER NOT NULL DEFAULT 0,
    n_subdirs INTEGER NOT NULL DEFAULT 0,
    bytes     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX dirs_parent ON dirs(parent);

CREATE TABLE files (
    id        INTEGER PRIMARY KEY,
    path      TEXT UNIQUE NOT NULL,
    dir       TEXT NOT NULL,
    name      TEXT NOT NULL,
    ext       TEXT,
    size      INTEGER NOT NULL,
    mtime     INTEGER NOT NULL,
    descr     TEXT,
    category  TEXT,
    body      TEXT,                   -- full text, filled in by extract-text.py
    body_state TEXT NOT NULL DEFAULT 'pending'  -- pending|done|none|error
);
CREATE INDEX files_dir ON files(dir);
CREATE INDEX files_ext ON files(ext);
CREATE INDEX files_body_state ON files(body_state);

-- rowid is kept in step with files.id so results can be joined back
CREATE VIRTUAL TABLE search USING fts5(
    name, descr, dirtitle, body,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT);
"""


def category_of(relpath):
    parts = relpath.split("/")
    if len(parts) >= 3 and parts[0] == "public" and parts[1] == "pub":
        return CATEGORY_LABELS.get(parts[2], parts[2])
    if len(parts) >= 2 and parts[0] == "public":
        return CATEGORY_LABELS.get(parts[1], parts[1])
    return None


def build(root, dbpath, quiet=False, cachepath=CACHEPATH):
    started = time.time()
    tmpdir = os.path.dirname(dbpath) or "."
    fd, tmppath = tempfile.mkstemp(dir=tmpdir, prefix=".collection-", suffix=".sqlite")
    os.close(fd)
    os.unlink(tmppath)

    db = sqlite3.connect(tmppath)
    db.executescript(SCHEMA)

    n_dirs = n_files = n_descr = 0
    file_rows = []
    dir_rows = []
    # descriptions a directory received from its parent
    dir_descr = {}

    for dirpath, dirnames, filenames in os.walk(root, topdown=True):
        rel = os.path.relpath(dirpath, root)
        rel = "" if rel == "." else rel.replace(os.sep, "/")

        dirnames[:] = sorted(
            d for d in dirnames
            if not (rel == "" and d in SKIP_TOPLEVEL_DIRS))
        filenames = sorted(filenames)

        title, lines = parse_descriptions(os.path.join(dirpath, "descriptions"))
        entries = match_entries(lines, filenames + dirnames) if lines else {}
        n_descr += len(entries)

        # .part: downloads in progress (fetch-magazines.py)
        visible = [f for f in filenames if f not in SKIP_NAMES and not f.endswith(".part")]
        total_bytes = 0

        for name in visible:
            full = os.path.join(dirpath, name)
            try:
                st = os.lstat(full)
            except OSError:
                continue
            if not os.path.isfile(full):
                continue
            total_bytes += st.st_size
            relfile = f"{rel}/{name}" if rel else name
            ext = os.path.splitext(name)[1].lstrip(".").lower() or None
            file_rows.append((
                relfile, rel, name, ext, st.st_size, int(st.st_mtime),
                entries.get(name), category_of(rel),
            ))
            n_files += 1

        for d in dirnames:
            child = f"{rel}/{d}" if rel else d
            if d in entries:
                dir_descr[child] = entries[d]

        parent = None if rel == "" else (os.path.dirname(rel) or "")
        dir_rows.append((
            rel, os.path.basename(rel) if rel else "/", parent, title,
            category_of(rel) if rel else None,
            0 if rel == "" else rel.count("/") + 1,
            len(visible), len(dirnames), total_bytes,
        ))
        n_dirs += 1

        if not quiet and sys.stderr.isatty() and n_dirs % 500 == 0:
            print(f"  ... {n_dirs} dirs, {n_files} files", file=sys.stderr)

    db.executemany(
        "INSERT INTO files (path, dir, name, ext, size, mtime, descr, category)"
        " VALUES (?,?,?,?,?,?,?,?)", file_rows)
    db.executemany(
        "INSERT INTO dirs (path, name, parent, title, category, depth,"
        " n_files, n_subdirs, bytes) VALUES (?,?,?,?,?,?,?,?,?)", dir_rows)

    # attach the parent-supplied description to each directory
    db.executemany("UPDATE dirs SET descr = ? WHERE path = ?",
                   [(v, k) for k, v in dir_descr.items()])

    # The document text itself lives page by page in the page store
    # (data/pages.sqlite), which search.php reads directly; this index only
    # learns which files have text, so rebuilding it stays cheap. A row counts
    # only while size and mtime still match what was extracted.
    n_body = 0
    if cachepath and os.path.exists(cachepath):
        db.execute("ATTACH DATABASE ? AS cache", (cachepath,))
        db.execute("""
            UPDATE files SET
                body_state = (SELECT c.state FROM cache.docs c
                              WHERE c.path = files.path AND c.size = files.size
                                AND c.mtime = files.mtime)
            WHERE EXISTS (SELECT 1 FROM cache.docs c
                          WHERE c.path = files.path AND c.size = files.size
                            AND c.mtime = files.mtime)
        """)
        n_body = db.execute(
            "SELECT COUNT(*) FROM files WHERE body_state = 'done'").fetchone()[0]
        # the UPDATE above holds a read transaction on the attached database;
        # it has to be closed before SQLite lets go of the cache file
        db.commit()
        db.execute("DETACH DATABASE cache")

    # feed the FTS index; rowid mirrors files.id
    db.execute("""
        INSERT INTO search (rowid, name, descr, dirtitle, body)
        SELECT f.id, f.name, COALESCE(f.descr, ''),
               COALESCE(d.title, '') || ' ' || f.dir, COALESCE(f.body, '')
        FROM files f LEFT JOIN dirs d ON d.path = f.dir
    """)

    db.executemany("INSERT INTO meta (key, value) VALUES (?,?)", [
        ("built_at", str(int(time.time()))),
        ("root", root),
        ("n_files", str(n_files)),
        ("n_dirs", str(n_dirs)),
        ("n_descriptions", str(n_descr)),
        ("n_fulltext", str(n_body)),
    ])
    db.commit()
    db.execute("VACUUM")
    db.close()

    os.chmod(tmppath, 0o664)   # the web service's group reads it, too
    os.replace(tmppath, dbpath)

    if not quiet:
        took = time.time() - started
        print(f"indexed {n_files} files in {n_dirs} directories "
              f"({n_descr} descriptions, {n_body} with full text) "
              f"in {took:.1f}s -> {dbpath}")
    return 0


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--root", default=DOCROOT)
    ap.add_argument("--db", default=DBPATH)
    ap.add_argument("--cache", default=CACHEPATH)
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args()

    if not os.path.isdir(args.root):
        sys.exit(f"document root not found: {args.root}")
    os.makedirs(os.path.dirname(args.db), exist_ok=True)
    return build(args.root, args.db, args.quiet, args.cache)


if __name__ == "__main__":
    sys.exit(main())
