#!/usr/bin/env python3
"""
OCR the scanned PDFs waiting in the queue of the page store
(data/pages.sqlite, filled by extract-text.py) and store their text page by
page, so the search can point at the page a hit is on.

Every page is rendered at 200 dpi in grayscale and read by tesseract in the
language data/ocr-lang.conf gives for its folder (default English). Each page
is committed as soon as it is read: an interrupted run, or a restart of the
service, continues in the middle of the issue where it stopped.

Order: magazines without text first, then thin text layers, then other
scans, then the old whole-document OCR that is to be redone page by page.

A service runs it with --daemon: it works through
the queue, waits for new entries when it is empty, uses at most --day-jobs
workers in the daytime and --night-jobs between 0 and 7 o'clock, and pauses
while the load average is above --max-load -- other services on the same machine
come first.

Usage:
    ocr-pdfs.py                    # work through the queue, then stop
    ocr-pdfs.py --daemon           # keep going (systemd)
    ocr-pdfs.py --limit 3 --jobs 1 # a taste of it
"""

import argparse
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import textstore  # noqa: E402

DOCROOT = os.path.join(textstore.HOME, "html")
PAGE_TIMEOUT = 180       # seconds for one page, rendering and recognition


def log(*a):
    print(time.strftime("%Y-%m-%d %H:%M:%S"), *a, flush=True)


def available_langs():
    try:
        out = subprocess.run(["tesseract", "--list-langs"], capture_output=True,
                             text=True, timeout=30).stdout
    except (OSError, subprocess.TimeoutExpired):
        return set()
    return {l.strip() for l in out.splitlines()[1:] if l.strip()}


def page_count(full):
    try:
        out = subprocess.run(["pdfinfo", full], capture_output=True, text=True,
                             timeout=120).stdout
    except (OSError, subprocess.TimeoutExpired):
        return 0
    for line in out.splitlines():
        if line.startswith("Pages:"):
            return int(line.split()[1])
    return 0


def ocr_page(full, page, lang, dpi, tmp):
    base = os.path.join(tmp, f"p{page}")
    try:
        subprocess.run(["pdftoppm", "-f", str(page), "-l", str(page), "-r", str(dpi),
                        "-gray", "-png", "-singlefile", full, base],
                       capture_output=True, timeout=PAGE_TIMEOUT, check=True)
        proc = subprocess.run(["tesseract", base + ".png", "stdout", "-l", lang],
                              capture_output=True, timeout=PAGE_TIMEOUT,
                              env={**os.environ, "OMP_THREAD_LIMIT": "1"})
    except (OSError, subprocess.SubprocessError):
        return None
    finally:
        try:
            os.unlink(base + ".png")
        except OSError:
            pass
    return textstore.tidy_page(proc.stdout.decode("utf-8", errors="replace"), dehyphen=True)


