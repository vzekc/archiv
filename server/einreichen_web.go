package archiv

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

func (a *App) routes() http.Handler {
	b := a.cfg.BasePath
	mux := http.NewServeMux()
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }

	page("GET "+b+"/{$}", a.home)
	page("POST "+b+"/neu", a.post(a.newSubmission))
	page("GET "+b+"/liste", a.list)
	page("GET "+b+"/hilfe", a.help)
	page("GET "+b+"/e/{id}", a.show)
	page("GET "+b+"/e/{id}/status", a.status)
	page("POST "+b+"/e/{id}/speichern", a.post(a.save))
	page("POST "+b+"/e/{id}/einreichen", a.post(a.submit))
	page("POST "+b+"/e/{id}/loeschen", a.post(a.deleteDraft))
	page("POST "+b+"/e/{id}/antworten", a.post(a.answer))
	page("POST "+b+"/f/{fid}/entfernen", a.post(a.removeFile))
	page("POST "+b+"/e/{id}/doubletten-entfernen", a.post(a.removeDuplicates))
	page("POST "+b+"/e/{id}/vorschlag", a.post(a.adopt))
	page("GET "+b+"/f/{fid}/bild/{which}", a.image)
	page("GET "+b+"/f/{fid}/datei", a.download)

	// moderation
	page("GET "+b+"/moderation", a.mod(a.modList))
	page("GET "+b+"/e/{id}/uebernehmen", a.mod(a.claim))
	page("POST "+b+"/e/{id}/zurueckgeben", a.mod(a.post(a.release)))
	page("POST "+b+"/e/{id}/kommentar", a.mod(a.post(a.comment)))
	page("POST "+b+"/e/{id}/rueckfrage", a.mod(a.post(a.ask)))
	page("POST "+b+"/e/{id}/ablehnen", a.mod(a.post(a.reject)))
	page("POST "+b+"/e/{id}/aufnehmen", a.mod(a.post(a.accept)))
	page("POST "+b+"/e/{id}/analysieren", a.mod(a.post(a.reanalyse)))
	page("GET "+b+"/kontingente", a.mod(a.quotas))
	page("POST "+b+"/kontingente", a.mod(a.post(a.setQuota)))

	page("GET /moderation/import", a.mod(a.importReport))

	a.accountRoutes(mux)
	if a.mailbox != nil {
		page("GET /nachrichten", a.inboxPage)
	}
	for _, e := range a.exts {
		if rp, ok := e.(RouteProvider); ok {
			rp.Routes(extRouter{a: a, mux: mux})
		}
	}

	mux.Handle(b+"/upload/", a.uploadGuard(http.StripPrefix(b+"/upload/", a.tus)))
	mux.Handle("GET "+b+"/static/", http.StripPrefix(b+"/static/", staticHandler()))
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", assetHandler()))
	a.siteRoutes(mux)
	return a.withUser(securityHeaders(mux))
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		next.ServeHTTP(w, r)
	})
}

// post wraps form handlers: parses the form and checks the CSRF token.
func (a *App) post(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		if err := r.ParseForm(); err != nil || !a.csrfOK(r) {
			a.fail(w, r, http.StatusForbidden, "Die Seite ist abgelaufen. Bitte lade sie neu und versuche es noch einmal.")
			return
		}
		h(w, r)
	}
}

func (a *App) mod(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).Moderator {
			a.fail(w, r, http.StatusForbidden, "Das dürfen nur Moderatoren.")
			return
		}
		h(w, r)
	}
}

type Page struct {
	Title    string
	User     *User
	UserName string
	CSRF     string
	Base     string
	Notice   string
	Data     any
}

