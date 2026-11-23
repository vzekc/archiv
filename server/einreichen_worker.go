package archiv

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// worker does everything that takes time, one thing after another: file
// processing, final classification, extension work, intake, reminders and
// cleanup. Suggestions for drafts come from the suggester.
func (a *App) worker(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	var lastHourly, lastDaily time.Time
	for {
		a.step(ctx)
		if time.Since(lastHourly) > time.Hour {
			lastHourly = time.Now()
			a.reminders(ctx)
			if a.cfg.Editing {
				a.queueDigest()
			}
		}
		if time.Since(lastDaily) > 24*time.Hour {
			lastDaily = time.Now()
			a.cleanup()
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-a.wake:
		}
	}
}

func (a *App) step(ctx context.Context) {
	// names first: they show up everywhere
	if d := a.directory(); d != nil {
		if ids := a.names.takeMissing(); len(ids) > 0 {
			if names, err := d.Names(ctx, ids); err != nil {
				logf("user names: %v", err)
			} else {
				for id, n := range names {
					a.names.remember(id, n)
				}
			}
		}
	}

	// uploaded files, all of them: hashing and previews take seconds
	files, _ := a.db.files("status = ? ORDER BY id", FProcessing)
	for _, f := range files {
		if ctx.Err() != nil {
			return
		}
		if err := a.processFile(ctx, f); err != nil {
			logf("process file %d: %v", f.ID, err)
			a.db.Exec("UPDATE files SET status = ?, error = ? WHERE id = ?", FError, "Die Datei konnte nicht gelesen werden.", f.ID)
		}
	}

	// final classification of submitted submissions
	subs, _ := a.db.submissions("analysis_state = 'pending' ORDER BY submitted")
	for _, s := range subs {
		var busy int
		a.db.QueryRow("SELECT COUNT(*) FROM files WHERE submission = ? AND status IN (?, ?)", s.ID, FUploading, FProcessing).Scan(&busy)
		if busy > 0 {
			continue
		}
		state := "done"
		if !a.ai.enabled() {
			state = "skipped"
		} else if err := a.ai.analyse(ctx, s); err != nil {
			logf("analyse submission %d: %v", s.ID, err)
			// give up for now after ten minutes; moderators get it without
			if now()-s.Submitted.Int64 < 600 && s.State == StAnalysing {
				continue
			}
			state = "failed"
		}
		if state != "done" {
			a.db.Exec("UPDATE submissions SET analysis_state = ?, updated = ? WHERE id = ?", state, now(), s.ID)
		}
		if s.State == StAnalysing {
			a.db.Exec("UPDATE submissions SET state = ? WHERE id = ?", StModeration, s.ID)
			a.notifyEvent(EvNew, s.ID, s.Owner, "")
		} else {
			a.notifyEvent(EvAnalysis, s.ID, "", "")
		}
	}
	// failed analyses are retried every half hour while in moderation
	a.db.Exec(`UPDATE submissions SET analysis_state = 'pending' WHERE analysis_state = 'failed'
	    AND state = ? AND updated < ?`, StModeration, now()-1800)

	// accepted submissions
	subs, _ = a.db.submissions("state = ? ORDER BY decided_at", StIntake)
	for _, s := range subs {
		if ctx.Err() != nil {
			return
		}
		a.intake(ctx, s)
	}

	for _, e := range a.exts {
		if w, ok := e.(Worker); ok {
			w.Work(ctx)
		}
	}
	a.runNotify(ctx)
}

// suggestSettle is how long a draft has to be quiet before the AI is asked:
// files that keep arriving push the request back.
const suggestSettle = 30

