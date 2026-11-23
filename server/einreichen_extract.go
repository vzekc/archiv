package archiv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Processing of an uploaded file: checksum, duplicates, what it is, an
// excerpt of its content and preview images. Runs once, right after upload.

var (
	pdfExts     = map[string]bool{".pdf": true}
	imageExts   = map[string]bool{".jpg": true, ".jpeg": true, ".png": true, ".gif": true, ".tif": true, ".tiff": true, ".bmp": true, ".webp": true}
	archiveExts = map[string]bool{".zip": true, ".7z": true, ".rar": true, ".lzh": true, ".lha": true, ".arj": true, ".cab": true,
		".tar": true, ".tgz": true, ".tbz": true, ".tbz2": true, ".txz": true, ".gz": true, ".bz2": true, ".xz": true}
	isoExts  = map[string]bool{".iso": true, ".bin": false, ".cue": false, ".nrg": true, ".mdf": true}
	diskExts = map[string]bool{".td0": true, ".imd": true, ".dsk": true, ".d64": true, ".d71": true, ".d81": true, ".g64": true,
		".adf": true, ".ima": true, ".img": true, ".hfe": true, ".scp": true, ".dmk": true, ".jv1": true, ".jv3": true,
		".st": true, ".msa": true, ".atr": true, ".woz": true, ".nib": true, ".do": true, ".po": true, ".86f": true, ".mfm": true, ".dcp": true}
	textExts = map[string]bool{".txt": true, ".doc": true, ".asm": true, ".mac": true, ".z80": true, ".bas": true, ".me": true,
		".1st": true, ".nfo": true, ".md": true, ".c": true, ".h": true, ".pas": true, ".for": true, ".lst": true, ".prn": true,
		".bbs": true, ".diz": true, ".csv": true, ".s": true, ".inc": true, ".lib": true, ".sub": true, ".hlp": true}
	scanExts = map[string]bool{".exe": true, ".com": true, ".dll": true, ".scr": true, ".bat": true, ".cmd": true, ".vbs": true,
		".js": true, ".docx": true, ".docm": true, ".xls": true, ".xlsx": true, ".xlsm": true, ".ppt": true, ".pptx": true,
		".pptm": true, ".msi": true, ".jar": true, ".ps1": true}
)

func lowerExt(name string) string {
	n := strings.ToLower(name)
	for _, double := range []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.z"} {
		if strings.HasSuffix(n, double) {
			return ".tar"
		}
	}
	return filepath.Ext(n)
}

func kindOf(name string) string {
	e := lowerExt(name)
	switch {
	case pdfExts[e]:
		return "pdf"
	case imageExts[e]:
		return "image"
	case archiveExts[e]:
		return "archive"
	case isoExts[e]:
		return "iso"
	case diskExts[e]:
		return "disk"
	case textExts[e]:
		return "text"
	}
	return "other"
}

func run(ctx context.Context, timeout time.Duration, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), fmt.Errorf("%s: %v %s", name, err, strings.TrimSpace(limitBytes(errb.String(), 300)))
	}
	return out.Bytes(), nil
}

func limitBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReaderSize(f, 1<<20)); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// decodeText reads old text files: UTF-8 if it is valid, else cp1252/latin-1;
// WordStar's high bit on the last letter of words is stripped.
func decodeText(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	high := 0
	for _, c := range b {
		if c >= 0x80 {
			high++
		}
	}
	var sb strings.Builder
	wordstar := high > len(b)/20
	for _, c := range b {
		if wordstar {
			c &= 0x7f
		}
		if c == 0x1a { // CP/M end of file
			break
		}
		if c < 0x20 && c != '\n' && c != '\t' && c != '\r' {
			continue
		}
		sb.WriteRune(rune(c)) // latin-1 is close enough for an excerpt
	}
	return sb.String()
}

