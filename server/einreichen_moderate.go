package archiv

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

func urlPathEscape(s string) string { return url.PathEscape(s) }

// claim is a plain link (GET), so that one click in a message takes a
// submission over. Taking it over is harmless and can be undone.
func (a *App) claim(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	u := currentUser(r)
	if s.State != StModeration && s.State != StFailed {
		a.redirect(w, r, a.url("/e/%d", s.ID), "")
		return
	}
	t := now()
	res, _ := a.db.Exec(`UPDATE submissions SET claimed_by = ?, claimed_at = ?, last_activity = ?,
	    reminded_at = NULL, reminders = 0 WHERE id = ? AND claimed_by IS NULL`, u.ID, t, t, s.ID)
	if n, _ := res.RowsAffected(); n == 0 {
		notice := "vergeben"
		if s.ClaimedBy.String == u.ID {
			notice = ""
		}
		a.redirect(w, r, a.url("/e/%d", s.ID), notice)
		return
	}
	a.db.logEvent(u.ID, s.ID, "claimed", nil)
	a.notifyEvent(EvClaimed, s.ID, u.ID, "")
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "uebernommen")
}

func (a *App) mustClaim(w http.ResponseWriter, r *http.Request, s *Submission) bool {
	if s.ClaimedBy.String != currentUser(r).ID {
		a.fail(w, r, http.StatusConflict, "Übernimm die Einreichung zuerst.")
		return false
	}
	return true
}

func (a *App) release(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil || !a.mustClaim(w, r, s) {
		return
	}
	a.db.Exec("UPDATE submissions SET claimed_by = NULL, claimed_at = NULL, reminded_at = NULL, reminders = 0 WHERE id = ?", s.ID)
	a.db.logEvent(currentUser(r).ID, s.ID, "released", nil)
	a.notifyEvent(EvReleased, s.ID, currentUser(r).ID, "")
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "zurueckgegeben")
}

func (a *App) comment(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	u := currentUser(r)
	text := limit(r.PostFormValue("text"), 5000)
	if text == "" {
		a.redirect(w, r, a.url("/e/%d", s.ID), "")
		return
	}
	t := now()
	a.db.Exec("INSERT INTO comments (submission, author, time, text, kind) VALUES (?,?,?,?,?)", s.ID, u.ID, t, text, "comment")
	if s.ClaimedBy.String == u.ID {
		a.db.Exec("UPDATE submissions SET last_activity = ?, reminded_at = NULL, reminders = 0 WHERE id = ?", t, s.ID)
	}
	a.db.logEvent(u.ID, s.ID, "comment", nil)
	a.notifyEvent(EvComment, s.ID, u.ID, text)
	a.poke()
	a.redirect(w, r, a.url("/e/%d#kommentare", s.ID), "kommentiert")
}

func (a *App) ask(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil || !a.mustClaim(w, r, s) {
		return
	}
	u := currentUser(r)
	text := limit(r.PostFormValue("text"), 5000)
	if text == "" {
		a.fail(w, r, http.StatusBadRequest, "Bitte schreib die Frage an den Einreicher.")
		return
	}
	t := now()
	a.db.Exec("INSERT INTO comments (submission, author, time, text, kind) VALUES (?,?,?,?,?)", s.ID, u.ID, t, text, "question")
	a.db.Exec(`UPDATE submissions SET state = ?, question = ?, last_activity = ?, updated = ?,
	    reminded_at = NULL, reminders = 0 WHERE id = ?`, StQuestion, text, t, t, s.ID)
	a.db.logEvent(u.ID, s.ID, "question", text)
	a.notifyEvent(EvQuestion, s.ID, u.ID, text)
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "gefragt")
}

func (a *App) reject(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil || !a.mustClaim(w, r, s) {
		return
	}
	u := currentUser(r)
	text := limit(r.PostFormValue("text"), 5000)
	if text == "" {
		a.fail(w, r, http.StatusBadRequest, "Bitte gib eine Begründung an; der Einreicher bekommt sie zu lesen.")
		return
	}
	t := now()
	a.db.Exec(`UPDATE submissions SET state = ?, decision_note = ?, decided_by = ?, decided_at = ?, updated = ?
	    WHERE id = ?`, StRejected, text, u.ID, t, t, s.ID)
	a.db.logEvent(u.ID, s.ID, "rejected", text)
	a.notifyEvent(EvRejected, s.ID, u.ID, text)
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "abgelehnt")
}