func (a *App) render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	u := currentUser(r)
	p := Page{Title: title, User: u, UserName: a.names.Name(u.ID), CSRF: a.csrfToken(w, r),
		Base: a.cfg.BasePath, Notice: notices[r.URL.Query().Get("ok")], Data: data}
	var buf bytes.Buffer
	if err := a.tmpl.ExecuteTemplate(&buf, name, p); err != nil {
		logf("template %s: %v", name, err)
		http.Error(w, "interner Fehler", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	buf.WriteTo(w)
}

var notices = map[string]string{
	"gespeichert":    "Gespeichert.",
	"eingereicht":    "Eingereicht. Das Sprachmodell ordnet die Einreichung jetzt ein, danach geht sie an die Moderatoren.",
	"geloescht":      "Der Entwurf ist gelöscht.",
	"entfernt":       "Die Datei ist entfernt.",
	"doubletten":     "Die Dateien, die es schon gibt, sind entfernt.",
	"uebernommen":    "Du bearbeitest diese Einreichung jetzt.",
	"vergeben":       "Diese Einreichung bearbeitet schon jemand anderes.",
	"zurueckgegeben": "Die Einreichung ist wieder frei.",
	"kommentiert":    "Kommentar gespeichert.",
	"gefragt":        "Die Rückfrage ist an den Einreicher gegangen.",
	"abgelehnt":      "Die Einreichung ist abgelehnt.",
	"aufgenommen":    "Die Aufnahme läuft. Die Dateien sind in wenigen Minuten im Archiv.",
	"analyse":        "Das Sprachmodell ordnet die Einreichung neu ein.",
	"beantwortet":    "Danke! Deine Antwort ist bei den Moderatoren.",
	"vorschlag":      "Vorschlag übernommen.",
	"kontingent":     "Kontingent gespeichert.",
}

func (a *App) fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	w.WriteHeader(status)
	a.render(w, r, "error", "Fehler", msg)
}

func (a *App) redirect(w http.ResponseWriter, r *http.Request, to, notice string) {
	if notice != "" {
		sep := "?"
		if strings.Contains(to, "?") {
			sep = "&"
		}
		to += sep + "ok=" + notice
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

func pathID(r *http.Request, name string) int64 {
	n, _ := strconv.ParseInt(r.PathValue(name), 10, 64)
	return n
}

// loadSub fetches the submission of the request and checks that the user
// may see it: drafts only their owner, everything else every member.
func (a *App) loadSub(w http.ResponseWriter, r *http.Request) *Submission {
	s, err := a.db.submission(pathID(r, "id"))
	u := currentUser(r)
	if err != nil || (s.State == StDraft && s.Owner != u.ID) {
		a.fail(w, r, http.StatusNotFound, "Diese Einreichung gibt es nicht.")
		return nil
	}
	return s
}

func (a *App) loadOwnFile(w http.ResponseWriter, r *http.Request) (*File, *Submission) {
	f, err := a.db.file(pathID(r, "fid"))
	if err == nil {
		s, err := a.db.submission(f.Submission)
		if err == nil && s.Owner == currentUser(r).ID {
			return f, s
		}
	}
	a.fail(w, r, http.StatusNotFound, "Diese Datei gibt es nicht.")
	return nil, nil
}

func editable(s *Submission) bool { return s.State == StDraft || s.State == StQuestion }

// ------------------------------------------------------------------ pages

type HomeData struct {
	Mine    []SubRow
	Open    int
	Quota   Quota
	Pending int64
}

type SubRow struct {
	S       *Submission
	Owner   string
	NFiles  int
	Bytes   int64
	Claimer string
}

func (a *App) rows(subs []*Submission) []SubRow {
	out := make([]SubRow, 0, len(subs))
	for _, s := range subs {
		row := SubRow{S: s, Owner: a.names.Name(s.Owner)}
		a.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(size), 0) FROM files WHERE submission = ? AND status <> ?",
			s.ID, FUploading).Scan(&row.NFiles, &row.Bytes)
		if s.ClaimedBy.Valid {
			row.Claimer = a.names.Name(s.ClaimedBy.String)
		}
		out = append(out, row)
	}
	return out
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	mine, err := a.db.submissions("owner = ? ORDER BY updated DESC", u.ID)
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	d := HomeData{Mine: a.rows(mine), Quota: a.db.quota(a.cfg, u.ID), Pending: a.db.pendingBytes(u.ID)}
	if u.Moderator {
		a.db.QueryRow("SELECT COUNT(*) FROM submissions WHERE state IN (?, ?)", StModeration, StFailed).Scan(&d.Open)
	}
	a.render(w, r, "home", "Einreichungen", d)
}

func (a *App) help(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "help", "So funktioniert das Einreichen", nil)
}

func (a *App) list(w http.ResponseWriter, r *http.Request) {
	subs, err := a.db.submissions("state <> ? ORDER BY COALESCE(submitted, created) DESC LIMIT 500", StDraft)
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	a.render(w, r, "list", "Eingereicht", a.rows(subs))
}