func (a *App) processFile(ctx context.Context, f *File) error {
	full := filepath.Join(a.cfg.StagingDir, f.Path)
	dir := filepath.Dir(full)
	st, err := os.Stat(full)
	if err != nil {
		return err
	}
	sum, err := fileSHA256(full)
	if err != nil {
		return err
	}
	kind := kindOf(f.Name)
	info := FileInfo{}
	if out, err := run(ctx, time.Minute, "file", "-b", full); err == nil {
		info.Format = limitBytes(strings.TrimSpace(string(out)), 200)
	}

	switch kind {
	case "pdf":
		if out, err := run(ctx, time.Minute, "pdfinfo", full); err == nil {
			for _, l := range strings.Split(string(out), "\n") {
				if v, ok := strings.CutPrefix(l, "Pages:"); ok {
					info.Pages, _ = strconv.Atoi(strings.TrimSpace(v))
				}
			}
		}
		if out, err := run(ctx, 3*time.Minute, "pdftotext", "-f", "1", "-l", "8", "-enc", "UTF-8", full, "-"); err == nil {
			info.Text = cleanExcerpt(string(out), 8000)
		}
		_, err := run(ctx, 3*time.Minute, "pdftoppm", "-f", "1", "-l", "1", "-singlefile", "-jpeg",
			"-jpegopt", "quality=80", "-scale-to", "1200", full, filepath.Join(dir, "preview"))
		info.Preview = err == nil
	case "image":
		_, err := run(ctx, 3*time.Minute, "convert", full+"[0]", "-auto-orient", "-thumbnail", "1200x1200>",
			"-quality", "82", filepath.Join(dir, "preview.jpg"))
		info.Preview = err == nil
	case "archive", "iso":
		a.listArchive(ctx, full, &info)
	case "text":
		if fh, err := os.Open(full); err == nil {
			b := make([]byte, 16000)
			n, _ := io.ReadFull(fh, b)
			fh.Close()
			info.Text = cleanExcerpt(decodeText(b[:n]), 8000)
		}
	}
	if info.Preview {
		_, err := run(ctx, time.Minute, "convert", filepath.Join(dir, "preview.jpg"), "-thumbnail", "320x320",
			"-quality", "82", filepath.Join(dir, "thumb.jpg"))
		info.Thumb = err == nil
	}
	if scanExts[lowerExt(f.Name)] || kind == "archive" {
		info.Virus = a.virusScan(ctx, full)
	}

	dups := a.findDuplicates(f, st.Size(), sum)
	unpack := kind == "archive"
	_, err = a.db.Exec(`UPDATE files SET sha256 = ?, kind = ?, info = ?, duplicates = ?, unpack = ?,
	    status = ?, error = '', suggest_due = ? WHERE id = ?`,
		sum, kind, mustJSON(info), mustJSON(dups), unpack, FReady, now()+suggestSettle, f.ID)
	return err
}

func cleanExcerpt(s string, n int) string {
	var lines []string
	blank := 0
	for _, l := range strings.Split(strings.ReplaceAll(s, "\r", ""), "\n") {
		l = strings.TrimRight(strings.ReplaceAll(l, "\f", ""), " \t")
		if l == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		lines = append(lines, l)
	}
	return limitBytes(strings.TrimSpace(strings.Join(lines, "\n")), n)
}

// listArchive reads the table of contents of an archive or disc image.
func (a *App) listArchive(ctx context.Context, full string, info *FileInfo) {
	n := strings.ToLower(full)
	var names []string
	var total int64
	sizeKnown := true
	if strings.Contains(n, ".tar") || strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".tbz") ||
		strings.HasSuffix(n, ".tbz2") || strings.HasSuffix(n, ".txz") {
		out, err := run(ctx, 5*time.Minute, "tar", "-tvf", full)
		if err != nil {
			return
		}
		for _, l := range strings.Split(string(out), "\n") {
			fs := strings.Fields(l)
			if len(fs) < 6 {
				continue
			}
			sz, _ := strconv.ParseInt(fs[2], 10, 64)
			total += sz
			names = append(names, strings.Join(fs[5:], " "))
		}
	} else {
		out, err := run(ctx, 5*time.Minute, "7zz", "l", "-slt", "-ba", "-p", full)
		if err != nil && len(out) == 0 {
			return
		}
		names, total, sizeKnown = parse7zListing(string(out))
	}
	info.ListingTotal = len(names)
	info.Unpacked = total
	if !sizeKnown {
		info.Unpacked = 0
	}
	root := ""
	for i, nm := range names {
		top, _, _ := strings.Cut(strings.TrimPrefix(path.Clean(nm), "/"), "/")
		if !strings.Contains(strings.TrimPrefix(path.Clean(nm), "/"), "/") {
			root = ""
			break
		}
		if i == 0 {
			root = top
		} else if top != root {
			root = ""
			break
		}
	}
	info.SingleRoot = root
	if len(names) > 400 {
		names = names[:400]
	}
	info.Listing = names
}

