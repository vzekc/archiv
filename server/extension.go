package archiv

import (
	"context"
	"database/sql"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
)

// Extension connects the archive to a particular installation, for example
// to a forum that announces submissions and delivers messages. Main gets the
// extensions; each one's Init runs once the databases are open. An extension
// implements whichever of the interfaces below it needs.
type Extension interface {
	Init(a *App) error
}

// Configurer takes settings of the config file that the archive itself does
// not know. It answers whether key is one of its settings.
type Configurer interface {
	Configure(key, value string) (known bool, err error)
}

// Notifier learns what happens to submissions. Notify runs in the request
// that caused the event, right after the change is stored, so it must return
// quickly: an extension that talks to slower systems stores the event and
// delivers it in Work.
type Notifier interface {
	Notify(ev SubmissionEvent)
}

// Event kinds and what User and Text carry.
const (
	EvNew             = "new"              // the submission went to the moderators
	EvAnalysis        = "analysis"         // the AI's suggestions are new
	EvClaimed         = "claimed"          // User took it on
	EvReleased        = "released"         // User gave it back
	EvComment         = "comment"          // User wrote Text on the submission page
	EvQuestion        = "question"         // a moderator asked the submitter Text
	EvAnswered        = "answered"         // the submitter answered Text ("" = resubmitted after changes)
	EvRejected        = "rejected"         // Text is the reason
	EvDone            = "done"             // taken into the archive
	EvFailed          = "failed"           // taking it in failed with Text
	EvSubmitted       = "submitted"        // the submitter sent the draft off (before the AI looks at it)
	EvRemindClaim     = "remind_claim"     // the claim by the claimant has been idle for Text
	EvAutoRelease     = "auto_release"     // the claim by User was given back for being idle
	EvRemindUnclaimed = "remind_unclaimed" // nobody has taken it on for Text
	EvRemindQuestion  = "remind_question"  // the question to the submitter is still open
)

// SubmissionEvent is something that happened to a submission.
type SubmissionEvent struct {
	Kind       string
	Submission int64
	User       string
	Text       string
}

// Messenger delivers messages about the catalog: answers to comments, the
// decisions on proposals, the moderators' daily digest. User "" means all
// moderators. Without a Messenger these messages are not kept.
type Messenger interface {
	Send(ctx context.Context, user, subject, html string) error
}

// Directory knows the display names of user ids the login passes without a
// name.
type Directory interface {
	Names(ctx context.Context, ids []string) (map[string]string, error)
}

// Worker runs in every round of the background worker, about once a minute.
type Worker interface {
	Work(ctx context.Context)
}

// ActivitySource reports when user last worked on a claimed submission
// outside the archive (in a forum thread, say), so that the claim is not
// given back while they are busy with it.
type ActivitySource interface {
	LastActivity(ctx context.Context, s *Submission, user string, since int64) (int64, error)
}

// RouteProvider adds pages to the submissions part of the site. The patterns
// are relative to the base path (e.g. "GET /aus-dem-forum").
type RouteProvider interface {
	Routes(r Router)
}

// Router registers pages for an extension. Page handlers get the logged-in
// user through CurrentUser; Post checks the form's CSRF token first.
type Router interface {
	Page(pattern string, h http.HandlerFunc)
	Post(pattern string, h http.HandlerFunc)
}

// TemplateProvider adds templates. They may define the slots the archive's
// templates leave open: "submission-extra" (in the moderators' panel of a
// submission page; . is the page's view, .S the submission), "submit-help-extra"
// (more ways to submit, a list item on the help page) and
// "submit-help-notify" (how a submitter hears back, on the help page).
type TemplateProvider interface {
	Templates() (files fs.FS, patterns []string, funcs template.FuncMap)
}

// extRouter registers an extension's pages below the base path.
type extRouter struct {
	a   *App
	mux *http.ServeMux
}

func (r extRouter) Page(pattern string, h http.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	r.mux.Handle(method+" "+r.a.cfg.BasePath+path, h)
}

func (r extRouter) Post(pattern string, h http.HandlerFunc) {
	method, path, _ := strings.Cut(pattern, " ")
	r.mux.Handle(method+" "+r.a.cfg.BasePath+path, r.a.post(h))
}

// extensions are the ones Main got.
var extensions []Extension

// notify hands an event to every Notifier.
func (a *App) notifyEvent(kind string, sub int64, user, text string) {
	for _, e := range a.exts {
		if n, ok := e.(Notifier); ok {
			n.Notify(SubmissionEvent{Kind: kind, Submission: sub, User: user, Text: text})
		}
	}
}

func (a *App) messenger() Messenger {
	for _, e := range a.exts {
		if m, ok := e.(Messenger); ok {
			return m
		}
	}
	return nil
}

func (a *App) directory() Directory {
	for _, e := range a.exts {
		if d, ok := e.(Directory); ok {
			return d
		}
	}
	return nil
}

