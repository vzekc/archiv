"""
The page-level text store of the collection: data/pages.sqlite.

Every document's text is kept page by page, without a length limit, so the
search can point at the page a hit is on (heft.pdf#page=17). Page 0 holds
text without page structure: plain text files, and OCR from before the store
existed.

    docs        one row per document: where its text came from, how much
    pages       the text, one row per page
    pages_fts   FTS5 index over pages (external content, kept by triggers)
    ocr_queue   scans waiting for bin/ocr-pdfs.py

Documents carry the SHA-256 of their bytes, taken from the catalog
(data/catalog.sqlite, written by "archiv sync"). A document whose
bytes are already known under another path takes that text over instead
of being read again: a moved file keeps it, a duplicate gets a copy.

Unlike collection.sqlite this database is never rebuilt; it is updated
document by document by extract-text.py and ocr-pdfs.py. It uses a rollback
journal rather than WAL so that search.php can read it with nothing but read
permission.

Shared by extract-text.py, ocr-pdfs.py, index-collection.py and ocr-status.
"""

import gzip
import os
import sys
import re
import shutil
import sqlite3
import time
import xml.etree.ElementTree as ET

# The installation: $ARCHIV_HOME, else the directory above bin/.
HOME = os.environ.get("ARCHIV_HOME") or os.path.dirname(os.path.dirname(os.path.abspath(sys.argv[0])))
DATA = os.path.join(HOME, "data")
PAGESDB = os.path.join(DATA, "pages.sqlite")
IA_TEXT = os.path.join(DATA, "ia-text")       # OCR text from archive.org items
LANG_CONF = os.path.join(DATA, "ocr-lang.conf")
CATALOG = os.path.join(DATA, "catalog.sqlite")

PAGE_MAX = 20_000        # characters per page; more is a broken text layer
MIN_USEFUL = 120         # fewer characters in a whole PDF: no text layer
THIN = 200               # average characters per page below which OCR may help

SCHEMA = """
CREATE TABLE IF NOT EXISTS docs (
    path    TEXT PRIMARY KEY,
    size    INTEGER NOT NULL,
    mtime   INTEGER NOT NULL,
    state   TEXT NOT NULL,          -- done | none | error
    tool    TEXT,                   -- pdftotext | archive.org | tesseract | plain | legacy-ocr
    n_pages INTEGER NOT NULL DEFAULT 0,
    chars   INTEGER NOT NULL DEFAULT 0,
    stamp   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS docs_state ON docs(state);

CREATE TABLE IF NOT EXISTS pages (
    id    INTEGER PRIMARY KEY,
    path  TEXT NOT NULL,
    page  INTEGER NOT NULL,         -- 1-based; 0 = no page structure
    body  TEXT NOT NULL,
    UNIQUE (path, page)
);

CREATE VIRTUAL TABLE IF NOT EXISTS pages_fts USING fts5(
    body, content = 'pages', content_rowid = 'id',
    tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER IF NOT EXISTS pages_ai AFTER INSERT ON pages BEGIN
    INSERT INTO pages_fts (rowid, body) VALUES (new.id, new.body);
END;
CREATE TRIGGER IF NOT EXISTS pages_ad AFTER DELETE ON pages BEGIN
    INSERT INTO pages_fts (pages_fts, rowid, body) VALUES ('delete', old.id, old.body);
END;
CREATE TRIGGER IF NOT EXISTS pages_au AFTER UPDATE ON pages BEGIN
    INSERT INTO pages_fts (pages_fts, rowid, body) VALUES ('delete', old.id, old.body);
    INSERT INTO pages_fts (rowid, body) VALUES (new.id, new.body);
END;

CREATE TABLE IF NOT EXISTS ocr_queue (
    path        TEXT PRIMARY KEY,
    priority    INTEGER NOT NULL,   -- 1 magazines without text, 2 thin text,
                                    -- 3 other scans, 4 redo of old whole-document OCR
    reason      TEXT,
    lang        TEXT NOT NULL,
    added       INTEGER NOT NULL,
    state       TEXT NOT NULL DEFAULT 'queued',   -- queued | running | done | error
    pages_total INTEGER,
    pages_done  INTEGER NOT NULL DEFAULT 0,
    started     INTEGER,
    finished    INTEGER,
    error       TEXT
);
CREATE INDEX IF NOT EXISTS ocr_queue_next ON ocr_queue(state, priority, added);
"""


