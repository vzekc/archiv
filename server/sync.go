package archiv

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// sync brings the catalog in line with the archive on disk: every file is
// hashed (once; a file whose size and mtime are unchanged is not read
// again), new files become items, files that changed keep their item, files
// that moved keep their item and leave a redirect, files that are gone mark
// their item as missing. Folder titles and descriptions files fill in items
// that nobody has edited yet; the location and rule tags follow the folder.
//
// "import" is the same run; the name is kept for the first one.

// Filepage scaffolding and site furniture, the same lists as
// bin/index-collection.py.
var skipNames = map[string]bool{
	"descriptions": true, "descript.txt": true, "banner": true, "adbanner": true, "adfooter": true, "index.html": true,
	"index.html.bak": true, "index.old": true, "favicon.ico": true, "robots.txt": true, "sitemap.xml": true,
	"filepage.cnf": true, "exclude.cnf": true, "extension.cnf": true, "descriptions.bak": true,
	"flag_de.gif": true, "alldir.txt": true, "search.php": true,
	"filepage.fch": true, "drcript.txt": true, "allfiles.txt": true,
}

// The site's own pages and scripts, at the top and in public/.
var skipSiteFiles = map[string]bool{
	"about.html": true, "mirrors.html": true, "listing.php": true, "zip.php": true, "search.php": true,
	"index-original.html": true, "gif.txt": true, "gifs.txt": true,
	"public/index-original.html": true,
}

// Inside a mirror everything is content except what Filepage put there.
var skipInMirror = map[string]bool{
	"descriptions": true, "descript.txt": true, "banner": true, "adbanner": true, "adfooter": true,
	"filepage.cnf": true, "exclude.cnf": true, "extension.cnf": true, "descriptions.bak": true,
}

// Layout images and the site's own files, at the top level only; the
// mirrors contain folders of the same names that are content.
var skipTopDirs = map[string]bool{"gif": true, "icon": true, "assets": true, "thumbs": true, "previews": true, "seiten": true}

const (
	generatedMarker = "<!-- oc-listing: automatisch erzeugt"
	filepageMarker  = "ANFANG Adbanner fuer filepage"
	landingMarker   = "<!-- oc-landing: erzeugt von bin/build-landing.py"
)

var (
	mirrorDirs = map[string]bool{"mirror": true, "mirrors": true, "saved_mirrors": true}
	hostRe     = regexp.MustCompile(`(?i)^(?:www|ftp)\.[a-z0-9-]+(?:\.[a-z0-9-]+)+$`)
	archiveRe  = regexp.MustCompile(`(?i)\.(?:part\d+\.rar|rar|zip|7z|iso|tgz|tar\.gz|tar)$`)
)

type diskFile struct {
	Path, Dir, Name string
	Size, Mtime     int64
	Sum             string
	Mirror          string // key of the mirror it belongs to
}

type diskDir struct {
	Path, Name string
	Title      string
	Descr      map[string]string // entry name -> description
}

type Syncer struct {
	cfg   *Config
	c     *Catalog
	root  string
	jobs  int
	stamp int64
	quiet bool

	files  []*diskFile
	dirs   map[string]*diskDir
	mirror map[string]*mirrorInfo
}

type mirrorInfo struct {
	Key, Host string
	Roots     []string // folders that are copies
	Files     []string // archives that are copies
	Title     string
	Text      string
}

func syncCmd(args []string) {
	fs := flag.NewFlagSet("sync", flag.ExitOnError)
	root := fs.String("root", "", "archive root (default: archive from the config)")
	catalog := fs.String("catalog", "", "catalog database")
	jobs := fs.Int("jobs", 4, "files hashed in parallel")
	quiet := fs.Bool("quiet", false, "only errors and the summary")
	cfg, err := loadConfig(fs, args)
	if err != nil {
		log.Fatal(err)
	}
	if *root != "" {
		cfg.ArchiveRoot = *root
	}
	if *catalog != "" {
		cfg.CatalogDB = *catalog
	}
	c, err := openCatalog(cfg.CatalogDB)
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s := &Syncer{cfg: cfg, c: c, root: cfg.ArchiveRoot, jobs: max(*jobs, 1), quiet: *quiet}
	if err := s.run(ctx); err != nil {
		log.Fatal(err)
	}
}

func (s *Syncer) logf(format string, args ...any) {
	if !s.quiet {
		log.Printf(format, args...)
	}
}

func (s *Syncer) run(ctx context.Context) error {
	started := time.Now()
	s.stamp = started.Unix()
	if err := s.walk(); err != nil {
		return err
	}
	s.findMirrors()
	s.logf("walk: %d files in %d folders, %d mirrors", len(s.files), len(s.dirs), len(s.mirror))
	if err := s.hashAll(ctx); err != nil {
		return err
	}
	st, err := s.apply()
	if err != nil {
		return err
	}
	s.c.setMeta("last_sync", s.stamp)
	s.c.setMeta("last_sync_root", s.root)
	log.Printf("sync: %d files, %d new items, %d changed, %d moved, %d missing, %d items in all (%s)",
		len(s.files), st.newItems, st.changed, st.moved, st.missing, st.items, time.Since(started).Round(time.Second))
	return nil
}

