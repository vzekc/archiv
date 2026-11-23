package archiv

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// intake takes the accepted files of a submission into the catalog
// (doIntakeCatalog) and tells the extensions how it went.
func (a *App) intake(ctx context.Context, s *Submission) {
	err := a.doIntakeCatalog(ctx, s)
	if err != nil {
		logf("intake %d: %v", s.ID, err)
		a.db.Exec("UPDATE submissions SET state = ?, decision_note = ?, updated = ? WHERE id = ?",
			StFailed, err.Error(), now(), s.ID)
		a.db.logEvent("system", s.ID, "intake failed", err.Error())
		a.notifyEvent(EvFailed, s.ID, "", err.Error())
		return
	}
	a.db.Exec("UPDATE submissions SET state = ?, updated = ? WHERE id = ?", StDone, now(), s.ID)
	a.db.logEvent("system", s.ID, "taken in", nil)
	a.notifyEvent(EvDone, s.ID, "", "")
	a.purgeTaken(s.ID)
}

// freeName returns name, or name with a number added if it is taken.
func freeName(dir, name string) string {
	if _, err := os.Lstat(filepath.Join(dir, name)); os.IsNotExist(err) {
		return name
	}
	ext := filepath.Ext(name)
	base := strings.TrimSuffix(name, ext)
	for i := 2; ; i++ {
		n := fmt.Sprintf("%s_%d%s", base, i, ext)
		if _, err := os.Lstat(filepath.Join(dir, n)); os.IsNotExist(err) {
			return n
		}
	}
}

// copyFile copies through a temporary name, so the archive never shows a
// half-copied file.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := filepath.Join(filepath.Dir(dst), "."+filepath.Base(dst)+".part")
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o664)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(target, 0o2775)
		case d.Type().IsRegular():
			return copyFile(p, target)
		}
		return nil // links, devices: never into the archive
	})
}

// unpackLimit bounds what unpacking an archive may write: twice the size its
// listing names, and 16 GB when the listing names none (Unpacked is 0, as for
// a bzip2 stream).
func unpackLimit(info FileInfo) int64 {
	if info.Unpacked <= 0 {
		return 16 << 30
	}
	return max(info.Unpacked*2, 64<<20)
}

// unpack extracts an archive into dir and makes sure nothing escaped, no
// links remain and the size stays plausible.
func unpack(ctx context.Context, src, dir string, limit int64) error {
	if err := os.MkdirAll(dir, 0o2770); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Hour)
	defer cancel()
	var cmd *exec.Cmd
	n := strings.ToLower(src)
	if strings.Contains(n, ".tar") || strings.HasSuffix(n, ".tgz") || strings.HasSuffix(n, ".tbz") ||
		strings.HasSuffix(n, ".tbz2") || strings.HasSuffix(n, ".txz") {
		cmd = exec.CommandContext(ctx, "tar", "-xf", src, "-C", dir, "--no-same-owner", "--no-same-permissions")
	} else {
		cmd = exec.CommandContext(ctx, "7zz", "x", "-y", "-p", "-snl-", "-o"+dir, src)
	}
	var errb bytes.Buffer
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%v %s", err, limitBytes(strings.TrimSpace(errb.String()), 300))
	}
	var total int64
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 || !(d.IsDir() || d.Type().IsRegular()) {
			return os.Remove(p)
		}
		if strings.HasPrefix(d.Name(), ".") && p != dir {
			// hidden files (e.g. __MACOSX, .DS_Store) are noise in the archive
			if d.IsDir() {
				os.RemoveAll(p)
				return filepath.SkipDir
			}
			return os.Remove(p)
		}
		if d.Name() == "__MACOSX" && d.IsDir() {
			os.RemoveAll(p)
			return filepath.SkipDir
		}
		if info, err := d.Info(); err == nil && d.Type().IsRegular() {
			total += info.Size()
			if total > limit {
				return fmt.Errorf("entpackt größer als erwartet (%s)", humanSize(limit))
			}
		}
		return nil
	})
}

// refreshArchive updates the archive's index, full text, previews and pages,
// holding the same lock as the nightly run.
func (a *App) refreshArchive(ctx context.Context, touched map[string]bool) error {
	lock, err := os.OpenFile(a.cfg.ReindexLock, os.O_CREATE|os.O_RDWR, 0o664)
	if err != nil {
		return err
	}
	defer lock.Close()
	deadline := time.Now().Add(3 * time.Hour)
	for {
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			break
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return fmt.Errorf("der nächtliche Lauf blockiert den Index")
		}
		time.Sleep(30 * time.Second)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	// the catalog first: the new files become items, and the text and
	// thumbnail jobs below find their hashes
	s := &Syncer{cfg: a.cfg, c: a.cat, root: a.cfg.ArchiveRoot, jobs: 2, quiet: true}
	if err := s.run(ctx); err != nil {
		logf("refresh: catalog sync: %v", err)
	}

	var dirs []string
	for d := range touched {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	bin := a.cfg.BinDir
	steps := [][]string{{filepath.Join(bin, "index-collection.py"), "--quiet"}}
	for _, d := range dirs {
		steps = append(steps, []string{filepath.Join(bin, "extract-text.py"), "--only", d + "/"})
	}
	layout := "path"
	if a.cfg.ThumbsByHash {
		layout = "hash"
	}
	steps = append(steps, []string{filepath.Join(bin, "make-thumbs.py"), "--layout", layout})
	// the listings of the old site, while it runs
	for _, d := range dirs {
		if a.cfg.ThumbsByHash {
			break
		}
		parent := path.Dir(d)
		if parent == "." {
			parent = d
		}
		steps = append(steps, []string{filepath.Join(bin, "generate-pages.py"), "--only", parent})
	}
	for _, st := range steps {
		out, err := run(ctx, 2*time.Hour, st[0], st[1:]...)
		if err != nil {
			// the files are in the archive; a failed refresh is caught up nightly
			logf("refresh %v: %v", st, err)
			continue
		}
		sc := bufio.NewScanner(bytes.NewReader(out))
		for sc.Scan() {
		}
	}
	return nil
}

// purgeTaken frees the staging space of a submission once it is in the
// archive (the originals of unpacked archives are kept in the backup dir).
func (a *App) purgeTaken(sub int64) {
	os.RemoveAll(filepath.Join(a.cfg.StagingDir, "files", strconv.FormatInt(sub, 10)))
}
