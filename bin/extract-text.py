#!/usr/bin/env python3
"""
Extract the text of documents in the collection, page by page, into the
page store (data/pages.sqlite, see textstore.py) that the search reads.

Where the text comes from, in this order:

  * the PDF's own text layer (pdftotext, one entry per page);
  * for scans fetched from archive.org, the OCR text that came with them
    (data/ia-text/, written by fetch-magazines.py);
  * otherwise the PDF is queued for bin/ocr-pdfs.py. A text layer that is
    there but very thin (a few header lines per page) is kept and the PDF is
    queued as well.

Plain text files go in as one block without page structure (page 0).

A document is read again only when its size or mtime changed, so the nightly
run costs little. A document whose bytes are already in the store under
another path (it was moved, or it is a duplicate) takes that text over; the
hashes come from the catalog, so "archiv sync" runs first. --redo forces documents in the given states to be read
again; --import-legacy carries the texts of the old textcache.sqlite over
once (see there).

Usage:
    extract-text.py                    # PDFs that are new or changed
    extract-text.py --ext pdf,txt      # PDFs and plain text files
    extract-text.py --limit 100        # a taste of it
    extract-text.py --only Zeitschriften/_DEC
    extract-text.py --import-legacy    # once: take over data/textcache.sqlite
"""

import argparse
import os
import sqlite3
import subprocess
import sys
import time
from concurrent.futures import ThreadPoolExecutor

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import textstore  # noqa: E402

DOCROOT = os.path.join(textstore.HOME, "html")
DBPATH = os.path.join(textstore.HOME, "data", "collection.sqlite")
LEGACY = os.path.join(textstore.HOME, "data", "textcache.sqlite")

PDF_TIMEOUT = 300        # seconds per document; magazines have 300+ pages
PLAIN_MAX = 2_000_000    # characters of a plain text file


# Control bytes that legitimately occur in CP/M-era text: tab, LF, form feed,
# CR, and 0x1A, which marks end-of-file and often pads the last record.
TEXTISH_CONTROL = {0x09, 0x0A, 0x0C, 0x0D, 0x1A}


def looks_binary(raw):
    """
    Guard for the broader file types: a .dat or .bin that happens to be listed
    would otherwise be decoded as cp1252 and fill the index with noise.
    """
    if not raw:
        return False
    sample = raw[:8192]
    if sample.count(0) > len(sample) * 0.01:
        return True
    ctrl = sum(1 for b in sample
               if b < 0x20 and b not in TEXTISH_CONTROL)
    return ctrl > len(sample) * 0.08


def looks_like_wordstar(raw):
    """
    WordStar and several CP/M editors set the 8th bit on the last character of
    each word. Decoded as cp1252 that turns "The following is an extract" into
    "Thå followinç iò aî extracô" -- every word ends in a wrong letter, and the
    text becomes useless for searching. Detect it: many high bytes, and nearly
    all of them printable ASCII once the high bit is cleared.
    """
    high = sum(1 for b in raw if b >= 0x80)
    if high < len(raw) * 0.02:
        return False
    clean = sum(1 for b in raw if b >= 0x80 and 32 <= (b & 0x7F) < 127)
    return clean > high * 0.9


def extract_pdf(relpath):
    """(state, tool, [(page, text)], queue reason or None)"""
    full = os.path.join(DOCROOT, relpath)
    try:
        proc = subprocess.run(["pdftotext", "-q", "-enc", "UTF-8", full, "-"],
                              capture_output=True, timeout=PDF_TIMEOUT)
    except subprocess.TimeoutExpired:
        return "error", "pdftotext:timeout", [], None
    except OSError as exc:
        return "error", f"pdftotext:{exc.errno}", [], None

    # pdftotext ends every page with a form feed
    raw = proc.stdout.decode("utf-8", errors="replace").split("\f")
    if raw and not raw[-1].strip():
        raw.pop()
    pages = [(n, textstore.tidy_page(t)) for n, t in enumerate(raw, 1)]
    chars = sum(len(t) for _, t in pages)
    n_pages = max(1, len(raw))

    if chars >= textstore.MIN_USEFUL and chars / n_pages >= textstore.THIN:
        return "done", "pdftotext", pages, None

    # a scan: no text layer, or only a little; archive.org may have OCRed it
    ia = textstore.read_ia_text(relpath)
    if ia:
        ia_pages = [(n, textstore.tidy_page(t, dehyphen=True)) for n, t in enumerate(ia, 1)]
        if sum(len(t) for _, t in ia_pages) > chars:
            return "done", "archive.org", ia_pages, None
    if chars < textstore.MIN_USEFUL:
        return "none", "pdftotext", [], "none"
    if n_pages >= 4:
        return "done", "pdftotext", pages, "thin"
    return "done", "pdftotext", pages, None


def extract_plain(relpath):
    full = os.path.join(DOCROOT, relpath)
    try:
        with open(full, "rb") as fh:
            raw = fh.read(PLAIN_MAX * 2)
    except OSError as exc:
        return "error", f"read:{exc.errno}", [], None

    if looks_binary(raw):
        return "none", "plain:binary", [], None

    tool = "plain"
    if looks_like_wordstar(raw):
        raw = bytes(b & 0x7F for b in raw)
        tool = "plain:wordstar"

    for enc in ("utf-8", "cp1252"):
        try:
            text = raw.decode(enc)
            break
        except UnicodeDecodeError:
            continue
    else:
        text = raw.decode("cp1252", errors="replace")
    # a plain file has no pages: one block, page 0, without the page limit
    text = textstore.CTRL.sub(" ", text)
    text = textstore.WS.sub(" ", text)
    text = textstore.NL.sub("\n\n", text).strip()[:PLAIN_MAX]
    # A short text file is still text: unlike a PDF there is no such thing as
    # a plain file "without a text layer", so MIN_USEFUL must not apply here.
    if not text:
        return "none", tool, [], None
    return "done", tool, [(0, text)], None


