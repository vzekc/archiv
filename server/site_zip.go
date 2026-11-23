package archiv

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// A folder of the archive as a ZIP file, written straight to the client
// while the files are read from the share: nothing is buffered on disk, and
// only the central directory is kept in memory. Formats that are compressed
// already are stored, everything else deflated; archive/zip writes data
// descriptors and ZIP64 where needed.

const (
	zipMaxTotal = 8 // downloads at once, all members together
	zipMaxUser  = 2 // downloads at once per member
)

var storedExts = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`pdf jpg jpeg png gif webp heic zip arc ark lzh lha arj zoo pak rar 7z gz tgz
	    bz2 xz zst cab jar docx xlsx pptx odt mp3 mp4 m4a m4v mov avi mkv mpg mpeg wmv webm flac ogg opus djvu`) {
		storedExts[e] = true
	}
}

// Filepage scaffolding; "descriptions" stays, it is the only description
// that travels with the files.
var zipSkip = map[string]bool{"banner": true, "adbanner": true, "adfooter": true, "filepage.cnf": true,
	"filepage.fch": true, "exclude.cnf": true, "extension.cnf": true, "index.html.tmp": true}

type zipSlots struct {
	mu     sync.Mutex
	total  int
	byUser map[string]int
}

var zips = &zipSlots{byUser: map[string]int{}}

// take reserves a download for user; the message says why not.
func (z *zipSlots) take(user string) (release func(), status int, msg string) {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.byUser[user] >= zipMaxUser {
		return nil, http.StatusTooManyRequests, fmt.Sprintf("Es laufen bereits %d ZIP-Downloads von dir. Bitte warte, bis einer davon fertig ist.", zipMaxUser)
	}
	if z.total >= zipMaxTotal {
		return nil, http.StatusServiceUnavailable, fmt.Sprintf("Im Moment laufen schon %d ZIP-Downloads. Bitte versuche es in ein paar Minuten noch einmal.", zipMaxTotal)
	}
	z.total++
	z.byUser[user]++
	return func() {
		z.mu.Lock()
		z.total--
		if z.byUser[user]--; z.byUser[user] <= 0 {
			delete(z.byUser, user)
		}
		z.mu.Unlock()
	}, 0, ""
}

func (a *App) zipFail(w http.ResponseWriter, r *http.Request, status int, title, text, rel string) {
	if status == http.StatusServiceUnavailable {
		w.Header().Set("Retry-After", "60")
	}
	back := "/"
	if rel != "" {
		back = href(rel) + "/"
	}
	a.renderSite(w, r, status, "site-error", &SitePage{Title: title, Data: text, Query: back})
}