func (a *App) hasNotifier() bool {
	for _, e := range a.exts {
		if _, ok := e.(Notifier); ok {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- for extensions

// Config is the configuration the service runs with.
func (a *App) Config() *Config { return a.cfg }

// SubmissionsDB is the database of the submissions (tables submissions,
// files, events). Extensions may read it and keep tables of their own there.
func (a *App) SubmissionsDB() *sql.DB { return a.db.DB }

// CatalogDB is the database of the catalog. Extensions may keep tables of
// their own there.
func (a *App) CatalogDB() *sql.DB { return a.cat.DB }

// Submission loads a submission; ErrNotFound when there is none.
func (a *App) Submission(id int64) (*Submission, error) { return a.db.submission(id) }

// SubmissionFiles are the files of a submission, by name.
func (a *App) SubmissionFiles(id int64) ([]*File, error) { return a.db.filesOf(id) }

// FileGroups are the files of a submission grouped by the tags the AI or
// the submitter gave them, as the submission page shows them.
func (a *App) FileGroups(id int64) []*GroupView {
	files, _ := a.db.filesOf(id)
	var fvs []FileView
	for _, f := range files {
		fvs = append(fvs, FileView{F: f, Sugg: parseSuggestion(f.Suggestion)})
	}
	return groupFiles(fvs)
}

// Analysis is the AI's view of a submission, or nil.
func (a *App) Analysis(s *Submission) *Analysis { return parseAnalysis(s.Analysis) }

// Duplicates are the archive files a file of a submission duplicates.
func Duplicates(f *File) []Duplicate { return parseDuplicates(f.Duplicates) }

// ItemOfPath is the catalog item an archive path belongs to, or nil.
func (a *App) ItemOfPath(p string) *ItemLink { return a.itemOfPath(p) }

// UserName is the display name of a user id.
func (a *App) UserName(id string) string { return a.names.Name(id) }

// AbsURL is the absolute URL of a page of the submissions part.
func (a *App) AbsURL(format string, args ...any) string { return a.absURL(format, args...) }

// URL is the path of a page of the submissions part.
func (a *App) URL(format string, args ...any) string { return a.url(format, args...) }

// SiteURL is the public address of the site.
func (a *App) SiteURL() string { return siteURL }

// ArchiveURL is the absolute URL of a path below the archive root.
func ArchiveURL(p string) string { return archiveURL(p) }

// Render shows the template name with data in the submissions' layout.
func (a *App) Render(w http.ResponseWriter, r *http.Request, name, title string, data any) {
	a.render(w, r, name, title, data)
}

// Fail shows an error page.
func (a *App) Fail(w http.ResponseWriter, r *http.Request, status int, msg string) {
	a.fail(w, r, status, msg)
}

// AddNotice registers a notice the next page shows after Redirect with key.
func (a *App) AddNotice(key, text string) { notices[key] = text }

// Redirect sends the browser on to to, with a notice key for the next page.
func (a *App) Redirect(w http.ResponseWriter, r *http.Request, to, notice string) {
	a.redirect(w, r, to, notice)
}

// Rows are the list entries of submissions, as the overview shows them.
func (a *App) Rows(subs []*Submission) []SubRow { return a.rows(subs) }

// Submissions lists the submissions matching where.
func (a *App) Submissions(where string, args ...any) ([]*Submission, error) {
	return a.db.submissions(where, args...)
}

// CreateDraft starts a new submission of owner and answers its id.
func (a *App) CreateDraft(owner, title, description string) (int64, error) {
	t := now()
	res, err := a.db.Exec(`INSERT INTO submissions (owner, title, description, state, created, updated)
	    VALUES (?,?,?,?,?,?)`, owner, limit(title, 200), description, StDraft, t, t)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AddFileCopy copies the local file src into the draft sub under name, ready
// for processing. It answers the new file's id, or why it could not be
// added (quota, copy failed).
func (a *App) AddFileCopy(sub int64, owner, name, src string, size int64) (int64, string) {
	if msg := a.overQuota(owner, sub, size); msg != "" {
		return 0, msg
	}
	return a.copyIntoDraft(sub, name, src, size)
}

// DeleteDraft removes a draft and its files.
func (a *App) DeleteDraft(sub int64) {
	a.purge(sub)
	a.db.Exec("DELETE FROM submissions WHERE id = ?", sub)
}

// LogEvent writes into a submission's history.
func (a *App) LogEvent(user string, sub int64, what string, data any) {
	a.db.logEvent(user, sub, what, data)
}

// Poke wakes the background worker.
func (a *App) Poke() { a.poke() }

// CurrentUser is the user of a request, or nil.
func CurrentUser(r *http.Request) *User { return currentUser(r) }

// Helpers for extensions that write messages.

func Limit(s string, n int) string      { return limit(s, n) }
func LimitBytes(s string, n int) string { return limitBytes(s, n) }
func FmtTime(t int64) string            { return fmtTime(t) }
func FmtDate(t int64) string            { return fmtDate(t) }
func Plural(n int, one, many string) string {
	return plural(n, one, many)
}
func Now() int64                      { return now() }
func Logf(format string, args ...any) { logf(format, args...) }