func (a *App) modList(w http.ResponseWriter, r *http.Request) {
	type ModData struct {
		Open, Question, Analysing, Recent []SubRow
	}
	var d ModData
	get := func(where string, args ...any) []SubRow {
		s, _ := a.db.submissions(where, args...)
		return a.rows(s)
	}
	d.Open = get("state IN (?, ?, ?) ORDER BY submitted", StModeration, StFailed, StIntake)
	d.Question = get("state = ? ORDER BY submitted", StQuestion)
	d.Analysing = get("state = ? ORDER BY submitted", StAnalysing)
	d.Recent = get("state IN (?, ?) ORDER BY decided_at DESC LIMIT 30", StDone, StRejected)
	a.render(w, r, "moderation", "Moderation", d)
}

func (a *App) newSubmission(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	t := now()
	res, err := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated) VALUES (?,?,?,?,?)",
		u.ID, strings.TrimSpace(r.PostFormValue("title")), StDraft, t, t)
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	id, _ := res.LastInsertId()
	a.db.logEvent(u.ID, id, "created", nil)
	a.redirect(w, r, a.url("/e/%d", id), "")
}

// ------------------------------------------------------------ submission

type FileView struct {
	F    *File
	Info FileInfo
	Dups []Duplicate
	Sugg *Suggestion
	Tags []string  // chosen by the moderator
	Own  []string  // tags of its own, apart from its group's
	Item *ItemLink // the item it became in the archive
}

// ItemLink is an item of the archive a submission's file went into, and
// the items it was split into.
type ItemLink struct {
	ID        int64
	Title     string
	Withdrawn string // why it is no longer shown
	Parts     []ItemLink
}

// itemOfPath is the item holding the archive file p, live or withdrawn,
// with the items it was split into.
func (a *App) itemOfPath(p string) *ItemLink {
	l := &ItemLink{}
	var state string
	if p == "" || a.cat.QueryRow(`SELECT i.id, i.title, i.state, i.withdrawn_reason FROM item_files f JOIN items i ON i.id = f.item
	    WHERE f.path = ? AND i.state IN ('live', 'withdrawn')`, p).Scan(&l.ID, &l.Title, &state, &l.Withdrawn) != nil {
		return nil
	}
	if state != "withdrawn" {
		l.Withdrawn = ""
		return l
	}
	rows, err := a.cat.Query(`SELECT i.id, i.title FROM revisions r JOIN items i ON i.id = CAST(substr(r.target, 6) AS INTEGER)
	    WHERE r.op = 'create-item' AND json_extract(r.data, '$.from') = ? AND i.state = 'live' ORDER BY i.id`, l.ID)
	if err == nil {
		for rows.Next() {
			var x ItemLink
			rows.Scan(&x.ID, &x.Title)
			l.Parts = append(l.Parts, x)
		}
		rows.Close()
	}
	return l
}

// GroupView is a set of files with the same suggestion; files without one
// form a group of their own.
type GroupView struct {
	N     int
	Sugg  *Suggestion // the group's suggestion, nil when there is none yet
	Files []FileView
	Tags  []string // the tags the moderator form starts with
}

type SubView struct {
	Items     []*ItemLink // what the submission became in the archive
	S         *Submission
	Owner     string
	Files     []FileView
	Groups    []*GroupView
	Comments  []Comment
	Events    []Event
	Claimer   string
	IsOwner   bool
	CanEdit   bool
	Mod       bool
	MineClaim bool
	CanDecide bool
	Busy      bool // files still uploading or being processed
	Checked   int  // files the server is done with
	Dups      int  // checked files that exist already elsewhere
	Bytes     int64
	Quota     Quota
	Final     *Analysis
	AIOn      bool
}

func (a *App) subView(r *http.Request, s *Submission) (*SubView, error) {
	u := currentUser(r)
	v := &SubView{S: s, Owner: a.names.Name(s.Owner), IsOwner: s.Owner == u.ID, Mod: u.Moderator}
	v.CanEdit = v.IsOwner && editable(s)
	v.AIOn = a.ai.enabled()
	if s.ClaimedBy.Valid {
		v.Claimer = a.names.Name(s.ClaimedBy.String)
		v.MineClaim = s.ClaimedBy.String == u.ID
	}
	v.CanDecide = v.Mod && v.MineClaim && (s.State == StModeration || s.State == StFailed)
	files, err := a.db.filesOf(s.ID)
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		fv := FileView{F: f, Info: parseInfo(f.Info), Dups: parseDuplicates(f.Duplicates), Sugg: parseSuggestion(f.Suggestion), Tags: tagLines(f.Tags),
			Item: a.itemOfPath(f.ArchivePath)}
		if fv.Item != nil && !slices.ContainsFunc(v.Items, func(l *ItemLink) bool { return l.ID == fv.Item.ID }) {
			v.Items = append(v.Items, fv.Item)
		}
		if f.Status == FProcessing || f.Status == FUploading {
			v.Busy = true
		} else {
			v.Checked++
		}
		if isDuplicate(f) {
			v.Dups++
		}
		v.Bytes += f.Size
		v.Files = append(v.Files, fv)
	}
	v.Groups = groupFiles(v.Files)
	if v.Mod || v.IsOwner {
		v.Comments, _ = a.db.comments(s.ID)
	}
	if v.Mod {
		v.Events, _ = a.db.events(s.ID)
		if an := parseAnalysis(s.Analysis); an != nil && an.Final {
			v.Final = an
		}
	}
	if v.IsOwner {
		v.Quota = a.db.quota(a.cfg, u.ID)
	}
	return v, nil
}