func (a *App) zipDir(w http.ResponseWriter, r *http.Request) {
	rel, ok := cleanRel(strings.ReplaceAll(r.URL.Query().Get("p"), `\`, "/"))
	full := filepath.Join(a.cfg.ArchiveRoot, filepath.FromSlash(rel))
	st, err := os.Stat(full)
	if !ok || err != nil || !st.IsDir() || (rel != "" && skipTopDirs[strings.Split(rel, "/")[0]]) {
		a.zipFail(w, r, http.StatusNotFound, "Nicht gefunden", "Dieses Verzeichnis gibt es nicht.", "")
		return
	}
	n, size := a.treeSize(rel)
	if n == 0 {
		a.zipFail(w, r, http.StatusNotFound, "Nichts herunterzuladen",
			"Dieses Verzeichnis enthält keine Dateien, oder es ist so neu, dass der Katalog es noch nicht kennt (er wird jede Nacht abgeglichen).", rel)
		return
	}
	if size > zipMaxBytes {
		a.zipFail(w, r, http.StatusRequestEntityTooLarge, "Zu groß für einen ZIP-Download",
			fmt.Sprintf("Dieses Verzeichnis enthält %s in %s Dateien. Als ZIP lassen sich höchstens %s auf einmal herunterladen – bitte die Unterverzeichnisse einzeln holen.",
				humanSize(size), num(n), humanSize(zipMaxBytes)), rel)
		return
	}
	user := "anonymous"
	if u := currentUser(r); u != nil {
		user = u.ID
	}
	release, status, msg := zips.take(user)
	if release == nil {
		a.zipFail(w, r, status, "Bitte warten", msg, rel)
		return
	}
	defer release()

	base := "archiv"
	if rel != "" {
		base = path.Base(rel)
	}
	streamZip(w, r, base, func(add func(e zipEntry) bool) {
		walkForZip(full, base, rel, rel == "", add, a.descriptionsFor)
	}, size, func(files int, sent int64, err error) {
		h := sha256.Sum256([]byte(user))
		state := "complete"
		if err != nil {
			state = "aborted: " + err.Error()
		}
		logf("zip: user=%s dir=%q files=%d bytes=%d %s", hex.EncodeToString(h[:8]), rel, files, sent, state)
	})
}

// zipEntry is a folder, a file on disk, or a file made in memory.
type zipEntry struct {
	Name, Full string
	Data       []byte
	Dir        bool
}

// streamZip writes the entries that fill gives it as one ZIP file. limit
// stops a tree that has grown far beyond what the catalog knew.
func streamZip(w http.ResponseWriter, r *http.Request, base string, fill func(add func(e zipEntry) bool), limit int64, done func(int, int64, error)) {
	zipname := base + ".zip"
	ascii := strings.Map(func(c rune) rune {
		if c < 128 && (c == '.' || c == '_' || c == '-' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return c
		}
		return '_'
	}, zipname)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"; filename*=UTF-8''%s`, ascii, url.PathEscape(zipname)))
	w.Header().Set("Cache-Control", "private, no-store")
	cw := &countWriter{w: w}
	zw := zip.NewWriter(cw)
	files := 0
	var failed error
	fill(func(e zipEntry) bool {
		name, full := e.Name, e.Full
		if failed != nil || r.Context().Err() != nil {
			if failed == nil {
				failed = r.Context().Err()
			}
			return false
		}
		if cw.n > limit+limit/10+(64<<20) {
			failed = fmt.Errorf("larger on disk than the catalog says, cut off")
			return false
		}
		if e.Data != nil {
			hw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Modified: time.Now(), Method: zip.Deflate})
			if err == nil {
				_, err = hw.Write(e.Data)
			}
			failed = err
			return err == nil
		}
		if e.Dir {
			st, err := os.Stat(full)
			mt := time.Now()
			if err == nil {
				mt = st.ModTime()
			}
			_, err = zw.CreateHeader(&zip.FileHeader{Name: name + "/", Modified: mt, Method: zip.Store})
			failed = err
			return err == nil
		}
		f, err := os.Open(full)
		if err != nil {
			return true // gone meanwhile: left out
		}
		defer f.Close()
		st, err := f.Stat()
		if err != nil {
			return true
		}
		method := zip.Deflate
		if storedExts[extOf(name)] || st.Size() == 0 {
			method = zip.Store
		}
		hw, err := zw.CreateHeader(&zip.FileHeader{Name: name, Modified: st.ModTime(), Method: method})
		if err != nil {
			failed = err
			return false
		}
		if _, err := io.Copy(hw, f); err != nil {
			failed = err
			return false
		}
		files++
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		return true
	})
	if failed == nil {
		failed = zw.Close()
	}
	done(files, cw.n, failed)
}

type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// walkForZip hands out a folder depth first, folders before their contents,
// without the site's furniture and generated listings. Symlinks are left out.
// Each folder gets a descriptions file written from the catalog.
func walkForZip(full, name, rel string, top bool, add func(zipEntry) bool, descr func(string) []byte) bool {
	if !add(zipEntry{Name: name, Full: full, Dir: true}) {
		return false
	}
	if d := descr(rel); len(d) > 0 {
		if !add(zipEntry{Name: name + "/descriptions", Data: d}) {
			return false
		}
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return true
	}
	sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
	var dirs []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		p := filepath.Join(full, n)
		if e.IsDir() {
			if !(top && skipTopDirs[n]) {
				dirs = append(dirs, n)
			}
			continue
		}
		if !e.Type().IsRegular() || zipSkip[strings.ToLower(n)] || strings.HasSuffix(n, ".part") || strings.EqualFold(n, "descriptions") {
			continue
		}
		if strings.EqualFold(n, "index.html") && isGeneratedPage(p) {
			continue
		}
		if !add(zipEntry{Name: name + "/" + n, Full: p}) {
			return false
		}
	}
	for _, d := range dirs {
		if !walkForZip(filepath.Join(full, d), name+"/"+d, joinRel(rel, d), false, add, descr) {
			return false
		}
	}
	return true
}