def connect(path=None, timeout=120):
    path = path or PAGESDB
    db = sqlite3.connect(path, timeout=timeout)
    db.execute("PRAGMA journal_mode = DELETE")
    db.executescript(SCHEMA)
    for table in ("docs", "ocr_queue"):
        cols = {r[1] for r in db.execute(f"PRAGMA table_info({table})")}
        if "sha256" not in cols:
            db.execute(f"ALTER TABLE {table} ADD COLUMN sha256 TEXT")
    db.execute("CREATE INDEX IF NOT EXISTS docs_sha ON docs(sha256)")
    db.commit()
    try:
        os.chmod(path, 0o664)
    except OSError:
        pass
    return db


# ------------------------------------------------------------------ text

WS = re.compile(r"[ \t\r\v]+")
NL = re.compile(r"\n{3,}")
CTRL = re.compile(r"[\x00-\x08\x0b-\x0c\x0e-\x1f\x7f]")
HYPHEN = re.compile(r"(\w)[-­]\n(\w)")


def tidy_page(text, dehyphen=False):
    """Collapse whitespace, keep paragraphs; join words split at a line end."""
    text = CTRL.sub(" ", text)
    text = WS.sub(" ", text)
    text = text.replace(" \n", "\n")
    if dehyphen:
        text = HYPHEN.sub(r"\1\2", text)
    text = NL.sub("\n\n", text)
    return text.strip()[:PAGE_MAX]


def store_doc(db, path, size, mtime, state, tool, pages, sha=None):
    """Replace a document's text. pages: list of (page number, text)."""
    pages = [(n, t) for n, t in pages if t]
    with db:
        db.execute("DELETE FROM pages WHERE path = ?", (path,))
        db.executemany("INSERT INTO pages (path, page, body) VALUES (?,?,?)",
                       [(path, n, t) for n, t in pages])
        db.execute(
            "INSERT INTO docs (path, size, mtime, state, tool, n_pages, chars, stamp, sha256)"
            " VALUES (?,?,?,?,?,?,?,?,?)"
            " ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime,"
            " state=excluded.state, tool=excluded.tool, n_pages=excluded.n_pages,"
            " chars=excluded.chars, stamp=excluded.stamp, sha256=excluded.sha256",
            (path, size, mtime, state, tool, len(pages),
             sum(len(t) for _, t in pages), int(time.time()), sha))


# ------------------------------------------------------------------ hashes

class Catalog:
    """The hashes of the archive's files, as the last sync found them."""

    def __init__(self, path=None):
        self.by_path = {}       # path -> (sha256, size, mtime)
        self.live = set()       # paths the last sync found
        self.by_sha = {}        # sha256 -> [paths]
        path = path or CATALOG
        if not os.path.exists(path):
            return
        db = sqlite3.connect(f"file:{path}?mode=ro", uri=True, timeout=60)
        last = db.execute("SELECT value FROM meta WHERE key = 'last_sync'").fetchone()
        last = int(last[0]) if last else 0
        for p, sha, size, mtime, seen in db.execute(
                "SELECT path, sha256, size, mtime, seen FROM file_paths"):
            self.by_path[p] = (sha, size, mtime)
            self.by_sha.setdefault(sha, []).append(p)
            if seen >= last:
                self.live.add(p)
        db.close()

    def sha(self, path, size=None, mtime=None):
        """The hash of path, if the catalog has it for this size and mtime."""
        k = self.by_path.get(path)
        if not k or (size is not None and (k[1], k[2]) != (size, mtime)):
            return None
        return k[0]


def fill_hashes(db, cat):
    """Give every document the hash of its current bytes."""
    rows = db.execute("SELECT path, size, mtime, sha256 FROM docs").fetchall()
    with db:
        for path, size, mtime, sha in rows:
            new = cat.sha(path, size, mtime)
            if new and new != sha:
                db.execute("UPDATE docs SET sha256 = ? WHERE path = ?", (new, path))
                db.execute("UPDATE ocr_queue SET sha256 = ? WHERE path = ?", (new, path))


def take_over(db, cat, path, size, mtime):
    """
    Give path the text stored for the same bytes under another path: the rows
    move if that path is gone, else the pages are copied. Returns the state
    taken over, or None if there is nothing to take.
    """
    sha = cat.sha(path, size, mtime)
    if not sha:
        return None
    src = db.execute("SELECT path, state, tool FROM docs WHERE sha256 = ? AND path <> ?"
                     " ORDER BY (state = 'done') DESC, stamp DESC LIMIT 1", (sha, path)).fetchone()
    if not src:
        return None
    old, state, tool = src
    with db:
        db.execute("DELETE FROM pages WHERE path = ?", (path,))
        db.execute("DELETE FROM docs WHERE path = ?", (path,))
        if old not in cat.live:
            db.execute("UPDATE pages SET path = ? WHERE path = ?", (path, old))
            db.execute("UPDATE docs SET path = ?, size = ?, mtime = ? WHERE path = ?",
                       (path, size, mtime, old))
            if not db.execute("SELECT 1 FROM ocr_queue WHERE path = ?", (path,)).fetchone():
                db.execute("UPDATE ocr_queue SET path = ? WHERE path = ?", (path, old))
        else:
            db.execute("INSERT INTO pages (path, page, body)"
                       " SELECT ?, page, body FROM pages WHERE path = ?", (path, old))
            db.execute("INSERT INTO docs (path, size, mtime, state, tool, n_pages, chars, stamp, sha256)"
                       " SELECT ?, ?, ?, state, tool, n_pages, chars, ?, sha256 FROM docs WHERE path = ?",
                       (path, size, mtime, int(time.time()), old))
    return state