func (a *App) show(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	v, err := a.subView(r, s)
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	title := s.Title
	if title == "" {
		title = "Neue Einreichung"
	}
	a.render(w, r, "submission", title, v)
}

// status feeds the page while files are processed: the info block of every
// file, so the page can update without losing what is being typed.
func (a *App) status(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	v, err := a.subView(r, s)
	if err != nil {
		http.Error(w, "", 500)
		return
	}
	csrf := a.csrfToken(w, r)
	out := map[string]any{"busy": v.Busy, "checked": v.Checked, "total": len(v.Files),
		"pending": v.pendingSuggestions(), "state": s.State, "dups": v.Dups, "files": map[string]string{}}
	files := out["files"].(map[string]string)
	for _, fv := range v.Files {
		var buf bytes.Buffer
		a.tmpl.ExecuteTemplate(&buf, "fileinfo", map[string]any{"V": v, "FV": fv, "Base": a.cfg.BasePath, "CSRF": csrf})
		files[strconv.FormatInt(fv.F.ID, 10)] = buf.String()
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(out)
}

func (a *App) save(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	u := currentUser(r)
	if s.Owner != u.ID || !editable(s) {
		a.fail(w, r, http.StatusForbidden, "Diese Einreichung kann nicht mehr geändert werden.")
		return
	}
	if err := a.saveForm(r, s); err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if r.Header.Get("X-Requested-With") == "fetch" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	a.redirect(w, r, a.url("/e/%d", s.ID), "gespeichert")
}

func (a *App) saveForm(r *http.Request, s *Submission) error {
	credit := r.PostFormValue("credit") == "1"
	t := now()
	if _, err := a.db.Exec(`UPDATE submissions SET title = ?, description = ?, credit = ?,
	    owner_note = ?, updated = ? WHERE id = ?`,
		limit(r.PostFormValue("title"), 200), limit(r.PostFormValue("description"), 5000), credit,
		limit(r.PostFormValue("owner_note"), 2000), t, s.ID); err != nil {
		return err
	}
	files, _ := a.db.filesOf(s.ID)
	for _, f := range files {
		key := "desc_" + strconv.FormatInt(f.ID, 10)
		if _, ok := r.PostForm[key]; !ok {
			continue
		}
		d := limit(r.PostFormValue(key), 1000)
		if d != f.Description {
			// ask the AI again once the typing has stopped for a minute
			a.db.Exec("UPDATE files SET description = ?, suggest_due = ? WHERE id = ?", d, t+60, f.ID)
		}
	}
	a.poke()
	return nil
}

func limit(s string, n int) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n"))
	if r := []rune(s); len(r) > n {
		s = string(r[:n])
	}
	return s
}

func (a *App) submit(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	u := currentUser(r)
	if s.Owner != u.ID || s.State != StDraft {
		a.fail(w, r, http.StatusForbidden, "Diese Einreichung ist schon eingereicht.")
		return
	}
	if err := a.saveForm(r, s); err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	s, _ = a.db.submission(s.ID)
	var n, busy int
	a.db.QueryRow("SELECT COUNT(*), COALESCE(SUM(status IN (?, ?)), 0) FROM files WHERE submission = ?",
		FUploading, FProcessing, s.ID).Scan(&n, &busy)
	switch {
	case s.Title == "":
		a.fail(w, r, http.StatusBadRequest, "Bitte gib der Einreichung einen Titel.")
		return
	case n == 0:
		a.fail(w, r, http.StatusBadRequest, "Die Einreichung enthält noch keine Dateien.")
		return
	case busy > 0:
		a.fail(w, r, http.StatusBadRequest, "Es werden noch Dateien hochgeladen oder geprüft. Bitte warte, bis alle fertig sind.")
		return
	}
	t := now()
	a.db.Exec("UPDATE submissions SET state = ?, submitted = ?, updated = ?, analysis_state = 'pending' WHERE id = ?",
		StAnalysing, t, t, s.ID)
	a.db.logEvent(u.ID, s.ID, "submitted", nil)
	if a.cfg.DirectIntake && u.Moderator {
		// a moderator's own submission stays with them: after the AI they
		// find the form to take it in on its page
		a.db.Exec("UPDATE submissions SET claimed_by = ?, claimed_at = ?, last_activity = ? WHERE id = ?", u.ID, t, t, s.ID)
		a.db.logEvent(u.ID, s.ID, "claimed", nil)
	}
	a.notifyEvent(EvSubmitted, s.ID, u.ID, "")
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "eingereicht")
}