// suggester asks the AI about drafts, once uploads and typing have settled:
// every file of the draft is checked and the last change is due. One request
// covers the whole draft. It runs beside the worker so suggestions hold up
// neither file processing nor the moderators' jobs.
func (a *App) suggester(ctx context.Context) {
	if !a.ai.enabled() {
		return
	}
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	for {
		var ids []int64
		rows, err := a.db.Query(`SELECT submission FROM files
		    WHERE submission IN (SELECT id FROM submissions WHERE state IN (?, ?))
		    GROUP BY submission
		    HAVING MAX(suggest_due) <= ? AND SUM(status = ?) = 0`, StDraft, StQuestion, now(), FProcessing)
		if err == nil {
			for rows.Next() {
				var id int64
				rows.Scan(&id)
				ids = append(ids, id)
			}
			rows.Close()
		}
		for _, id := range ids {
			if ctx.Err() != nil {
				return
			}
			s, err := a.db.submission(id)
			if err != nil {
				continue
			}
			if err := a.ai.suggestDraft(ctx, s); err != nil {
				logf("suggest submission %d: %v", id, err)
				a.db.Exec("UPDATE files SET suggest_due = ? WHERE submission = ? AND suggest_due IS NOT NULL", now()+600, id)
				continue
			}
			a.db.Exec("UPDATE files SET suggest_due = NULL WHERE submission = ?", id)
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// reminders keep submissions from being forgotten.
func (a *App) reminders(ctx context.Context) {
	if !a.hasNotifier() {
		return
	}
	t := now()
	cfg := a.cfg
	subs, _ := a.db.submissions("state IN (?, ?)", StModeration, StFailed)
	for _, s := range subs {
		if s.ClaimedBy.Valid {
			last := max(s.ClaimedAt.Int64, s.LastActivity.Int64)
			for _, e := range a.exts {
				if src, ok := e.(ActivitySource); ok {
					if p, err := src.LastActivity(ctx, s, s.ClaimedBy.String, last); err == nil && p > last {
						last = p
						a.db.Exec("UPDATE submissions SET last_activity = ? WHERE id = ?", p, s.ID)
					}
				}
			}
			idle := time.Duration(t-last) * time.Second
			switch {
			case idle > cfg.ClaimRelease:
				a.db.Exec("UPDATE submissions SET claimed_by = NULL, claimed_at = NULL, reminded_at = NULL, reminders = 0, last_activity = ? WHERE id = ?", t, s.ID)
				a.db.logEvent("system", s.ID, "auto release", s.ClaimedBy.String)
				a.notifyEvent(EvAutoRelease, s.ID, s.ClaimedBy.String, "")
			case idle > cfg.ClaimReminder && (!s.RemindedAt.Valid || time.Duration(t-s.RemindedAt.Int64)*time.Second > cfg.ClaimReminder):
				a.db.Exec("UPDATE submissions SET reminded_at = ?, reminders = reminders + 1 WHERE id = ?", t, s.ID)
				a.notifyEvent(EvRemindClaim, s.ID, s.ClaimedBy.String, since(last))
			}
			continue
		}
		waiting := max(s.Submitted.Int64, s.LastActivity.Int64)
		if time.Duration(t-waiting)*time.Second > cfg.UnclaimedRemind &&
			(!s.RemindedAt.Valid || time.Duration(t-s.RemindedAt.Int64)*time.Second > cfg.UnclaimedRemind) {
			a.db.Exec("UPDATE submissions SET reminded_at = ?, reminders = reminders + 1 WHERE id = ?", t, s.ID)
			a.notifyEvent(EvRemindUnclaimed, s.ID, "", since(s.Submitted.Int64))
		}
	}
	subs, _ = a.db.submissions("state = ?", StQuestion)
	for _, s := range subs {
		asked := s.LastActivity.Int64
		if time.Duration(t-asked)*time.Second > cfg.QuestionRemind &&
			(!s.RemindedAt.Valid || time.Duration(t-s.RemindedAt.Int64)*time.Second > cfg.QuestionRemind) {
			a.db.Exec("UPDATE submissions SET reminded_at = ?, reminders = reminders + 1 WHERE id = ?", t, s.ID)
			a.notifyEvent(EvRemindQuestion, s.ID, s.Owner, "")
		}
	}
	a.poke()
}

// cleanup removes what is no longer needed: files of rejected submissions
// after 30 days, abandoned drafts after 90 days, broken-off uploads after 7.
func (a *App) cleanup() {
	t := now()
	subs, _ := a.db.submissions("state = ? AND decided_at < ?", StRejected, t-30*86400)
	for _, s := range subs {
		a.purge(s.ID)
	}
	subs, _ = a.db.submissions("state = ? AND updated < ?", StDraft, t-90*86400)
	for _, s := range subs {
		a.purge(s.ID)
		a.db.Exec("DELETE FROM submissions WHERE id = ?", s.ID)
		a.db.logEvent("system", s.ID, "draft expired", nil)
	}
	stale, _ := a.db.files("status = ? AND created < ?", FUploading, t-7*86400)
	for _, f := range stale {
		if f.UploadID.Valid {
			p := filepath.Join(a.cfg.StagingDir, "tus", f.UploadID.String)
			os.Remove(p)
			os.Remove(p + ".info")
		}
		a.db.Exec("DELETE FROM files WHERE id = ?", f.ID)
	}
	// tus leftovers without a file row
	entries, _ := os.ReadDir(filepath.Join(a.cfg.StagingDir, "tus"))
	for _, e := range entries {
		id := strings.TrimSuffix(e.Name(), ".info")
		var n int
		a.db.QueryRow("SELECT COUNT(*) FROM files WHERE upload_id = ?", id).Scan(&n)
		if info, err := e.Info(); n == 0 && err == nil && time.Since(info.ModTime()) > 24*time.Hour {
			os.Remove(filepath.Join(a.cfg.StagingDir, "tus", e.Name()))
		}
	}
}