class Worker:
    def __init__(self, args, langs):
        self.args = args
        self.langs = langs
        self.lock = threading.Lock()      # one claim at a time
        self.stop = False
        self.done = 0

    def allowed_jobs(self):
        hour = time.localtime().tm_hour
        return self.args.night_jobs if hour < 7 else self.args.day_jobs

    def wait_turn(self, slot):
        """Block while this worker slot is not allowed to run, or the box is busy."""
        while not self.stop:
            if slot >= self.allowed_jobs():
                time.sleep(60)
                continue
            if os.getloadavg()[0] > self.args.max_load:
                time.sleep(30)
                continue
            return True
        return False

    def claim(self, db):
        """Next queued document whose language is installed, marked running."""
        with self.lock:
            row = next((r for r in db.execute(
                "SELECT path, lang, pages_done FROM ocr_queue WHERE state = 'queued'"
                " ORDER BY priority, added, path LIMIT 1000")
                if set(r[1].split("+")) <= self.langs), None)
            if not row:
                return None
            with db:
                db.execute("UPDATE ocr_queue SET state = 'running', started = ?"
                           " WHERE path = ?", (int(time.time()), row[0]))
            return row

    def run(self, slot):
        db = textstore.connect()
        tmp = tempfile.mkdtemp(prefix="ocr-")
        try:
            while self.wait_turn(slot):
                if self.args.limit and self.done >= self.args.limit:
                    return
                row = self.claim(db)
                if not row:
                    if not self.args.daemon:
                        return
                    time.sleep(300)
                    continue
                self.document(db, tmp, slot, *row)
                self.done += 1
        finally:
            shutil.rmtree(tmp, ignore_errors=True)

    def document(self, db, tmp, slot, path, lang, pages_done):
        full = os.path.join(DOCROOT, path)
        started = time.time()
        try:
            st = os.stat(full)
        except OSError:
            with db:
                db.execute("DELETE FROM ocr_queue WHERE path = ?", (path,))
            log(f"[{slot}] {path}: gone, dropped from the queue")
            return
        ia = textstore.read_ia_text(path)
        if ia:
            # archive.org OCR arrived after the document was queued
            textstore.store_doc(db, path, st.st_size, int(st.st_mtime), "done", "archive.org",
                                [(i, textstore.tidy_page(t, dehyphen=True))
                                 for i, t in enumerate(ia, 1)])
            with db:
                db.execute("DELETE FROM ocr_queue WHERE path = ?", (path,))
            log(f"[{slot}] {path}: archive.org text taken, no OCR needed")
            return
        total = page_count(full)
        if not total:
            with db:
                db.execute("UPDATE ocr_queue SET state = 'error', error = 'no pages',"
                           " finished = ? WHERE path = ?", (int(time.time()), path))
            log(f"[{slot}] {path}: no pages")
            return

        with db:
            db.execute("UPDATE ocr_queue SET pages_total = ? WHERE path = ?", (total, path))
            if pages_done == 0:
                # first page of a fresh run: the thin or legacy text goes, the
                # new one comes in page by page
                db.execute("DELETE FROM pages WHERE path = ?", (path,))

        chars = 0
        for page in range(pages_done + 1, total + 1):
            if not self.wait_turn(slot):
                with db:
                    db.execute("UPDATE ocr_queue SET state = 'queued' WHERE path = ?", (path,))
                return
            text = ocr_page(full, page, lang, self.args.dpi, tmp)
            with db:
                if text:
                    db.execute("INSERT OR REPLACE INTO pages (path, page, body) VALUES (?,?,?)",
                               (path, page, text))
                    chars += len(text)
                db.execute("UPDATE ocr_queue SET pages_done = ? WHERE path = ?", (page, path))

        n, total_chars = db.execute(
            "SELECT COUNT(*), COALESCE(SUM(LENGTH(body)), 0) FROM pages WHERE path = ?",
            (path,)).fetchone()
        state = "done" if total_chars >= textstore.MIN_USEFUL else "none"
        with db:
            db.execute(
                "INSERT INTO docs (path, size, mtime, state, tool, n_pages, chars, stamp)"
                " VALUES (?,?,?,?,?,?,?,?)"
                " ON CONFLICT(path) DO UPDATE SET size=excluded.size, mtime=excluded.mtime,"
                " state=excluded.state, tool=excluded.tool, n_pages=excluded.n_pages,"
                " chars=excluded.chars, stamp=excluded.stamp",
                (path, st.st_size, int(st.st_mtime), state, "tesseract", n, total_chars,
                 int(time.time())))
            db.execute("UPDATE ocr_queue SET state = 'done', finished = ? WHERE path = ?",
                       (int(time.time()), path))
        took = time.time() - started
        log(f"[{slot}] {path}: {total} pages, {total_chars} chars, {lang}, "
            f"{took / max(1, total - pages_done):.1f} s/page")


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--daemon", action="store_true")
    ap.add_argument("--store", default=textstore.PAGESDB)
    ap.add_argument("--dpi", type=int, default=200)
    ap.add_argument("--day-jobs", type=int, default=3)
    ap.add_argument("--night-jobs", type=int, default=6)
    ap.add_argument("--jobs", type=int, default=0, help="fixed number of workers")
    ap.add_argument("--max-load", type=float, default=6.0)
    ap.add_argument("--limit", type=int, default=0, help="documents per worker")
    args = ap.parse_args()
    textstore.PAGESDB = args.store
    if args.jobs:
        args.day_jobs = args.night_jobs = args.jobs

    langs = available_langs()
    if not langs:
        sys.exit("tesseract not found")

    db = textstore.connect()
    with db:
        # whatever was running when the last run ended continues where it stopped
        db.execute("UPDATE ocr_queue SET state = 'queued' WHERE state = 'running'")
    missing = [r[0] for r in db.execute(
        "SELECT DISTINCT lang FROM ocr_queue WHERE state = 'queued'")
        if not set(r[0].split("+")) <= langs]
    if missing:
        log(f"no tesseract language data for {', '.join(missing)};"
            " those documents wait (apt install tesseract-ocr-<lang>)")
    db.close()

    worker = Worker(args, langs)
    log(f"OCR started: {args.day_jobs} workers by day, {args.night_jobs} at night, "
        f"{args.dpi} dpi, languages {', '.join(sorted(langs))}")
    slots = max(args.day_jobs, args.night_jobs)
    threads = [threading.Thread(target=worker.run, args=(i,), daemon=True)
               for i in range(slots)]
    for t in threads:
        t.start()
    try:
        for t in threads:
            t.join()
    except KeyboardInterrupt:
        worker.stop = True
    log("OCR stopped")
    return 0


if __name__ == "__main__":
    sys.exit(main())