func (a *App) deleteDraft(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	if s.Owner != currentUser(r).ID || s.State != StDraft {
		a.fail(w, r, http.StatusForbidden, "Nur Entwürfe können gelöscht werden.")
		return
	}
	a.purge(s.ID)
	a.db.Exec("DELETE FROM submissions WHERE id = ?", s.ID)
	a.db.logEvent(s.Owner, s.ID, "deleted", nil)
	a.redirect(w, r, a.url("/"), "geloescht")
}

// purge removes the staged files of a submission.
func (a *App) purge(sub int64) {
	files, _ := a.db.files("submission = ?", sub)
	for _, f := range files {
		if f.UploadID.Valid && f.Status == FUploading {
			p := filepath.Join(a.cfg.StagingDir, "tus", f.UploadID.String)
			os.Remove(p)
			os.Remove(p + ".info")
		}
	}
	os.RemoveAll(filepath.Join(a.cfg.StagingDir, "files", strconv.FormatInt(sub, 10)))
}

func (a *App) removeFile(w http.ResponseWriter, r *http.Request) {
	f, s := a.loadOwnFile(w, r)
	if f == nil {
		return
	}
	if !editable(s) {
		a.fail(w, r, http.StatusForbidden, "Diese Einreichung kann nicht mehr geändert werden.")
		return
	}
	a.dropFile(s, f)
	a.db.Exec("UPDATE submissions SET updated = ? WHERE id = ?", now(), s.ID)
	a.redirect(w, r, a.url("/e/%d", s.ID), "entfernt")
}

// removeDuplicates removes every checked file of the submission that is
// already in the archive or in another submission.
func (a *App) removeDuplicates(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	if s.Owner != currentUser(r).ID || !editable(s) {
		a.fail(w, r, http.StatusForbidden, "Diese Einreichung kann nicht mehr geändert werden.")
		return
	}
	files, err := a.db.filesOf(s.ID)
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	for _, f := range files {
		if isDuplicate(f) {
			a.dropFile(s, f)
		}
	}
	a.db.Exec("UPDATE submissions SET updated = ? WHERE id = ?", now(), s.ID)
	a.redirect(w, r, a.url("/e/%d", s.ID), "doubletten")
}

// isDuplicate tells whether a checked file exists already elsewhere.
func isDuplicate(f *File) bool {
	return f.Status != FProcessing && f.Status != FUploading && len(parseDuplicates(f.Duplicates)) > 0
}

// dropFile deletes a file of a submission with its staged data.
func (a *App) dropFile(s *Submission, f *File) {
	os.RemoveAll(filepath.Join(a.cfg.StagingDir, "files", strconv.FormatInt(s.ID, 10), strconv.FormatInt(f.ID, 10)))
	if f.UploadID.Valid {
		p := filepath.Join(a.cfg.StagingDir, "tus", f.UploadID.String)
		os.Remove(p)
		os.Remove(p + ".info")
	}
	a.db.Exec("DELETE FROM files WHERE id = ?", f.ID)
	a.db.logEvent(s.Owner, s.ID, "file removed", f.Name)
}

// adopt takes over the AI's descriptions for a group of files, for those
// that have none yet.
func (a *App) adopt(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	if s.Owner != currentUser(r).ID || !editable(s) {
		a.fail(w, r, http.StatusForbidden, "Diese Einreichung kann nicht mehr geändert werden.")
		return
	}
	want := map[int64]bool{}
	for _, v := range r.PostForm["f"] {
		if id, err := strconv.ParseInt(v, 10, 64); err == nil {
			want[id] = true
		}
	}
	files, _ := a.db.filesOf(s.ID)
	for _, f := range files {
		g := parseSuggestion(f.Suggestion)
		if !want[f.ID] || g == nil {
			continue
		}
		if f.Description == "" && g.Description != "" {
			a.db.Exec("UPDATE files SET description = ? WHERE id = ?", g.Description, f.ID)
		}
	}
	a.redirect(w, r, a.url("/e/%d", s.ID), "vorschlag")
}

