package archiv

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
)

// Uploads use the tus protocol (resumable, in chunks), served by tusd as a
// library. Every upload belongs to a file row of a draft; the row is created
// before the first byte is accepted, after checking ownership and quota.

func (a *App) newTus() (*handler.Handler, error) {
	store := filestore.New(filepath.Join(a.cfg.StagingDir, "tus"))
	composer := handler.NewStoreComposer()
	store.UseIn(composer)
	memorylocker.New().UseIn(composer)
	return handler.NewHandler(handler.Config{
		StoreComposer:              composer,
		BasePath:                   a.cfg.BasePath + "/upload/",
		MaxSize:                    0, // checked per member in preCreate
		NotifyCompleteUploads:      true,
		DisableDownload:            true,
		DisableConcatenation:       true,
		RespectForwardedHeaders:    true,
		PreUploadCreateCallback:    a.preCreate,
		PreUploadTerminateCallback: a.preTerminate,
	})
}

func tusError(code, msg string, status int) error {
	return handler.NewError(code, msg, status)
}

func (a *App) preCreate(hook handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
	none := handler.HTTPResponse{}
	u := a.userFrom(&http.Request{Header: hook.HTTPRequest.Header})
	if u == nil {
		return none, handler.FileInfoChanges{}, tusError("ERR_AUTH", "nicht angemeldet", 401)
	}
	info := hook.Upload
	if info.SizeIsDeferred || info.Size <= 0 {
		return none, handler.FileInfoChanges{}, tusError("ERR_SIZE", "Größe unbekannt", 400)
	}
	subID, _ := strconv.ParseInt(info.MetaData["submission"], 10, 64)
	s, err := a.db.submission(subID)
	if err != nil || s.Owner != u.ID {
		return none, handler.FileInfoChanges{}, tusError("ERR_SUBMISSION", "Einreichung nicht gefunden", 404)
	}
	if s.State != StDraft && s.State != StQuestion {
		return none, handler.FileInfoChanges{}, tusError("ERR_STATE", "Diese Einreichung kann nicht mehr geändert werden", 409)
	}
	raw := info.MetaData["filename"]
	if raw == "" {
		raw = info.MetaData["name"]
	}
	name := cleanName(raw)
	if name == "" {
		return none, handler.FileInfoChanges{}, tusError("ERR_NAME", "ungültiger Dateiname", 400)
	}
	if msg := a.overQuota(u.ID, s.ID, info.Size); msg != "" {
		return none, handler.FileInfoChanges{}, tusError("ERR_QUOTA", msg, 413)
	}

	b := make([]byte, 16)
	rand.Read(b)
	id := hex.EncodeToString(b)
	if _, err := a.db.Exec(`INSERT INTO files (submission, name, size, upload_id, status, created)
	    VALUES (?,?,?,?,?,?)`, s.ID, name, info.Size, id, FUploading, now()); err != nil {
		logf("preCreate: %v", err)
		return none, handler.FileInfoChanges{}, tusError("ERR_DB", "interner Fehler", 500)
	}
	a.db.Exec("UPDATE submissions SET updated = ? WHERE id = ?", now(), s.ID)
	return none, handler.FileInfoChanges{ID: id}, nil
}

// overQuota tells why a file of the given size does not fit into the
// member's submission, or "" when it fits.
func (a *App) overQuota(member string, sub, size int64) string {
	q := a.db.quota(a.cfg, member)
	switch {
	case size > q.MaxFile:
		return "Datei größer als " + humanSize(q.MaxFile) + " – bitte einen Moderator fragen"
	case a.db.submissionBytes(sub)+size > q.MaxSubmission:
		return "Einreichung größer als " + humanSize(q.MaxSubmission) + " – bitte einen Moderator fragen"
	case a.db.pendingBytes(member)+size > q.MaxPending:
		return "Kontingent für offene Einreichungen (" + humanSize(q.MaxPending) + ") erschöpft"
	}
	return ""
}

func (a *App) preTerminate(hook handler.HookEvent) (handler.HTTPResponse, error) {
	a.db.Exec("DELETE FROM files WHERE upload_id = ? AND status = ?", hook.Upload.ID, FUploading)
	return handler.HTTPResponse{}, nil
}