func (a *App) reanalyse(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil {
		return
	}
	if s.State != StModeration && s.State != StFailed {
		a.redirect(w, r, a.url("/e/%d", s.ID), "")
		return
	}
	a.db.Exec("UPDATE submissions SET analysis_state = 'pending' WHERE id = ?", s.ID)
	a.db.logEvent(currentUser(r).ID, s.ID, "reanalyse", nil)
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "analyse")
}

// accept takes the submission into the archive: per file whether it is
// included, its tags, its description and whether to unpack it. A file
// carries the tags of its group unless it has tags of its own.
func (a *App) accept(w http.ResponseWriter, r *http.Request) {
	s := a.loadSub(w, r)
	if s == nil || !a.mustClaim(w, r, s) {
		return
	}
	if s.State != StModeration && s.State != StFailed {
		a.fail(w, r, http.StatusConflict, "Diese Einreichung kann gerade nicht aufgenommen werden.")
		return
	}
	u := currentUser(r)
	form := r.PostForm

	files, _ := a.db.filesOf(s.ID)
	type fileDecision struct {
		ID          int64    `json:"id"`
		Take        bool     `json:"take"`
		Unpack      bool     `json:"unpack"`
		Split       bool     `json:"split,omitempty"`
		Tags        []string `json:"tags"`
		Description string   `json:"description"`
	}
	var fds []fileDecision
	taken := 0
	for _, f := range files {
		k := strconv.FormatInt(f.ID, 10)
		tags := formTags(form["ftag_"+k])
		if len(tags) == 0 {
			tags = formTags(form["gtag_"+form.Get("grp_"+k)])
		}
		d := fileDecision{ID: f.ID, Take: form.Get("take_"+k) == "1",
			Unpack: form.Get("unpack_"+k) == "1" || form.Get("unpack_"+k) == "split", Split: form.Get("unpack_"+k) == "split",
			Tags: tags, Description: limit(form.Get("desc_"+k), 1000)}
		if d.Take {
			if f.Status != FReady {
				a.fail(w, r, http.StatusBadRequest, f.Name+" ist noch nicht fertig geprüft.")
				return
			}
			for _, t := range d.Tags {
				if err := a.checkTag(u, t); err != nil {
					a.fail(w, r, http.StatusBadRequest, fmt.Sprintf("%s: %v", f.Name, err))
					return
				}
			}
			taken++
		}
		fds = append(fds, d)
	}
	if taken == 0 {
		a.fail(w, r, http.StatusBadRequest, "Es ist keine Datei zur Aufnahme ausgewählt. Zum Ablehnen nimm „Ablehnen“.")
		return
	}

	tx, err := a.db.Begin()
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	defer tx.Rollback()
	for _, d := range fds {
		status := FReady
		if !d.Take {
			status = FSkipped
		}
		tx.Exec("UPDATE files SET tags = ?, description = ?, unpack = ?, split = ?, status = CASE WHEN status = ? THEN ? ELSE status END WHERE id = ?",
			strings.Join(d.Tags, "\n"), d.Description, d.Unpack, d.Split, FReady, status, d.ID)
	}
	t := now()
	tx.Exec(`UPDATE submissions SET state = ?, decided_by = ?, decided_at = ?, updated = ?, decision_note = ?
	    WHERE id = ?`, StIntake, u.ID, t, t, limit(form.Get("note"), 2000), s.ID)
	if err := tx.Commit(); err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	a.db.logEvent(u.ID, s.ID, "accepted", map[string]any{"files": fds})
	a.poke()
	a.redirect(w, r, a.url("/e/%d", s.ID), "aufgenommen")
}