// walk lists the archive with the descriptions of every folder.
func (s *Syncer) walk() error {
	s.dirs = map[string]*diskDir{}
	var visit func(rel string) error
	visit = func(rel string) error {
		full := filepath.Join(s.root, filepath.FromSlash(rel))
		ents, err := os.ReadDir(full)
		if err != nil {
			if rel == "" {
				return err
			}
			log.Printf("walk %s: %v", rel, err)
			return nil
		}
		d := &diskDir{Path: rel, Name: path.Base(rel)}
		title, lines := readDescriptions(full)
		d.Title = title
		var names, sub []string
		for _, e := range ents {
			names = append(names, e.Name())
		}
		if len(lines) > 0 {
			d.Descr = matchEntries(lines, names)
		}
		s.dirs[rel] = d
		for _, e := range ents {
			name := e.Name()
			p := joinRel(rel, name)
			if e.IsDir() {
				if rel == "" && skipTopDirs[name] {
					continue
				}
				sub = append(sub, p)
				continue
			}
			if !e.Type().IsRegular() || strings.HasSuffix(name, ".part") || strings.HasSuffix(name, ".filepart") || skipSiteFiles[p] {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			s.files = append(s.files, &diskFile{Path: p, Dir: rel, Name: name, Size: info.Size(), Mtime: info.ModTime().Unix()})
		}
		sort.Strings(sub)
		for _, p := range sub {
			if err := visit(p); err != nil {
				return err
			}
		}
		return nil
	}
	return visit("")
}

func joinRel(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// findMirrors applies the rules of bin/build-landing.py: a copy of a site is
// a folder or an archive below a folder named mirror, mirrors or
// saved_mirrors, or one named like a host anywhere. Copies of the same host
// make one mirror. Files are then kept or dropped by the skip lists.
func (s *Syncer) findMirrors() {
	conf := readMirrorsConf(s.cfg.MirrorsConf)
	s.mirror = map[string]*mirrorInfo{}
	var roots []string
	for p, d := range s.dirs {
		if p == "" {
			continue
		}
		parent := strings.ToLower(path.Base(path.Dir(p)))
		if mirrorDirs[parent] || hostRe.MatchString(d.Name) {
			roots = append(roots, p)
		}
	}
	sort.Strings(roots)
	// folders inside a copy are not copies of their own
	var top []string
	for _, p := range roots {
		if len(top) == 0 || !strings.HasPrefix(p, top[len(top)-1]+"/") {
			top = append(top, p)
		}
	}
	inMirror := func(p string) string {
		i := sort.SearchStrings(top, p)
		for _, j := range []int{i, i - 1} {
			if j >= 0 && j < len(top) && (p == top[j] || strings.HasPrefix(p, top[j]+"/")) {
				return top[j]
			}
		}
		return ""
	}
	add := func(key, host string) *mirrorInfo {
		m := s.mirror[key]
		if m == nil {
			m = &mirrorInfo{Key: key, Host: host}
			s.mirror[key] = m
		}
		return m
	}
	for _, p := range top {
		name := path.Base(p)
		key := strings.TrimPrefix(strings.ToLower(name), "www.")
		m := add(key, name)
		m.Roots = append(m.Roots, p)
	}
	rootKey := map[string]string{}
	for k, m := range s.mirror {
		for _, r := range m.Roots {
			rootKey[r] = k
		}
	}

	kept := s.files[:0]
	for _, f := range s.files {
		if r := inMirror(f.Dir); r != "" {
			if skipInMirror[f.Name] || (f.Name == "index.html" && s.generatedPage(f.Path)) {
				continue
			}
			f.Mirror = rootKey[r]
			kept = append(kept, f)
			continue
		}
		if skipNames[f.Name] {
			continue
		}
		if archiveRe.MatchString(f.Name) {
			key, host := mirrorKey(f.Name)
			if mirrorDirs[strings.ToLower(path.Base(f.Dir))] || hostRe.MatchString(host) {
				m := add(key, host)
				m.Files = append(m.Files, f.Path)
				f.Mirror = key
			}
		}
		kept = append(kept, f)
	}
	s.files = kept

	for k, m := range s.mirror {
		sec := conf[k]
		if hide := strings.ToLower(sec["hide"]); hide == "yes" || hide == "ja" || hide == "1" || hide == "true" {
			delete(s.mirror, k)
			continue
		}
		m.Title = sec["title"]
		if m.Title == "" {
			m.Title = m.Host
		}
		m.Text = sec["text"]
		if m.Text == "" {
			for _, r := range m.Roots {
				if d := s.descrOf(r); d != "" && !strings.Contains(d, " - ") {
					m.Text = d
					break
				}
			}
		}
		if m.Text == "" {
			for _, p := range m.Files {
				if d := s.descrOf(p); d != "" {
					m.Text = d
					break
				}
			}
		}
	}
	// a hidden mirror's files are ordinary files again
	for _, f := range s.files {
		if f.Mirror != "" && s.mirror[f.Mirror] == nil {
			f.Mirror = ""
		}
	}
	files := s.files[:0]
	for _, f := range s.files {
		if f.Mirror == "" && skipNames[f.Name] {
			continue
		}
		files = append(files, f)
	}
	s.files = files
}

// mirrorKey: "mirror_www.prof80.de.zip" -> "prof80.de", "www.prof80.de".
func mirrorKey(name string) (key, host string) {
	stem := archiveRe.ReplaceAllString(name, "")
	if l := strings.ToLower(stem); strings.HasPrefix(l, "mirror_") || strings.HasPrefix(l, "mirror-") {
		stem = stem[7:]
	}
	return strings.TrimPrefix(strings.ToLower(stem), "www."), stem
}

// descrOf is the description the parent folder gives to p.
func (s *Syncer) descrOf(p string) string {
	if d := s.dirs[parentDir(p)]; d != nil {
		return d.Descr[path.Base(p)]
	}
	return ""
}

func parentDir(p string) string {
	if d := path.Dir(p); d != "." {
		return d
	}
	return ""
}

// generatedPage tells whether an index.html was written by Filepage or by
// bin/generate-pages.py.
func (s *Syncer) generatedPage(p string) bool {
	f, err := os.Open(filepath.Join(s.root, filepath.FromSlash(p)))
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	return bytes.Contains(head, []byte(generatedMarker)) || bytes.Contains(head, []byte(filepageMarker))
}

// readMirrorsConf reads data/mirrors.conf: [section] per site, key = value,
// indented continuation lines.
func readMirrorsConf(file string) map[string]map[string]string {
	out := map[string]map[string]string{}
	f, err := os.Open(file)
	if err != nil {
		return out
	}
	defer f.Close()
	var sec map[string]string
	last := ""
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		t := strings.TrimSpace(line)
		switch {
		case t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";"):
		case strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"):
			sec = map[string]string{}
			out[strings.ToLower(t[1:len(t)-1])] = sec
			last = ""
		case sec != nil && last != "" && (line[0] == ' ' || line[0] == '\t'):
			sec[last] += " " + t
		case sec != nil:
			if k, v, ok := strings.Cut(t, "="); ok {
				last = strings.ToLower(strings.TrimSpace(k))
				sec[last] = strings.TrimSpace(v)
			}
		}
	}
	return out
}