// uploadGuard lets only the owner of an upload continue, inspect or cancel it.
func (a *App) uploadGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := currentUser(r)
		if r.Method != http.MethodOptions && r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !a.csrfOK(r) {
				http.Error(w, "Sitzung abgelaufen, bitte Seite neu laden.", http.StatusForbidden)
				return
			}
		}
		id := strings.Trim(strings.TrimPrefix(r.URL.Path, a.cfg.BasePath+"/upload/"), "/")
		if id != "" {
			var owner, status string
			err := a.db.QueryRow(`SELECT s.owner, f.status FROM files f JOIN submissions s ON s.id = f.submission
			    WHERE f.upload_id = ?`, id).Scan(&owner, &status)
			if err != nil || owner != u.ID {
				http.NotFound(w, r)
				return
			}
		}
		// tusd builds the upload URLs it hands out from these headers; they
		// are set to the host the request came in under, if the site answers
		// to it, so a proxy chain that appends its own values cannot turn
		// them into a list.
		pub, _ := url.Parse(a.cfg.BaseURL)
		r.Header.Del("Forwarded")
		r.Header.Set("X-Forwarded-Proto", pub.Scheme)
		r.Header.Set("X-Forwarded-Host", a.cfg.publicHost(r))
		next.ServeHTTP(w, r)
	})
}

// handleCompletedUploads moves finished uploads out of the tus store into the
// submission's folder and queues them for processing.
func (a *App) handleCompletedUploads(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-a.tus.CompleteUploads:
			a.finishUpload(ev.Upload.ID)
		}
	}
}

func (a *App) finishUpload(uploadID string) {
	var fid, sub int64
	var name string
	if err := a.db.QueryRow("SELECT id, submission, name FROM files WHERE upload_id = ? AND status = ?",
		uploadID, FUploading).Scan(&fid, &sub, &name); err != nil {
		logf("finished upload %s: %v", uploadID, err)
		return
	}
	src := filepath.Join(a.cfg.StagingDir, "tus", uploadID)
	rel := filepath.Join("files", strconv.FormatInt(sub, 10), strconv.FormatInt(fid, 10), name)
	dst := filepath.Join(a.cfg.StagingDir, rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o2770); err == nil {
		err = os.Rename(src, dst)
		if err == nil {
			os.Remove(src + ".info")
			a.db.Exec("UPDATE files SET path = ?, status = ? WHERE id = ?", rel, FProcessing, fid)
			a.poke()
			return
		}
		logf("finished upload %s: %v", uploadID, err)
	}
	a.db.Exec("UPDATE files SET status = ?, error = ? WHERE id = ?", FError, "Datei konnte nicht abgelegt werden", fid)
}

// cleanName keeps a file name usable in the archive: no paths, no control
// characters, no leading dots.
func cleanName(n string) string {
	n = strings.ReplaceAll(n, "\\", "/")
	if i := strings.LastIndex(n, "/"); i >= 0 {
		n = n[i+1:]
	}
	n = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == ':' || r == '*' || r == '?' || r == '"' || r == '<' || r == '>' || r == '|' {
			return '_'
		}
		return r
	}, n)
	n = strings.TrimLeft(strings.TrimSpace(n), ".")
	if len(n) > 200 {
		ext := filepath.Ext(n)
		n = n[:200-len(ext)] + ext
	}
	return n
}

// copyIntoDraft stages a copy of the local file src as a file of the draft
// sub, ready for processing. It answers the file's id, or why the copy
// failed.
func (a *App) copyIntoDraft(sub int64, name, src string, size int64) (int64, string) {
	name = cleanName(name)
	if name == "" {
		name = "datei"
	}
	res, err := a.db.Exec(`INSERT INTO files (submission, name, size, status, created) VALUES (?,?,?,?,?)`,
		sub, name, size, FUploading, now())
	if err != nil {
		return 0, "Datenbankfehler"
	}
	fid, _ := res.LastInsertId()
	rel := filepath.Join("files", strconv.FormatInt(sub, 10), strconv.FormatInt(fid, 10), name)
	dst := filepath.Join(a.cfg.StagingDir, rel)
	err = os.MkdirAll(filepath.Dir(dst), 0o2770)
	if err == nil {
		err = copyFile(src, dst)
	}
	if err != nil {
		logf("copy %s into submission %d: %v", src, sub, err)
		return 0, "Die Datei konnte nicht übernommen werden."
	}
	a.db.Exec("UPDATE files SET path = ?, status = ? WHERE id = ?", rel, FProcessing, fid)
	return fid, ""
}