// formTags are the tags typed into a form's fields, without blanks and
// repetitions, written as the autocompletion writes them.
func formTags(vs []string) []string {
	var out []string
	for _, v := range vs {
		v = strings.TrimSpace(strings.ReplaceAll(v, " > ", " › "))
		if v != "" && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// checkTag tells whether a tag can be given at intake: it exists, or the
// moderator may create it and names its facet ("Rechner: Hersteller ›
// Modell").
func (a *App) checkTag(u *User, t string) error {
	if _, err := a.resolveTag(t); err == nil {
		return nil
	}
	f, rest, ok := strings.Cut(t, ":")
	facet := ""
	for k, l := range facetLabels {
		if ok && (strings.EqualFold(strings.TrimSpace(f), l) || strings.EqualFold(strings.TrimSpace(f), k)) {
			facet = k
		}
	}
	switch {
	case facet == "" || facet == "location" || facet == "group":
		return fmt.Errorf("das Schlagwort „%s“ gibt es nicht; ein neues braucht seine Facette, etwa „Rechner: Hersteller › Modell“", t)
	case !a.mayCreateTags(u):
		return fmt.Errorf("das Schlagwort „%s“ gibt es nicht, und neue legen Moderatoren an", t)
	case strings.TrimSpace(rest) == "":
		return fmt.Errorf("„%s“: der Name fehlt", t)
	}
	return nil
}

// ----------------------------------------------------------------- quotas

type QuotaRow struct {
	Member  string
	Name    string
	Q       Quota
	Pending int64
}

func (a *App) quotas(w http.ResponseWriter, r *http.Request) {
	rows, err := a.db.Query("SELECT member, max_file, max_submission, max_pending, until, set_by FROM quotas ORDER BY member")
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	defer rows.Close()
	var out []QuotaRow
	for rows.Next() {
		var q QuotaRow
		rows.Scan(&q.Member, &q.Q.MaxFile, &q.Q.MaxSubmission, &q.Q.MaxPending, &q.Q.Until, &q.Q.SetBy)
		q.Name = a.names.Name(q.Member)
		q.Pending = a.db.pendingBytes(q.Member)
		out = append(out, q)
	}
	type QuotaPage struct {
		Rows    []QuotaRow
		Default Quota
		Member  string
	}
	a.render(w, r, "quotas", "Kontingente", QuotaPage{Rows: out, Member: r.URL.Query().Get("m"),
		Default: Quota{MaxFile: a.cfg.MaxFile, MaxSubmission: a.cfg.MaxSubmission, MaxPending: a.cfg.MaxPending}})
}

func (a *App) setQuota(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	member := strings.TrimSpace(r.PostFormValue("member"))
	if _, err := strconv.Atoi(member); err != nil {
		a.fail(w, r, http.StatusBadRequest, "Bitte die Benutzer-ID des Mitglieds angeben (die Zahl aus seinem Forenprofil).")
		return
	}
	if r.PostFormValue("reset") == "1" {
		a.db.Exec("DELETE FROM quotas WHERE member = ?", member)
		a.db.logEvent(u.ID, 0, "quota reset", member)
		a.redirect(w, r, a.url("/kontingente"), "kontingent")
		return
	}
	gb := func(k string) (int64, bool) {
		f, err := strconv.ParseFloat(strings.ReplaceAll(r.PostFormValue(k), ",", "."), 64)
		return int64(f * float64(GB)), err == nil && f > 0
	}
	mf, ok1 := gb("max_file")
	ms, ok2 := gb("max_submission")
	mp, ok3 := gb("max_pending")
	if !ok1 || !ok2 || !ok3 {
		a.fail(w, r, http.StatusBadRequest, "Bitte alle drei Grenzen in GB angeben.")
		return
	}
	var until sql.NullInt64
	if d := r.PostFormValue("until"); d != "" {
		t, err := time.ParseInLocation("2006-01-02", d, berlin)
		if err != nil {
			a.fail(w, r, http.StatusBadRequest, "Ungültiges Datum.")
			return
		}
		until = sql.NullInt64{Int64: t.Add(24*time.Hour - time.Second).Unix(), Valid: true}
	}
	a.db.Exec(`INSERT INTO quotas (member, max_file, max_submission, max_pending, until, set_by, set_at)
	    VALUES (?,?,?,?,?,?,?) ON CONFLICT(member) DO UPDATE SET max_file = excluded.max_file,
	    max_submission = excluded.max_submission, max_pending = excluded.max_pending, until = excluded.until,
	    set_by = excluded.set_by, set_at = excluded.set_at`, member, mf, ms, mp, until, u.ID, now())
	a.db.logEvent(u.ID, 0, "quota", map[string]any{"member": member, "file": mf, "submission": ms, "pending": mp, "until": until.Int64})
	a.names.want(member)
	a.redirect(w, r, a.url("/kontingente"), "kontingent")
}