func (a *App) answer(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	u := currentUser(r)
	if s.Owner != u.ID || s.State != StQuestion {
		a.fail(w, r, http.StatusForbidden, "Zu dieser Einreichung gibt es keine offene Rückfrage.")
		return
	}
	if err := a.saveForm(r, s); err != nil {
		a.fail(w, r, http.StatusBadRequest, err.Error())
		return
	}
	text := limit(r.PostFormValue("answer"), 5000)
	if text != "" {
		a.db.Exec("INSERT INTO comments (submission, author, time, text, kind) VALUES (?,?,?,?,?)", s.ID, u.ID, now(), text, "answer")
	}
	t := now()
	a.db.Exec(`UPDATE submissions SET state = ?, question = '', updated = ?, analysis_state = 'pending',
	    last_activity = ?, reminded_at = NULL, reminders = 0 WHERE id = ?`, StAnalysing, t, t, s.ID)
	a.db.logEvent(u.ID, s.ID, "answered", text)
	a.notifyEvent(EvAnswered, s.ID, u.ID, text)
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "beantwortet")
}

// ------------------------------------------------------------- file data

func (a *App) mayRead(r *http.Request, s *Submission) bool {
	u := currentUser(r)
	return s.Owner == u.ID || s.State != StDraft
}

func (a *App) image(w http.ResponseWriter, r *http.Request) {
	f, err := a.db.file(pathID(r, "fid"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s, err := a.db.submission(f.Submission)
	which := r.PathValue("which")
	if err != nil || !a.mayRead(r, s) || (which != "thumb" && which != "preview") {
		http.NotFound(w, r)
		return
	}
	p := filepath.Join(a.cfg.StagingDir, "files", strconv.FormatInt(s.ID, 10), strconv.FormatInt(f.ID, 10), which+".jpg")
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeFile(w, r, p)
}

func (a *App) download(w http.ResponseWriter, r *http.Request) {
	f, err := a.db.file(pathID(r, "fid"))
	if err != nil || f.Path == "" {
		http.NotFound(w, r)
		return
	}
	s, err := a.db.submission(f.Submission)
	if err != nil || !a.mayRead(r, s) {
		http.NotFound(w, r)
		return
	}
	fh, err := os.Open(filepath.Join(a.cfg.StagingDir, f.Path))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer fh.Close()
	// never rendered by the browser: an uploaded web page must not run here
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename*=UTF-8''`+strings.ReplaceAll(urlPathEscape(f.Name), "'", "%27"))
	w.Header().Set("Content-Security-Policy", "sandbox")
	http.ServeContent(w, r, "", time.Unix(f.Created, 0), fh)
}

// pendingSuggestions: a draft file is waiting for its AI suggestion.
func (v *SubView) pendingSuggestions() bool {
	if !v.CanEdit || !v.AIOn {
		return false
	}
	for _, fv := range v.Files {
		if fv.F.Status == FReady && (fv.Sugg == nil || fv.F.SuggestDue.Valid) {
			return true
		}
	}
	return false
}

// groupFiles puts files with the same suggestion together, in the order of
// their first file.
func groupFiles(files []FileView) []*GroupView {
	var out []*GroupView
	byKey := map[string]*GroupView{}
	for _, fv := range files {
		key := fv.Sugg.GroupKey()
		if fv.Sugg == nil {
			key = "\x01"
		}
		g := byKey[key]
		if g == nil {
			g = &GroupView{N: len(out), Sugg: fv.Sugg}
			byKey[key] = g
			out = append(out, g)
		}
		g.Files = append(g.Files, fv)
	}
	for _, g := range out {
		// the moderator form starts with the tags the files share, else
		// with the suggestion
		common := g.Files[0].F.Tags
		for _, fv := range g.Files {
			if fv.F.Tags != common {
				common = ""
			}
		}
		switch {
		case common != "":
			g.Tags = tagLines(common)
		case g.Sugg != nil:
			g.Tags = g.Sugg.Tags
		}
		for i, fv := range g.Files {
			if len(fv.Tags) > 0 && !slices.Equal(fv.Tags, g.Tags) {
				g.Files[i].Own = fv.Tags
			}
		}
	}
	return out
}