// hashAll reads every file whose size or mtime differ from what the catalog
// knows, several at a time, and stores each result at once, so an
// interrupted run goes on where it stopped.
func (s *Syncer) hashAll(ctx context.Context) error {
	known := map[string][3]any{}
	rows, err := s.c.Query("SELECT path, sha256, size, mtime FROM file_paths")
	if err != nil {
		return err
	}
	for rows.Next() {
		var p, sum string
		var size, mtime int64
		rows.Scan(&p, &sum, &size, &mtime)
		known[p] = [3]any{sum, size, mtime}
	}
	rows.Close()

	var todo []*diskFile
	var todoBytes int64
	for _, f := range s.files {
		if k, ok := known[f.Path]; ok && k[1].(int64) == f.Size && k[2].(int64) == f.Mtime {
			f.Sum = k[0].(string)
			continue
		}
		todo = append(todo, f)
		todoBytes += f.Size
	}
	s.logf("hash: %d files, %s to read", len(todo), humanSize(todoBytes))
	if len(todo) == 0 {
		return s.markSeen()
	}

	work := make(chan *diskFile)
	done := make(chan *diskFile)
	var wg sync.WaitGroup
	for range s.jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for f := range work {
				sum, err := hashPath(ctx, filepath.Join(s.root, filepath.FromSlash(f.Path)))
				if err != nil {
					if ctx.Err() == nil {
						log.Printf("hash %s: %v", f.Path, err)
					}
					continue
				}
				f.Sum = sum
				done <- f
			}
		}()
	}
	go func() {
		defer close(work)
		for _, f := range todo {
			select {
			case work <- f:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(done) }()

	var batch []*diskFile
	var nDone, bytesDone int64
	start, lastLog := time.Now(), time.Now()
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		err := s.storeHashes(batch)
		batch = batch[:0]
		return err
	}
	for f := range done {
		batch = append(batch, f)
		nDone++
		bytesDone += f.Size
		if len(batch) >= 200 || time.Since(lastLog) > 30*time.Second {
			if err := flush(); err != nil {
				return err
			}
		}
		if time.Since(lastLog) > 30*time.Second {
			lastLog = time.Now()
			el := time.Since(start).Seconds()
			rate := float64(bytesDone) / el
			left := time.Duration(float64(todoBytes-bytesDone)/max(rate, 1)) * time.Second
			s.logf("hash: %d of %d files, %s of %s, %s/s, about %s left", nDone, len(todo),
				humanSize(bytesDone), humanSize(todoBytes), humanSize(int64(rate)), left.Round(time.Minute))
		}
	}
	if err := flush(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("interrupted after %d of %d files; the next run goes on from there", nDone, len(todo))
	}
	return s.markSeen()
}