def extract(item):
    if item["ext"] == "pdf":
        return extract_pdf(item["path"])
    return extract_plain(item["path"])


def import_legacy(store, db):
    """
    Carry the old textcache.sqlite over, once. Its OCR results (tesseract, many
    hours of work) have no page structure; they go in as page 0, stay
    searchable, and are queued for page-wise OCR at the lowest priority. Text
    from pdftotext is not taken over -- reading it again page by page is cheap.
    """
    if not os.path.exists(LEGACY):
        print("no legacy text cache")
        return
    current = {r[0]: (r[1], r[2]) for r in db.execute("SELECT path, size, mtime FROM files")}
    legacy = sqlite3.connect(LEGACY)
    n_ocr = n_plain = 0
    for path, size, mtime, state, body, tool in legacy.execute(
            "SELECT path, size, mtime, state, body, tool FROM texts"
            " WHERE state = 'done' AND (tool LIKE 'tesseract%' OR tool LIKE 'plain%')"):
        if current.get(path) != (size, mtime):
            continue
        if store.execute("SELECT 1 FROM docs WHERE path = ?", (path,)).fetchone():
            continue
        if tool.startswith("tesseract"):
            textstore.store_doc(store, path, size, mtime, "done", "legacy-ocr", [(0, body)])
            textstore.enqueue(store, path, 4, "legacy")
            n_ocr += 1
        else:
            textstore.store_doc(store, path, size, mtime, "done", tool, [(0, body)])
            n_plain += 1
    print(f"legacy: {n_ocr} OCR texts (queued for page-wise OCR), {n_plain} plain texts")


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", default=DBPATH)
    ap.add_argument("--store", default=textstore.PAGESDB)
    ap.add_argument("--catalog", default=textstore.CATALOG)
    ap.add_argument("--ext", default="pdf", help="comma separated, default pdf")
    ap.add_argument("--only", default="", help="only paths below this prefix")
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--jobs", type=int, default=4)
    ap.add_argument("--redo", default="", help="also redo these states, e.g. none,error")
    ap.add_argument("--import-legacy", action="store_true")
    ap.add_argument("--quiet", action="store_true")
    args = ap.parse_args()

    exts = [e.strip().lower().lstrip(".") for e in args.ext.split(",") if e.strip()]
    redo = {s.strip() for s in args.redo.split(",") if s.strip()}

    store = textstore.connect(args.store)
    db = sqlite3.connect(args.db, timeout=60)
    db.row_factory = sqlite3.Row

    if args.import_legacy:
        import_legacy(store, db)

    cat = textstore.Catalog(args.catalog)
    textstore.fill_hashes(store, cat)

    # what is already known, and still current; tesseract results are never
    # overwritten by a fresh pdftotext pass
    known = {r[0]: r[1:] for r in store.execute("SELECT path, size, mtime, state, tool FROM docs")}

    todo = []
    taken = 0
    placeholders = ",".join("?" * len(exts))
    for row in db.execute(
            f"SELECT path, ext, size, mtime FROM files WHERE ext IN ({placeholders})"
            " ORDER BY path", exts):
        if args.only and not row["path"].startswith(args.only):
            continue
        prev = known.get(row["path"])
        if prev and prev[0] == row["size"] and prev[1] == row["mtime"]:
            if prev[2] not in redo or prev[3] == "tesseract":
                continue
        elif textstore.take_over(store, cat, row["path"], row["size"], row["mtime"]):
            taken += 1
            continue
        todo.append(dict(row))
    if args.limit:
        todo = todo[:args.limit]

    if taken and not args.quiet:
        print(f"text taken over for {taken} moved or duplicate documents")
    if not todo:
        if not args.quiet:
            print("nothing to extract")
        return 0
    if not args.quiet:
        print(f"extracting text from {len(todo)} documents with {args.jobs} workers", flush=True)

    started = time.time()
    counts = {"done": 0, "none": 0, "error": 0}
    pages_total = chars_total = queued = 0

    def work(item):
        return item, extract(item)

    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        for n, (item, (state, tool, pages, reason)) in enumerate(pool.map(work, todo), 1):
            textstore.store_doc(store, item["path"], item["size"], item["mtime"],
                                state, tool, pages, cat.sha(item["path"], item["size"], item["mtime"]))
            if reason:
                textstore.enqueue(store, item["path"],
                                  textstore.ocr_priority(item["path"], reason), reason)
                queued += 1
            elif state == "done":
                # it has text now (changed file, or archive.org text arrived)
                textstore.dequeue(store, item["path"])
            counts[state] = counts.get(state, 0) + 1
            pages_total += len(pages)
            chars_total += sum(len(t) for _, t in pages)
            if not args.quiet and n % 200 == 0:
                rate = n / max(1e-9, time.time() - started)
                print(f"  ... {n}/{len(todo)} ({rate:.1f}/s)", flush=True)

    db.close()
    if not args.quiet:
        took = time.time() - started
        print(f"text extracted: {counts['done']} documents, {pages_total} pages "
              f"({chars_total / 1e6:.1f} million characters), "
              f"{counts['none']} without a text layer, {counts['error']} failed, "
              f"{queued} queued for OCR, in {took:.0f}s")
    return 0


if __name__ == "__main__":
    sys.exit(main())
