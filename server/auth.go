package archiv

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// User is the person behind a request.
//
// With auth = local the archive keeps the accounts itself (accounts.go):
// people log in with name and password, and their role decides what they
// may do. With open_read, visitors without an account read like a reader.
//
// With auth = header, a proxy in front (Apache with mod_auth_openidc, say)
// has checked the login and passes the user's id, name, email and groups as
// request headers; the service then listens on localhost only, and the
// proxy removes these headers from what the browser sends. Everyone logged
// in may submit; the moderator group moderates.
type User struct {
	ID        string
	Groups    []string
	Moderator bool
	Role      string // auth = local: reader, submitter or archivar
	Reader    bool   // may only read: the role reader, or a visitor without an account
	Anonymous bool   // a visitor without an account
}

// Roles of auth = local.
const (
	RoleReader    = "reader"
	RoleSubmitter = "submitter"
	RoleArchivar  = "archivar"
)

func (u *User) inGroup(g string) bool {
	if u == nil {
		return false
	}
	for _, x := range u.Groups {
		if x == g {
			return true
		}
	}
	return false
}

type ctxKey int

const userKey ctxKey = 0

// userFrom is the user of a request, or nil when there is none.
func (a *App) userFrom(r *http.Request) *User {
	var id, groups string
	switch {
	case a.cfg.DevUser != "":
		parts := strings.SplitN(a.cfg.DevUser, ":", 3)
		id = parts[0]
		if len(parts) > 1 && parts[1] != "" {
			a.names.remember(id, parts[1])
		}
		if len(parts) > 2 {
			groups = parts[2]
		}
		// tests may act as somebody else
		if h := r.Header.Get("X-Dev-User"); h != "" {
			p := strings.SplitN(h, ":", 2)
			id = p[0]
			if len(p) > 1 {
				groups = p[1]
			}
		}
	case a.cfg.Auth == "local":
		return a.accounts.fromSession(r)
	default:
		id = r.Header.Get(a.cfg.UserHeader)
		groups = r.Header.Get(a.cfg.GroupsHeader)
		if id != "" {
			if name := strings.TrimSpace(r.Header.Get(a.cfg.NameHeader)); a.cfg.NameHeader != "" && name != "" {
				a.names.remember(id, name)
			}
		}
	}
	if id == "" {
		return nil
	}
	u := &User{ID: id}
	for _, g := range strings.Split(groups, ",") {
		if g = strings.TrimSpace(g); g != "" {
			u.Groups = append(u.Groups, g)
			if g == a.cfg.ModeratorGrp {
				u.Moderator = true
			}
		}
	}
	if a.cfg.Auth == "header" && a.cfg.DevUser == "" {
		email := ""
		if a.cfg.EmailHeader != "" {
			email = strings.TrimSpace(r.Header.Get(a.cfg.EmailHeader))
		}
		a.people.seen(u.ID, email, u.Moderator)
	}
	return u
}

// openPaths are reachable without logging in: the login itself, setting a
// password from a link, the first setup, and what these pages need.
func (a *App) openPath(p string) bool {
	for _, pre := range []string{"/anmelden", "/abmelden", "/passwort/", "/einrichten", "/assets/", a.cfg.BasePath + "/static/", "/favicon.ico", "/robots.txt"} {
		if p == pre || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// writerPath tells whether a GET of p is for those who may write: the
// submissions, moderation, the archive browser, messages, user management.
func (a *App) writerPath(p string) bool {
	for _, pre := range []string{a.cfg.BasePath + "/", "/moderation", "/browser", "/api/", "/benutzer", "/nachrichten"} {
		if p == strings.TrimSuffix(pre, "/") || strings.HasPrefix(p, pre) {
			return true
		}
	}
	return false
}

// withUser finds the user of each request and keeps readers to reading.
func (a *App) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := a.userFrom(r)
		local := a.cfg.Auth == "local" && a.cfg.DevUser == ""
		if local && a.accounts.needsSetup() && !a.openPath(r.URL.Path) {
			a.setupNotice(w, r)
			return
		}
		if u == nil {
			if !local {
				http.Error(w, "Nicht angemeldet.", http.StatusUnauthorized)
				return
			}
			u = &User{Reader: true, Anonymous: true}
		}
		read := r.Method == http.MethodGet || r.Method == http.MethodHead
		switch {
		case a.openPath(r.URL.Path):
		case u.Anonymous && (!a.cfg.OpenRead || !read || a.writerPath(r.URL.Path)):
			if read {
				http.Redirect(w, r, "/anmelden?weiter="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
			} else {
				http.Error(w, "Nicht angemeldet.", http.StatusUnauthorized)
			}
			return
		case u.Reader && !u.Anonymous && r.URL.Path == "/konto":
		case u.Reader && (!read || a.writerPath(r.URL.Path)):
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			http.Error(w, "Dafür reicht deine Rolle nicht: Leser können lesen, aber nichts einreichen oder ändern.", http.StatusForbidden)
			return
		}
		if u.ID != "" {
			a.names.want(u.ID)
			a.noteGroups(u)
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

func currentUser(r *http.Request) *User {
	u, _ := r.Context().Value(userKey).(*User)
	return u
}

// CSRF: a random token in a cookie that every form repeats (double submit),
// plus a check that POSTs come from our own pages.
const csrfCookie = "einreichen_csrf"

func (a *App) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) == 32 {
		return c.Value
	}
	b := make([]byte, 16)
	rand.Read(b)
	tok := hex.EncodeToString(b)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: tok, Path: "/",
		HttpOnly: true, Secure: strings.HasPrefix(a.cfg.BaseURL, "https:"), SameSite: http.SameSiteLaxMode})
	return tok
}

func (a *App) csrfOK(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" && origin != "null" {
		o, err := url.Parse(origin)
		if err != nil || !a.cfg.knownHost(o.Host) {
			return false
		}
	}
	c, err := r.Cookie(csrfCookie)
	if err != nil {
		return false
	}
	// the header first: reading the form would consume an upload's body
	form := r.Header.Get("X-CSRF-Token")
	if form == "" {
		form = r.PostFormValue("csrf")
	}
	return len(form) == len(c.Value) && subtle.ConstantTimeCompare([]byte(form), []byte(c.Value)) == 1
}

var seenGroups sync.Map

// noteGroups logs the groups a member arrives with whenever they change, so
// a missing moderator group (stale login session) can be seen in the log.
func (a *App) noteGroups(u *User) {
	g := strings.Join(u.Groups, ",")
	if old, ok := seenGroups.Load(u.ID); !ok || old.(string) != g {
		seenGroups.Store(u.ID, g)
		logf("user %s groups: %s (moderator %v)", u.ID, g, u.Moderator)
	}
}