// parse7zListing reads the output of "7zz l -slt -ba": one block of
// "Key = value" lines per entry, blocks separated by blank lines. It returns
// the files, their total size, and whether every file had a size; a bzip2
// stream lists a single entry with a blank size.
func parse7zListing(out string) (names []string, total int64, sizeKnown bool) {
	sizeKnown = true
	var cur, size string
	var isDir, inEntry bool
	flush := func() {
		if inEntry && !isDir {
			if cur != "" {
				names = append(names, cur)
			}
			sz, err := strconv.ParseInt(size, 10, 64)
			if err != nil {
				sizeKnown = false
			}
			total += sz
		}
		cur, size, isDir, inEntry = "", "", false, false
	}
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimRight(l, "\r")
		switch {
		case l == "":
			flush()
		case strings.HasPrefix(l, "Path = "):
			cur = strings.ReplaceAll(strings.TrimPrefix(l, "Path = "), "\\", "/")
			inEntry = true
		case strings.HasPrefix(l, "Folder = +"), strings.HasPrefix(l, "Attributes = D"):
			isDir = true
		case strings.HasPrefix(l, "Size = "):
			size = strings.TrimPrefix(l, "Size = ")
			inEntry = true
		}
	}
	flush()
	return names, total, sizeKnown
}

// virusScan uses ClamAV's daemon if it is installed; empty means clean or
// not scanned.
func (a *App) virusScan(ctx context.Context, full string) string {
	if _, err := exec.LookPath("clamdscan"); err != nil {
		return ""
	}
	out, err := run(ctx, 10*time.Minute, "clamdscan", "--no-summary", "--fdpass", full)
	if err == nil {
		return ""
	}
	for _, l := range strings.Split(string(out), "\n") {
		if strings.HasSuffix(l, " FOUND") {
			_, v, _ := strings.Cut(l, ": ")
			return strings.TrimSuffix(v, " FOUND")
		}
	}
	return ""
}

// findDuplicates compares the checksum with archive files of the same size
// (hashes cached) and with other uploads.
func (a *App) findDuplicates(f *File, size int64, sum string) []Duplicate {
	var out []Duplicate
	// the catalog knows the hash of every file in the archive
	if a.lastSync() > 0 {
		rows, err := a.cat.Query("SELECT path FROM file_paths WHERE sha256 = ? AND seen >= ? ORDER BY path LIMIT 20", sum, a.lastSync())
		if err == nil {
			for rows.Next() {
				var p string
				rows.Scan(&p)
				out = append(out, Duplicate{Where: "archive", Path: p})
			}
			rows.Close()
		}
	} else if size >= 1024 {
		cands := a.arch.SameSize(size, 25)
		if len(cands) <= 20 {
			for _, p := range cands {
				full := filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(p))
				st, err := os.Stat(full)
				if err != nil {
					continue
				}
				var h string
				err = a.db.QueryRow("SELECT sha256 FROM archive_hashes WHERE path = ? AND size = ? AND mtime = ?",
					p, st.Size(), st.ModTime().Unix()).Scan(&h)
				if err != nil {
					if h, err = fileSHA256(full); err != nil {
						continue
					}
					a.db.Exec("INSERT OR REPLACE INTO archive_hashes (path, size, mtime, sha256) VALUES (?,?,?,?)",
						p, st.Size(), st.ModTime().Unix(), h)
				}
				if h == sum {
					out = append(out, Duplicate{Where: "archive", Path: p})
				}
			}
		}
	}
	rows, err := a.db.Query(`SELECT f.submission, f.name, f.archive_path FROM files f
	    WHERE f.sha256 = ? AND f.id <> ? AND f.submission <> ? AND f.status <> ?
	      AND f.submission NOT IN (SELECT id FROM submissions WHERE state = ?)`, sum, f.ID, f.Submission, FSkipped, StRejected)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var sub int64
			var name, ap string
			rows.Scan(&sub, &name, &ap)
			if ap != "" {
				known := false
				for _, d := range out {
					known = known || d.Path == ap
				}
				if !known {
					out = append(out, Duplicate{Where: "archive", Path: ap})
				}
			} else {
				out = append(out, Duplicate{Where: "pending", Path: name, Submission: sub})
			}
		}
	}
	return out
}