func hashPath(ctx context.Context, p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 4<<20)
	for {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		n, err := f.Read(buf)
		h.Write(buf[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (s *Syncer) storeHashes(batch []*diskFile) error {
	tx, err := s.c.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, f := range batch {
		if _, err := tx.Exec("INSERT OR IGNORE INTO files (sha256, size, kind, added) VALUES (?, ?, ?, ?)",
			f.Sum, f.Size, kindOf(f.Name), s.stamp); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO file_paths (path, sha256, size, mtime, seen) VALUES (?, ?, ?, ?, ?)
		    ON CONFLICT (path) DO UPDATE SET sha256 = excluded.sha256, size = excluded.size,
		    mtime = excluded.mtime, seen = excluded.seen`, f.Path, f.Sum, f.Size, f.Mtime, s.stamp); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// markSeen stamps every path found by this run.
func (s *Syncer) markSeen() error {
	tx, err := s.c.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	st, err := tx.Prepare("UPDATE file_paths SET seen = ? WHERE path = ?")
	if err != nil {
		return err
	}
	defer st.Close()
	for _, f := range s.files {
		if f.Sum != "" {
			if _, err := st.Exec(s.stamp, f.Path); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

type syncStats struct{ newItems, changed, moved, missing, items int }

// apply turns the hashed files into items and tags, in one transaction.
func (s *Syncer) apply() (st syncStats, err error) {
	var tax []TaxEntry
	if _, e := os.Stat(s.cfg.TaxonomyConf); e == nil {
		if tax, err = parseTaxonomy(s.cfg.TaxonomyConf); err != nil {
			return st, err
		}
	}
	var rules []Rule
	if _, e := os.Stat(s.cfg.LegacyMap); e == nil {
		if rules, err = parseRules(s.cfg.LegacyMap); err != nil {
			return st, err
		}
	}
	tags, err := newTags(s.c)
	if err != nil {
		return st, err
	}
	tx, err := s.c.Begin()
	if err != nil {
		return st, err
	}
	defer tx.Rollback()
	if err := tags.loadTaxonomy(tx, tax); err != nil {
		return st, fmt.Errorf("taxonomy: %w", err)
	}
	loc, err := s.locationTags(tx)
	if err != nil {
		return st, fmt.Errorf("location tags: %w", err)
	}
	a := &applier{s: s, tx: tx, tags: tags, rules: rules, loc: loc, ruleHits: map[int]int{},
		facetOf: map[int64]string{}, expand: map[string]int64{}}
	if a.groups, err = groupTags(tx); err != nil {
		return st, fmt.Errorf("group tags: %w", err)
	}
	if rows, err := tx.Query("SELECT id, facet, name FROM tags"); err == nil {
		for rows.Next() {
			var id int64
			var facet, name string
			rows.Scan(&id, &facet, &name)
			a.facetOf[id] = facet
			if facet == "kind" && name == "Mirror" {
				a.mirror = id
			}
		}
		rows.Close()
	}
	if err := a.items(&st); err != nil {
		return st, err
	}
	if err := a.missing(&st); err != nil {
		return st, err
	}
	if err := rebuildFTS(tx); err != nil {
		return st, fmt.Errorf("full text: %w", err)
	}
	if err := rebuildClosure(tx); err != nil {
		return st, fmt.Errorf("tag closure: %w", err)
	}
	tx.QueryRow("SELECT COUNT(*) FROM items WHERE state = 'live'").Scan(&st.items)
	hits := make([]string, len(rules))
	for i, r := range rules {
		hits[i] = fmt.Sprintf("%d\t%d\t%s", r.Line, a.ruleHits[i], r.Text)
	}
	tx.Exec("INSERT OR REPLACE INTO meta (key, value) VALUES ('rule_hits', ?)", strings.Join(hits, "\n"))
	return st, tx.Commit()
}

// locationTags makes one location tag per folder, named by its title, and
// returns them by folder path.
func (s *Syncer) locationTags(tx *sql.Tx) (map[string]int64, error) {
	out := map[string]int64{}
	rows, err := tx.Query("SELECT id, path FROM tags WHERE facet = 'location'")
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var p string
		rows.Scan(&id, &p)
		out[p] = id
	}
	rows.Close()
	paths := make([]string, 0, len(s.dirs))
	for p := range s.dirs {
		// objekte/ is storage by item number, not a place to browse
		if p != "" && p != "objekte" && !strings.HasPrefix(p, "objekte/") {
			paths = append(paths, p)
		}
	}
	sort.Strings(paths) // parents first
	for _, p := range paths {
		d := s.dirs[p]
		name := dirLabel(d.Name, d.Title)
		descr := s.descrOf(p)
		parent := out[parentDir(p)]
		if id, ok := out[p]; ok {
			if _, err := tx.Exec("UPDATE tags SET name = ?, descr = ?, parent = NULLIF(?, 0) WHERE id = ? AND rev = 0", name, descr, parent, id); err != nil {
				return nil, err
			}
			continue
		}
		slug, err := freeSlug(tx, "location", parent, slugify(d.Name))
		if err != nil {
			return nil, err
		}
		res, err := tx.Exec("INSERT INTO tags (facet, parent, name, slug, descr, path) VALUES ('location', NULLIF(?, 0), ?, ?, ?, ?)",
			parent, name, slug, descr, p)
		if err != nil {
			return nil, err
		}
		out[p], _ = res.LastInsertId()
	}
	return out, nil
}

type applier struct {
	s        *Syncer
	tx       *sql.Tx
	tags     *Tags
	rules    []Rule
	loc      map[string]int64
	ruleHits map[int]int
	facetOf  map[int64]string  // every tag's facet
	mirror   int64             // the kind tag "Mirror"
	expand   map[string]int64  // rule tag path -> tag id, filled as rules are used
	groups   map[string]int64  // folder -> its group tag
	held     map[string]heldBy // folder -> the items in it and below
}

// heldBy tells which items a folder holds, as far as a group needs it:
// one, or more than one.
type heldBy struct {
	first int64
	many  bool
}

type itemFile struct {
	item       int64
	sha, name  string
	path       string
	itemRev    int
	itemRoot   sql.NullString
	itemState  string
	pathSeen   int64
	pathExists bool
}

func (a *applier) items(st *syncStats) error {
	tx, s := a.tx, a.s
	// what the catalog knows: the files of every item and whether they
	// were found by this run
	byPath := map[string]*itemFile{}
	vanished := map[string][]*itemFile{} // by hash
	rows, err := tx.Query(`SELECT f.item, f.sha256, f.name, f.path, i.rev, i.root, i.state, COALESCE(p.seen, 0)
	    FROM item_files f JOIN items i ON i.id = f.item LEFT JOIN file_paths p ON p.path = f.path`)
	if err != nil {
		return err
	}
	for rows.Next() {
		x := &itemFile{}
		rows.Scan(&x.item, &x.sha, &x.name, &x.path, &x.itemRev, &x.itemRoot, &x.itemState, &x.pathSeen)
		byPath[x.path] = x
		if x.pathSeen < s.stamp && !x.itemRoot.Valid {
			vanished[x.sha] = append(vanished[x.sha], x)
		}
	}
	rows.Close()

	// mirrors: one item each
	mirrorItem := map[string]int64{}
	keys := make([]string, 0, len(s.mirror))
	for k := range s.mirror {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		id, err := a.mirrorItem(s.mirror[k], byPath, st)
		if err != nil {
			return err
		}
		mirrorItem[k] = id
	}

	touched := map[int64]bool{}
	ord := map[int64]int{}
	// the files of each item, for its tags; and how many it has: titles
	// from the descriptions belong to items of one file
	type fileRef struct{ dir, name string }
	refs := map[int64][]fileRef{}
	editable := map[int64]bool{}
	nfiles := map[int64]int{}
	if rows, err := tx.Query("SELECT item, COUNT(*) FROM item_files GROUP BY item"); err == nil {
		for rows.Next() {
			var id int64
			var n int
			rows.Scan(&id, &n)
			nfiles[id] = n
		}
		rows.Close()
	}
	for _, f := range s.files {
		if f.Sum == "" {
			continue // could not be read
		}
		if f.Mirror != "" {
			id := mirrorItem[f.Mirror]
			m := s.mirror[f.Mirror]
			name := mirrorName(m, f.Path)
			if x := byPath[f.Path]; x != nil && x.item == id {
				if x.sha != f.Sum {
					st.changed++
				}
				if _, err := tx.Exec("UPDATE item_files SET sha256 = ?, name = ? WHERE path = ?", f.Sum, name, f.Path); err != nil {
					return err
				}
			} else {
				if x != nil {
					// was an item of its own before the mirror was known
					if err := a.mergeInto(x.item, id); err != nil {
						return err
					}
				}
				ord[id]++
				role := "main"
				if !strings.HasPrefix(f.Path, m.root()+"/") {
					role = "extra"
				}
				if _, err := tx.Exec(`INSERT INTO item_files (item, sha256, name, path, ord, role) VALUES (?, ?, ?, ?, ?, ?)
				    ON CONFLICT (item, name) DO UPDATE SET sha256 = excluded.sha256, path = excluded.path`,
					id, f.Sum, name, f.Path, ord[id], role); err != nil {
					return err
				}
			}
			touched[id] = true
			continue
		}

		descr := s.dirs[f.Dir].Descr[f.Name]
		x := byPath[f.Path]
		if x == nil {
			// a file that moved: same bytes, its old place gone
			if c := vanished[f.Sum]; len(c) > 0 {
				x = c[0]
				vanished[f.Sum] = c[1:]
				if _, err := tx.Exec("UPDATE item_files SET path = ?, name = ? WHERE item = ? AND path = ?", f.Path, f.Name, x.item, x.path); err != nil {
					return err
				}
				if _, err := tx.Exec("INSERT OR REPLACE INTO redirects (old_path, item, name, at) VALUES (?, ?, ?, ?)", x.path, x.item, f.Name, s.stamp); err != nil {
					return err
				}
				delete(byPath, x.path)
				x.path = f.Path
				byPath[f.Path] = x
				st.moved++
			}
		}
		var id int64
		if x == nil {
			// a new file in an item's folder below objekte/ belongs to it
			if sid := storedID(f.Path); sid != 0 {
				var n int
				tx.QueryRow("SELECT COUNT(*) FROM items WHERE id = ?", sid).Scan(&n)
				if n > 0 {
					name := strings.TrimPrefix(f.Path, storeDir(sid)+"/")
					var o int
					tx.QueryRow("SELECT COALESCE(MAX(ord), 0) + 1 FROM item_files WHERE item = ?", sid).Scan(&o)
					if _, err := tx.Exec("INSERT OR IGNORE INTO item_files (item, sha256, name, path, ord) VALUES (?, ?, ?, ?, ?)", sid, f.Sum, name, f.Path, o); err != nil {
						return err
					}
					x = &itemFile{item: sid, sha: f.Sum, name: name, path: f.Path, itemRev: 1}
					byPath[f.Path] = x
					nfiles[sid]++
				}
			}
		}
		if x == nil {
			title := descr
			if title == "" {
				title = f.Name
			}
			res, err := tx.Exec("INSERT INTO items (title, descr, created, updated) VALUES (?, ?, ?, ?)", title, descr, s.stamp, s.stamp)
			if err != nil {
				return err
			}
			id, _ = res.LastInsertId()
			if _, err := tx.Exec("INSERT INTO item_files (item, sha256, name, path, ord) VALUES (?, ?, ?, ?, 0)", id, f.Sum, f.Name, f.Path); err != nil {
				return err
			}
			st.newItems++
			x = &itemFile{item: id, sha: f.Sum, name: f.Name, path: f.Path}
			byPath[f.Path] = x
			nfiles[id] = 1
		} else {
			id = x.item
			if x.sha != f.Sum {
				if _, err := tx.Exec("UPDATE item_files SET sha256 = ? WHERE item = ? AND path = ?", f.Sum, id, f.Path); err != nil {
					return err
				}
				st.changed++
			}
			if x.itemRev == 0 && nfiles[id] == 1 {
				title := descr
				if title == "" {
					title = f.Name
				}
				if _, err := tx.Exec("UPDATE items SET title = ?, descr = ? WHERE id = ? AND (title != ? OR descr != ?)", title, descr, id, title, descr); err != nil {
					return err
				}
			}
		}
		refs[id] = append(refs[id], fileRef{f.Dir, f.Name})
		editable[id] = x.itemRev == 0
		touched[id] = true
	}
	mirrorIDs := map[int64]bool{}
	for k, id := range mirrorItem {
		m := s.mirror[k]
		refs[id] = []fileRef{{parentDir(m.root()), path.Base(m.root())}}
		editable[id] = true
		mirrorIDs[id] = true
	}
	ids := make([]int64, 0, len(refs))
	for id := range refs {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	// what each folder holds, for the groups: the live items, without the
	// file lists and caches
	gone := map[int64]bool{}
	if rows, err := tx.Query("SELECT id FROM items WHERE state NOT IN ('live', 'missing')"); err == nil {
		for rows.Next() {
			var id int64
			rows.Scan(&id)
			gone[id] = true
		}
		rows.Close()
	}
	a.held = map[string]heldBy{}
	for _, id := range ids {
		if mirrorIDs[id] || gone[id] {
			continue
		}
		if rs := refs[id]; len(rs) == 1 && listingNames[strings.ToLower(rs[0].name)] {
			continue
		}
		for _, r := range refs[id] {
			for d := r.dir; ; d = parentDir(d) {
				h, ok := a.held[d]
				switch {
				case !ok:
					a.held[d] = heldBy{first: id}
				case h.first != id && !h.many:
					a.held[d] = heldBy{first: h.first, many: true}
				}
				if d == "" {
					break
				}
			}
		}
	}
	for _, id := range ids {
		var dirs, names []string
		for _, r := range refs[id] {
			dirs = append(dirs, r.dir)
			names = append(names, r.name)
		}
		if err := a.tagItem(id, dirs, names, editable[id]); err != nil {
			return err
		}
	}
	// items found again
	for id := range touched {
		if _, err := tx.Exec("UPDATE items SET state = 'live' WHERE id = ? AND state = 'missing'", id); err != nil {
			return err
		}
	}
	return nil
}

// root is the folder of a mirror, or the first archive if it has no folder.
func (m *mirrorInfo) root() string {
	if len(m.Roots) > 0 {
		return m.Roots[0]
	}
	return m.Files[0]
}

// mirrorName is the name of a file within its mirror: relative to the
// mirror's folder; other copies keep their own name as the first part.
func mirrorName(m *mirrorInfo, p string) string {
	if r, ok := strings.CutPrefix(p, m.root()+"/"); ok {
		return r
	}
	for _, r := range m.Roots[min(1, len(m.Roots)):] {
		if strings.HasPrefix(p, r+"/") {
			return path.Base(r) + "/" + strings.TrimPrefix(p, r+"/")
		}
	}
	return path.Base(p)
}

// mirrorItem finds or creates the item of a mirror.
func (a *applier) mirrorItem(m *mirrorInfo, byPath map[string]*itemFile, st *syncStats) (int64, error) {
	var id int64
	err := a.tx.QueryRow("SELECT id FROM items WHERE root = ? AND state IN ('live', 'missing')", m.root()).Scan(&id)
	if err == sql.ErrNoRows {
		for _, p := range m.Files {
			if x := byPath[p]; x != nil && x.itemRoot.Valid {
				id = x.item
				break
			}
		}
	} else if err != nil {
		return 0, err
	}
	if id != 0 {
		_, err := a.tx.Exec("UPDATE items SET root = ?, title = CASE WHEN rev = 0 THEN ? ELSE title END, descr = CASE WHEN rev = 0 THEN ? ELSE descr END WHERE id = ?",
			m.root(), m.Title, m.Text, id)
		return id, err
	}
	res, err := a.tx.Exec("INSERT INTO items (title, descr, root, created, updated) VALUES (?, ?, ?, ?, ?)", m.Title, m.Text, m.root(), a.s.stamp, a.s.stamp)
	if err != nil {
		return 0, err
	}
	st.newItems++
	id, _ = res.LastInsertId()
	if _, err := a.tx.Exec("INSERT INTO item_tags (item, tag, added_by, added_at) SELECT ?, id, 'import', ? FROM tags WHERE facet = 'kind' AND name = 'Mirror'", id, a.s.stamp); err != nil {
		return 0, err
	}
	return id, nil
}

// mergeInto moves the files of item from into item to; from stays as a
// redirect.
func (a *applier) mergeInto(from, to int64) error {
	if _, err := a.tx.Exec("DELETE FROM item_files WHERE item = ?", from); err != nil {
		return err
	}
	_, err := a.tx.Exec("UPDATE items SET state = 'merged', merged_into = ?, updated = ? WHERE id = ?", to, a.s.stamp, from)
	return err
}

// tagItem gives the item the location tags of the folders its files lie
// in, the group of the folder that holds it below the folder a rule names
// its machine, magazine or meeting by, and, while nobody has edited it,
// the tags of the rules for each file. Only what differs from the tags the
// item has is written, so a sync that finds nothing new writes nothing.
func (a *applier) tagItem(id int64, dirs, names []string, rules bool) error {
	tx := a.tx
	want := map[int64]string{} // tag -> added_by
	for _, dir := range dirs {
		if t, ok := a.loc[dir]; ok {
			want[t] = "import"
		}
	}
	type pathTag struct {
		facet, path string
		id          int64
		by          string
		at          string // the folder the rule names
		glob        bool   // the rule names files, too
	}
	var found []pathTag
	for i := range a.rules {
		r := &a.rules[i]
		var caps []string
		var at string
		ok := false
		for j := range dirs {
			if caps, at, ok = r.matchAt(dirs[j], names[j]); ok {
				break
			}
		}
		if !ok {
			continue
		}
		if rules {
			a.ruleHits[i]++
		}
		by := "import"
		if r.Weak {
			by = "import:weak"
		}
		for _, rt := range r.Tags {
			p := rt.expand(caps, a.s.label)
			key := rt.Facet + "\x00" + p
			t, ok := a.expand[key]
			if !ok {
				var err error
				if t, err = a.tags.ensure(tx, rt.Facet, p); err != nil {
					return fmt.Errorf("legacy-map.conf:%d: %w", r.Line, err)
				}
				a.expand[key] = t
				a.facetOf[t] = rt.Facet
			}
			found = append(found, pathTag{rt.Facet, p, t, by, at, r.Glob != ""})
		}
	}
	// the deepest tag of a tree (DEC > VAX) stands for those above it
	deepest := func(f pathTag) bool {
		for _, g := range found {
			if g.facet == f.facet && strings.HasPrefix(g.path, f.path+tagSep) {
				return false
			}
		}
		return true
	}
	if rules {
		// of the kinds, the strong ones win over the guesses, and of
		// those the rule naming the deepest folder, and of a folder the
		// rule naming files
		weight := func(f pathTag) int {
			w := 2 * len(f.at)
			if f.glob {
				w++
			}
			return w
		}
		kindWeight := -1
		for _, f := range found {
			if f.facet == "kind" && f.by == "import" && weight(f) > kindWeight {
				kindWeight = weight(f)
			}
		}
		for _, f := range found {
			switch {
			case (facetIsTree(f.facet) || f.facet == "source") && !deepest(f):
			case f.facet == "kind" && kindWeight >= 0 && (f.by != "import" || weight(f) != kindWeight):
			case want[f.id] != "import": // a strong tag replaces a weak one
				want[f.id] = f.by
			}
		}
	}
	// the group: the folder below the one the machine, magazine or meeting
	// was found by, that holds the item and others; a folder a rule names
	// is said by its tags already
	var scope pathTag
	named := map[string]bool{}
	for _, f := range found {
		named[f.at] = true
		if facetIsTree(f.facet) && deepest(f) && len(f.at) >= len(scope.at) {
			scope = f
		}
	}
	// a collection named by its source holds groups, too, where no
	// machine, magazine or meeting does
	if scope.id == 0 {
		for _, f := range found {
			if f.facet == "source" && deepest(f) && len(f.at) >= len(scope.at) {
				scope = f
			}
		}
	}
	group := int64(0)
	if scope.id != 0 {
		dir := commonDir(dirs)
		for dir != "" && (!a.held[dir].many || named[dir]) {
			dir = parentDir(dir)
		}
		if strings.HasPrefix(dir, scope.at+"/") {
			var err error
			if group, err = a.group(dir, scope.at, scope.id, named); err != nil {
				return err
			}
		}
	}
	have := map[int64]string{}
	rows, err := tx.Query("SELECT tag, added_by FROM item_tags WHERE item = ?", id)
	if err != nil {
		return err
	}
	for rows.Next() {
		var t int64
		var by string
		rows.Scan(&t, &by)
		have[t] = by
	}
	rows.Close()
	// a group someone gave the item by hand replaces the folder's
	grouped := false
	for t, by := range have {
		if a.facetOf[t] == "group" && by != "import" && by != "import:weak" {
			grouped = true
		}
	}
	if group != 0 && !grouped {
		want[group] = "import"
	}
	for t, by := range have {
		if by != "import" && by != "import:weak" {
			continue // someone else's
		}
		if _, ok := want[t]; ok {
			continue
		}
		// the import's tags that no longer apply: the folders and groups
		// always, the rules' only while the item is unedited; the mirror's
		// kind stays
		if f := a.facetOf[t]; f == "location" || f == "group" || (rules && t != a.mirror) {
			if _, err := tx.Exec("DELETE FROM item_tags WHERE item = ? AND tag = ?", id, t); err != nil {
				return err
			}
		}
	}
	for t, by := range want {
		old, ok := have[t]
		switch {
		case !ok:
			if _, err := tx.Exec("INSERT INTO item_tags (item, tag, added_by, added_at) VALUES (?, ?, ?, ?)", id, t, by, a.s.stamp); err != nil {
				return err
			}
		case old != by && (old == "import" || old == "import:weak"):
			if _, err := tx.Exec("UPDATE item_tags SET added_by = ? WHERE item = ? AND tag = ?", by, id, t); err != nil {
				return err
			}
		}
	}
	return nil
}

// groupTags are the group tags made from folders, by folder; one that was
// merged into another stands for that one.
func groupTags(tx *sql.Tx) (map[string]int64, error) {
	out := map[string]int64{}
	rows, err := tx.Query(`WITH RECURSIVE m(id, to_id) AS (
	        SELECT id, id FROM tags WHERE facet = 'group' AND path LIKE 'group:%'
	        UNION SELECT m.id, t.merged_into FROM m JOIN tags t ON t.id = m.to_id WHERE t.merged_into IS NOT NULL)
	    SELECT substr(t.path, 7), m.to_id FROM m JOIN tags t ON t.id = m.id
	    WHERE NOT EXISTS (SELECT 1 FROM tags x WHERE x.id = m.to_id AND x.merged_into IS NOT NULL)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p string
		var id int64
		rows.Scan(&p, &id)
		out[p] = id
	}
	return out, rows.Err()
}

// group is the group tag of a folder below the folder at, which names the
// machine, magazine or meeting scope; the groups of the folders between,
// but those a rule names, are its parents. It is made when missing, named
// by the folder's title.
func (a *applier) group(dir, at string, scope int64, named map[string]bool) (int64, error) {
	if id, ok := a.groups[dir]; ok {
		return id, nil
	}
	up := parentDir(dir)
	for up != at && named[up] {
		up = parentDir(up)
	}
	var parent int64
	if up != at {
		var err error
		if parent, err = a.group(up, at, scope, named); err != nil {
			return 0, err
		}
	}
	slug, err := freeSlug(a.tx, "group", parent, slugify(path.Base(dir)))
	if err != nil {
		return 0, err
	}
	res, err := a.tx.Exec("INSERT INTO tags (facet, parent, scope, name, slug, path) VALUES ('group', NULLIF(?, 0), ?, ?, ?, ?)",
		parent, scope, a.s.label(dir), slug, "group:"+dir)
	if err != nil {
		return 0, err
	}
	id, _ := res.LastInsertId()
	a.groups[dir] = id
	a.facetOf[id] = "group"
	return id, nil
}

// commonDir is the deepest folder that holds all the folders.
func commonDir(dirs []string) string {
	if len(dirs) == 0 {
		return ""
	}
	c := dirs[0]
	for _, d := range dirs[1:] {
		for c != "" && d != c && !strings.HasPrefix(d, c+"/") {
			c = parentDir(c)
		}
	}
	return c
}

// missing marks items none of whose files were found.
func (a *applier) missing(st *syncStats) error {
	res, err := a.tx.Exec(`UPDATE items SET state = 'missing', updated = ? WHERE state = 'live'
	    AND NOT EXISTS (SELECT 1 FROM item_files f JOIN file_paths p ON p.path = f.path WHERE f.item = items.id AND p.seen >= ?)`,
		a.s.stamp, a.s.stamp)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	st.missing = int(n)
	return nil
}

func rebuildFTS(tx *sql.Tx) error {
	if _, err := tx.Exec("DELETE FROM items_fts"); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO items_fts (rowid, title, descr, tags, names)
	    SELECT i.id, i.title, i.descr,
	        COALESCE((SELECT group_concat(t.name, ' ') FROM item_tags it JOIN tags t ON t.id = it.tag WHERE it.item = i.id), ''),
	        COALESCE((SELECT group_concat(f.name, ' ') FROM item_files f WHERE f.item = i.id), '')
	    FROM items i WHERE i.state = 'live'`)
	return err
}

// label is the name of a folder of the archive.
func (s *Syncer) label(p string) string {
	if d := s.dirs[p]; d != nil {
		return dirLabel(d.Name, d.Title)
	}
	return folderTitle(path.Base(p))
}

// dirLabel chooses the name of a folder. Filepage titles are breadcrumbs
// ("public - pub - roms - VICTOR-SIRIUS"), and some were copied into
// subfolders unchanged, so a breadcrumb only counts when its last part
// names this folder; one-part titles are real titles. The same rule as in
// bin/generate-pages.py.
func dirLabel(name, title string) string {
	name = folderTitle(name)
	title = strings.TrimSpace(title)
	if title == "" {
		return name
	}
	var segs []string
	for _, s := range strings.Split(title, " - ") {
		if s = strings.TrimSpace(s); s != "" {
			segs = append(segs, s)
		}
	}
	if len(segs) <= 1 {
		return title
	}
	last := segs[len(segs)-1]
	normName := strings.NewReplacer(" ", "", "_", "", "-", "")
	if strings.ToLower(normName.Replace(last)) != strings.ToLower(normName.Replace(name)) {
		return name
	}
	if strings.Contains(last, " ") {
		return last
	}
	return name
}

// folderTitle makes a folder name readable as a title: the old tree marks
// sorting folders with underscores ("_TRS-80_Genie" is "TRS-80 Genie").
func folderTitle(name string) string {
	c := strings.Join(strings.Fields(strings.ReplaceAll(name, "_", " ")), " ")
	if c == "" {
		return name
	}
	return c
}
