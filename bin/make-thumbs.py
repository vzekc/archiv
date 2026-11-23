#!/usr/bin/env python3
"""
Build thumbnails for the images and PDFs of the collection, so the generated
directory pages can show a contact sheet instead of a list of file names. A
PDF gets a picture of its first page -- for the magazines, that is the cover.
PDFs also get a large version (html/previews/<path>.jpg, 1200 px) that the
pages show on hover, big enough to read; the small one is scaled down from it,
so every PDF is rendered only once.

Thumbnails go to html/thumbs/<same path>.jpg and are rebuilt only when the
original is newer. A file whose bytes already have pictures under another
path (it was moved, or it is a duplicate) gets those: moved along, or
copied; the hashes come from the catalog ("archiv sync"). Very small images are skipped: they are spacers, buttons
and rules from the mirrored websites, not photographs. Files that could not be
read are remembered in data/thumbs-failed.tsv and only tried again once they
change.

generate-pages.py picks the thumbnails up automatically -- it simply looks
whether a matching file exists under thumbs/.

Usage:  make-thumbs.py [--jobs N] [--size 240] [--pdf-size 320] [--min-bytes 8192]
                      [--limit N]
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
CAT = None              # textstore.Catalog, set in main
THUMBDIR = os.path.join(DOCROOT, "thumbs")
PREVIEWDIR = os.path.join(DOCROOT, "previews")
FAILED = os.path.join(textstore.HOME, "data", "thumbs-failed.tsv")

IMAGE_EXTS = ("jpg", "jpeg", "png", "gif", "bmp", "tif", "tiff")
PDF_EXTS = ("pdf",)


LAYOUT = "path"         # "hash" once the old site is gone: <dir>/<sha[0:2]>/<sha>.jpg


def by_hash(base, relpath):
    sha = CAT.sha(relpath)
    if not sha:
        return os.path.join(base, "unhashed", relpath + ".jpg")
    return os.path.join(base, sha[:2], sha + ".jpg")


def thumb_path(relpath):
    if LAYOUT == "hash":
        return by_hash(THUMBDIR, relpath)
    return os.path.join(THUMBDIR, relpath + ".jpg")


def preview_path(relpath):
    if LAYOUT == "hash":
        return by_hash(PREVIEWDIR, relpath)
    return os.path.join(PREVIEWDIR, relpath + ".jpg")


def is_current(dst, mtime):
    try:
        return os.stat(dst).st_mtime >= mtime
    except OSError:
        return False


def run(cmd, tmp, dst):
    """Run cmd, which writes tmp; move tmp to dst. True on success."""
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    try:
        proc = subprocess.run(cmd, capture_output=True, timeout=120)
    except (subprocess.TimeoutExpired, OSError):
        proc = None
    if proc is None or proc.returncode != 0 or not os.path.exists(tmp):
        if os.path.exists(tmp):
            os.unlink(tmp)
        return False
    os.replace(tmp, dst)
    return True


def scale(src, tmp, size):
    return ["convert", f"{src}[0]", "-auto-orient",
            "-thumbnail", f"{size}x{size}", "-quality", "82", tmp]


def build_pdf(relpath, size, big, failed):
    """Large preview from the PDF, then the thumbnail from the preview."""
    src = os.path.join(DOCROOT, relpath)
    dst, pv = thumb_path(relpath), preview_path(relpath)
    try:
        mtime = os.stat(src).st_mtime
    except OSError:
        return "error", 0
    if failed.get(relpath) == int(mtime):
        return "skipped", mtime
    if is_current(dst, mtime) and is_current(pv, mtime):
        return "current", mtime
    if LAYOUT == "path" and (textstore.take_over_image(CAT, relpath, mtime, preview_path)
                             and textstore.take_over_image(CAT, relpath, mtime, thumb_path)):
        return "taken", mtime
    if not is_current(pv, mtime):
        tmp = pv + ".tmp.jpg"           # pdftoppm appends ".jpg" itself
        if not run(["pdftoppm", "-f", "1", "-l", "1", "-singlefile", "-jpeg",
                    "-jpegopt", "quality=80", "-scale-to", str(big), src, tmp[:-4]],
                   tmp, pv):
            return "error", mtime
    if not is_current(dst, mtime):
        if not run(scale(pv, dst + ".tmp.jpg", size), dst + ".tmp.jpg", dst):
            return "error", mtime
    return "built", mtime


def build_one(relpath, size, failed):
    src = os.path.join(DOCROOT, relpath)
    dst = thumb_path(relpath)
    try:
        st_src = os.stat(src)
    except OSError:
        return "error", 0
    if failed.get(relpath) == int(st_src.st_mtime):
        return "skipped", st_src.st_mtime
    if os.path.exists(dst):
        try:
            if os.stat(dst).st_mtime >= st_src.st_mtime:
                return "current", st_src.st_mtime
        except OSError:
            pass

    if LAYOUT == "path" and textstore.take_over_image(CAT, relpath, st_src.st_mtime, thumb_path):
        return "taken", st_src.st_mtime
    tmp = dst + ".tmp.jpg"
    if not run(scale(src, tmp, size), tmp, dst):
        return "error", st_src.st_mtime
    return "built", st_src.st_mtime


def read_failed(path):
    failed = {}
    try:
        with open(path, encoding="utf-8") as fh:
            for line in fh:
                rel, _, mtime = line.rstrip("\n").rpartition("\t")
                if rel:
                    failed[rel] = int(mtime)
    except (OSError, ValueError):
        pass
    return failed


def write_failed(path, failed):
    tmp = path + ".tmp"
    with open(tmp, "w", encoding="utf-8") as fh:
        for rel in sorted(failed):
            fh.write(f"{rel}\t{failed[rel]}\n")
    os.replace(tmp, path)


def main():
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--db", default=DBPATH)
    ap.add_argument("--size", type=int, default=240)
    ap.add_argument("--pdf-size", type=int, default=320,
                    help="covers are looked at more closely than photos")
    ap.add_argument("--preview-size", type=int, default=1200,
                    help="large PDF page shown on hover, big enough to read")
    ap.add_argument("--failed", default=FAILED)
    ap.add_argument("--min-bytes", type=int, default=8192)
    ap.add_argument("--jobs", type=int, default=4)
    ap.add_argument("--limit", type=int, default=0)
    ap.add_argument("--catalog", default=textstore.CATALOG)
    ap.add_argument("--layout", choices=("path", "hash"), default="path",
                    help="file pictures by the path of the original, or by its hash")
    args = ap.parse_args()

    global CAT, LAYOUT
    CAT = textstore.Catalog(args.catalog)
    LAYOUT = args.layout
    db = sqlite3.connect(args.db, timeout=60)
    placeholders = ",".join("?" * len(IMAGE_EXTS))
    rows = db.execute(
        f"SELECT path FROM files WHERE (ext IN ({placeholders}) AND size >= ?)"
        f" OR ext IN ({','.join('?' * len(PDF_EXTS))})"
        " ORDER BY path", (*IMAGE_EXTS, args.min_bytes, *PDF_EXTS)).fetchall()
    paths = [r[0] for r in rows]
    if args.limit:
        paths = paths[:args.limit]

    if not paths:
        print("no images to thumbnail")
        return 0

    failed = read_failed(args.failed)
    print(f"thumbnails for {len(paths)} images and PDFs, {args.jobs} workers", flush=True)
    started = time.time()
    counts = {"built": 0, "current": 0, "taken": 0, "error": 0, "skipped": 0}

    def one(p):
        if p.lower().endswith(".pdf"):
            return build_pdf(p, args.pdf_size, args.preview_size, failed)
        return build_one(p, args.size, failed)

    now_failed = {}
    with ThreadPoolExecutor(max_workers=args.jobs) as pool:
        for n, (p, (res, mtime)) in enumerate(zip(paths, pool.map(one, paths)), 1):
            counts[res] = counts.get(res, 0) + 1
            if res in ("error", "skipped") and mtime:
                now_failed[p] = int(mtime)
            if n % 500 == 0:
                print(f"  {n}/{len(paths)}  {counts['built']} neu, "
                      f"{counts['current']} aktuell, {counts['error']} Fehler",
                      flush=True)

    if not args.limit:
        write_failed(args.failed, now_failed)
    took = time.time() - started
    print(f"thumbnails: {counts['built']} built, {counts['current']} already current, "
          f"{counts['taken']} taken over from moved or duplicate files, "
          f"{counts['error']} failed, {counts['skipped']} known broken, in {took:.0f}s")
    return 0


if __name__ == "__main__":
    sys.exit(main())
