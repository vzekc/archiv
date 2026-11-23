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

// User is the person behind a request. With auth = header, a proxy in front
// (Apache with mod_auth_openidc, say) has checked the login and passes the
// user's id, name and groups as request headers; the service then listens on
// localhost only, and the proxy removes these headers from what the browser
// sends. With auth = single there is no login: every request is the one user,
// who moderates and edits.
type User struct {
	ID        string
	Groups    []string
	Moderator bool
}

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

func (a *App) userFrom(r *http.Request) *User {
	var id, groups string
	if a.cfg.DevUser != "" {
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
	} else if a.cfg.Auth == "single" {
		id = "1"
		a.names.remember(id, a.cfg.SingleUser)
		groups = a.cfg.ModeratorGrp
	} else {
		id = r.Header.Get(a.cfg.UserHeader)
		groups = r.Header.Get(a.cfg.GroupsHeader)
		if a.cfg.NameHeader != "" {
			if name := strings.TrimSpace(r.Header.Get(a.cfg.NameHeader)); name != "" && id != "" {
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
	return u
}

func (a *App) withUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u := a.userFrom(r)
		if u == nil {
			http.Error(w, "Nicht angemeldet.", http.StatusUnauthorized)
			return
		}
		a.names.want(u.ID)
		a.noteGroups(u)
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