def take_over_image(cat, path, mtime, image_path):
    """
    The same for a thumbnail or preview: image_path(p) is where the picture
    of p lies. True if path has one now.
    """
    sha = cat.sha(path)
    dst = image_path(path)
    for old in cat.by_sha.get(sha, []) if sha else []:
        src = image_path(old)
        if old == path or not os.path.exists(src):
            continue
        try:
            if os.stat(src).st_mtime < mtime:
                continue        # made from older bytes than these
            os.makedirs(os.path.dirname(dst), exist_ok=True)
            if old in cat.live:
                shutil.copy2(src, dst)
            else:
                os.replace(src, dst)
            return True
        except OSError:
            continue
    return False


# ------------------------------------------------------------------ OCR queue

def lang_for(path, _cache={}):
    """
    tesseract language for a document, from data/ocr-lang.conf: lines of
    "<path prefix>  <lang>", longest prefix wins, default eng.
    """
    if "rules" not in _cache:
        rules = []
        try:
            with open(LANG_CONF, encoding="utf-8") as fh:
                for line in fh:
                    line = line.split("#", 1)[0].strip()
                    if line:
                        prefix, _, lang = line.rpartition(" ")
                        rules.append((prefix.strip(), lang.strip()))
        except OSError:
            pass
        _cache["rules"] = sorted(rules, key=lambda r: len(r[0]), reverse=True)
    for prefix, lang in _cache["rules"]:
        if path == prefix or path.startswith(prefix.rstrip("/") + "/"):
            return lang
    return "eng"


def enqueue(db, path, priority, reason):
    """Queue a document for OCR, or raise the priority of a queued one."""
    with db:
        db.execute(
            "INSERT INTO ocr_queue (path, priority, reason, lang, added)"
            " VALUES (?,?,?,?,?)"
            " ON CONFLICT(path) DO UPDATE SET priority = MIN(priority, excluded.priority),"
            " state = CASE WHEN state IN ('done', 'error') THEN 'queued' ELSE state END,"
            " pages_done = CASE WHEN state IN ('done', 'error') THEN 0 ELSE pages_done END",
            (path, priority, reason, lang_for(path), int(time.time())))


def dequeue(db, path):
    """Drop a document from the OCR queue unless it is being worked on."""
    with db:
        db.execute("DELETE FROM ocr_queue WHERE path = ? AND state <> 'running'", (path,))


def ocr_priority(path, reason):
    if reason == "thin":
        return 2
    return 1 if path.startswith("Zeitschriften/") else 3


# ------------------------------------------------------------- archive.org OCR

def ia_text_path(relpath):
    return os.path.join(IA_TEXT, relpath + ".txt.gz")


def djvu_xml_pages(fileobj):
    """
    Page texts from an archive.org *_djvu.xml: one <OBJECT> per page of the
    original PDF, words grouped into lines and paragraphs.
    """
    pages, lines, words, paras = [], [], [], []
    for event, el in ET.iterparse(fileobj, events=("end",)):
        tag = el.tag
        if tag == "WORD":
            if el.text:
                words.append(el.text)
        elif tag == "LINE":
            if words:
                lines.append(" ".join(words))
            words = []
        elif tag == "PARAGRAPH":
            if lines:
                paras.append("\n".join(lines))
            lines = []
        elif tag == "OBJECT":
            if lines:
                paras.append("\n".join(lines))
            pages.append("\n\n".join(paras))
            lines, words, paras = [], [], []
            el.clear()
    return pages


def write_ia_text(relpath, pages):
    dest = ia_text_path(relpath)
    os.makedirs(os.path.dirname(dest), exist_ok=True)
    tmp = dest + ".tmp"
    with gzip.open(tmp, "wt", encoding="utf-8") as fh:
        fh.write("\f".join(pages))
    os.replace(tmp, dest)


def read_ia_text(relpath):
    """Pages of the archive.org OCR for a document, or None."""
    try:
        with gzip.open(ia_text_path(relpath), "rt", encoding="utf-8") as fh:
            return fh.read().split("\f")
    except OSError:
        return None
