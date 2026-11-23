package archiv

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

// browser keeps the cookies of one person across requests and sends the
// CSRF token with every form, as the pages do.
type browser struct {
	t       *testing.T
	h       http.Handler
	cookies map[string]string
}

func newBrowser(t *testing.T, h http.Handler) *browser {
	return &browser{t: t, h: h, cookies: map[string]string{}}
}

func (b *browser) do(r *http.Request) *httptest.ResponseRecorder {
	for k, v := range b.cookies {
		r.AddCookie(&http.Cookie{Name: k, Value: v})
	}
	w := httptest.NewRecorder()
	b.h.ServeHTTP(w, r)
	for _, c := range w.Result().Cookies() {
		if c.MaxAge < 0 {
			delete(b.cookies, c.Name)
		} else {
			b.cookies[c.Name] = c.Value
		}
	}
	return w
}

func (b *browser) get(path string) *httptest.ResponseRecorder {
	return b.do(httptest.NewRequest("GET", path, nil))
}

func (b *browser) post(path string, v url.Values) *httptest.ResponseRecorder {
	if b.cookies[csrfCookie] == "" {
		b.cookies[csrfCookie] = strings.Repeat("cd", 16)
	}
	v.Set("csrf", b.cookies[csrfCookie])
	r := httptest.NewRequest("POST", path, strings.NewReader(v.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return b.do(r)
}

// localApp is an archive with auth = local and no accounts yet.
func localApp(t *testing.T) *App {
	a := testApp(t)
	a.cfg.DevUser = ""
	a.cfg.Auth = "local"
	ac, err := newAccounts(a)
	if err != nil {
		t.Fatal(err)
	}
	a.accounts = ac
	return a
}

var linkRE = regexp.MustCompile(`/passwort/([0-9a-f]{64})`)

// account makes an account with a password and logs it in.
func account(t *testing.T, a *App, login, role string) *browser {
	t.Helper()
	acc, err := a.accounts.create(login, strings.ToUpper(login[:1])+login[1:], login+"@example.org", role)
	if err != nil {
		t.Fatal(err)
	}
	a.accounts.setPassword(acc.ID, "geheim-"+login+"-1234")
	b := newBrowser(t, a.routes())
	if w := b.post("/anmelden", url.Values{"login": {login}, "password": {"geheim-" + login + "-1234"}, "weiter": {"/"}}); w.Code != 303 {
		t.Fatalf("login %s: %d", login, w.Code)
	}
	return b
}

// The first start: every page asks for the setup; the setup link from the
// log creates the first archivar and logs them in.
func TestSetup(t *testing.T) {
	a := localApp(t)
	b := newBrowser(t, a.routes())
	if w := b.get("/"); w.Code != 503 || !strings.Contains(w.Body.String(), "noch nicht eingerichtet") {
		t.Errorf("before setup: %d", w.Code)
	}
	if w := b.get("/einrichten?schluessel=falsch"); w.Code != 503 {
		t.Errorf("wrong key: %d", w.Code)
	}
	key := a.accounts.setupToken
	if w := b.get("/einrichten?schluessel=" + key); w.Code != 200 || !strings.Contains(w.Body.String(), "Archiv einrichten") {
		t.Fatalf("setup page: %d", w.Code)
	}
	if w := b.post("/einrichten", url.Values{"schluessel": {key}, "login": {"erika"}, "password": {"kurz"}, "password2": {"kurz"}}); w.Code != 400 {
		t.Errorf("short password: %d", w.Code)
	}
	w := b.post("/einrichten", url.Values{"schluessel": {key}, "login": {"erika"}, "name": {"Erika Muster"},
		"password": {"ein-langes-passwort"}, "password2": {"ein-langes-passwort"}})
	if w.Code != 303 {
		t.Fatalf("setup: %d %s", w.Code, w.Body.String())
	}
	if w := b.get("/benutzer"); w.Code != 200 || !strings.Contains(w.Body.String(), "Erika Muster") {
		t.Errorf("users page after setup: %d", w.Code)
	}
	if w := b.post("/einrichten", url.Values{"schluessel": {key}, "login": {"mallory"}, "password": {"ein-langes-passwort"}, "password2": {"ein-langes-passwort"}}); w.Code == 303 && w.Header().Get("Location") == "/benutzer?ok=eingerichtet" {
		t.Errorf("setup ran twice")
	}
}

// An archivar creates an account; its owner sets the password with the
// link and is logged in.
func TestInviteAndLogin(t *testing.T) {
	a := localApp(t)
	arch := account(t, a, "erika", RoleArchivar)
	w := arch.post("/benutzer", url.Values{"login": {"max"}, "name": {"Max"}, "role": {RoleSubmitter}})
	m := linkRE.FindStringSubmatch(w.Body.String())
	if w.Code != 200 || m == nil {
		t.Fatalf("create: %d, link %v", w.Code, m)
	}
	if w := arch.post("/benutzer", url.Values{"login": {"MAX"}, "role": {RoleReader}}); w.Code != 400 {
		t.Errorf("duplicate login (case): %d", w.Code)
	}
	b := newBrowser(t, a.routes())
	if w := b.get("/passwort/" + m[1]); w.Code != 200 || !strings.Contains(w.Body.String(), "max") {
		t.Errorf("set password page: %d", w.Code)
	}
	if w := b.post("/passwort/"+m[1], url.Values{"password": {"max-passwort-1"}, "password2": {"max-passwort-1"}}); w.Code != 303 {
		t.Fatalf("set password: %d", w.Code)
	}
	if w := b.get("/einreichen/"); w.Code != 200 || !strings.Contains(w.Body.String(), "Max") {
		t.Errorf("submitter at the submissions: %d", w.Code)
	}
	if w := b.get("/passwort/" + m[1]); w.Code != 404 {
		t.Errorf("link used twice: %d", w.Code)
	}
	// logout and in again
	b.post("/abmelden", url.Values{})
	if w := b.get("/einreichen/"); w.Code != 303 || !strings.HasPrefix(w.Header().Get("Location"), "/anmelden") {
		t.Errorf("after logout: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := b.post("/anmelden", url.Values{"login": {"max"}, "password": {"falsch"}}); w.Code != 401 {
		t.Errorf("wrong password: %d", w.Code)
	}
	if w := b.post("/anmelden", url.Values{"login": {"Max"}, "password": {"max-passwort-1"}, "weiter": {"/einreichen/"}}); w.Code != 303 || w.Header().Get("Location") != "/einreichen/" {
		t.Errorf("login: %d %s", w.Code, w.Header().Get("Location"))
	}
	if w := b.post("/anmelden", url.Values{"login": {"max"}, "password": {"max-passwort-1"}, "weiter": {"//evil.example"}}); w.Header().Get("Location") != "/" {
		t.Errorf("login sends off-site: %s", w.Header().Get("Location"))
	}
}

func TestLoginThrottle(t *testing.T) {
	a := localApp(t)
	account(t, a, "erika", RoleArchivar)
	b := newBrowser(t, a.routes())
	for range 5 {
		b.post("/anmelden", url.Values{"login": {"erika"}, "password": {"falsch"}})
	}
	if w := b.post("/anmelden", url.Values{"login": {"erika"}, "password": {"geheim-erika-1234"}}); w.Code != 429 {
		t.Errorf("sixth try: %d", w.Code)
	}
}

// What each role may do, and visitors without an account.
func TestRoles(t *testing.T) {
	a := localApp(t)
	arch := account(t, a, "erika", RoleArchivar)
	sub := account(t, a, "max", RoleSubmitter)
	reader := account(t, a, "lena", RoleReader)
	anon := newBrowser(t, a.routes())

	cases := []struct {
		b    *browser
		path string
		code int
	}{
		{reader, "/", 200},
		{reader, "/suche?q=x", 200},
		{reader, "/einreichen/", 403},
		{reader, "/benutzer", 403},
		{reader, "/konto", 200},
		{sub, "/einreichen/", 200},
		{sub, "/benutzer", 403},
		{sub, "/einreichen/moderation", 403},
		{arch, "/einreichen/moderation", 200},
		{arch, "/benutzer", 200},
		{anon, "/", 303},
		{anon, "/anmelden", 200},
	}
	for _, c := range cases {
		if w := c.b.get(c.path); w.Code != c.code {
			t.Errorf("%s: %d, want %d", c.path, w.Code, c.code)
		}
	}
	if w := reader.post("/einreichen/neu", url.Values{"title": {"x"}}); w.Code != 403 {
		t.Errorf("reader submits: %d", w.Code)
	}
	if body := reader.get("/").Body.String(); strings.Contains(body, `href="/einreichen/"`) {
		t.Errorf("reader offered the submissions")
	}

	// open read access: visitors read, but nothing else
	a.cfg.OpenRead = true
	if w := anon.get("/"); w.Code != 200 || !strings.Contains(w.Body.String(), "Anmelden") {
		t.Errorf("open read, start page: %d", w.Code)
	}
	if w := anon.get("/einreichen/"); w.Code != 303 {
		t.Errorf("open read, submissions: %d", w.Code)
	}
	if w := anon.post("/einreichen/neu", url.Values{"title": {"x"}}); w.Code != 401 {
		t.Errorf("open read, submit: %d", w.Code)
	}
}

// The last archivar cannot lose the role; a locked account cannot log in.
func TestUserManagement(t *testing.T) {
	a := localApp(t)
	arch := account(t, a, "erika", RoleArchivar)
	if w := arch.post("/benutzer/1", url.Values{"name": {"Erika"}, "role": {RoleReader}}); w.Code != 400 {
		t.Errorf("last archivar demoted: %d", w.Code)
	}
	other := account(t, a, "max", RoleSubmitter)
	if w := arch.post("/benutzer/2", url.Values{"name": {"Max"}, "role": {RoleSubmitter}, "disabled": {"1"}}); w.Code != 303 {
		t.Fatalf("lock: %d", w.Code)
	}
	if w := other.get("/einreichen/"); w.Code != 303 {
		t.Errorf("locked account still logged in: %d", w.Code)
	}
	if w := newBrowser(t, a.routes()).post("/anmelden", url.Values{"login": {"max"}, "password": {"geheim-max-1234"}}); w.Code != 401 {
		t.Errorf("locked account logs in: %d", w.Code)
	}
	w := arch.post("/benutzer/2/link", url.Values{})
	if !linkRE.MatchString(w.Body.String()) {
		t.Errorf("no new link")
	}
}

// With intake = direct an archivar's own submission stays with them for
// taking in.
func TestDirectIntake(t *testing.T) {
	a := localApp(t)
	a.cfg.DirectIntake = true
	arch := account(t, a, "erika", RoleArchivar)
	res, err := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated) VALUES ('1', 'Fotos', ?, 1, 1)", StDraft)
	if err != nil {
		t.Fatal(err)
	}
	sub, _ := res.LastInsertId()
	a.db.Exec(`INSERT INTO files (submission, name, size, path, sha256, kind, status, created)
	    VALUES (?, 'a.jpg', 1, 'files/x/a.jpg', 'aa', 'image', ?, 1)`, sub, FReady)
	if w := arch.post(fmt.Sprintf("/einreichen/e/%d/einreichen", sub), url.Values{"title": {"Fotos"}}); w.Code != 303 {
		t.Fatalf("submit: %d %s", w.Code, w.Body.String())
	}
	s, _ := a.db.submission(sub)
	if s.ClaimedBy.String != "1" {
		t.Errorf("own submission not kept for taking in: claimed by %q", s.ClaimedBy.String)
	}
}

// auth = header: id, name, email and groups come from the proxy's headers.
func TestHeaderAuth(t *testing.T) {
	a := testApp(t)
	a.cfg.DevUser = ""
	a.cfg.Auth = "header"
	h := a.routes()
	if w := get(t, h, "/einreichen/"); w.Code != 401 {
		t.Errorf("without login: %d", w.Code)
	}
	r := httptest.NewRequest("GET", "/einreichen/moderation", nil)
	r.Header.Set("OIDC_CLAIM_sub", "42")
	r.Header.Set("OIDC_CLAIM_preferred_username", "Erika")
	r.Header.Set("OIDC_CLAIM_email", "erika@example.org")
	r.Header.Set("OIDC_CLAIM_groups", "Mitglieder, Moderatoren")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "Erika") {
		t.Errorf("with login: %d, name shown: %v", w.Code, strings.Contains(w.Body.String(), "Erika"))
	}
	if got := a.people.moderators(); len(got) != 1 || got[0] != "42" {
		t.Errorf("moderators: %v", got)
	}
	if got := a.people.mailTo("42"); got != "erika@example.org" {
		t.Errorf("email: %q", got)
	}
}

// Without an extension, the archive tells the people concerned itself.
func TestMailbox(t *testing.T) {
	a := localApp(t)
	if a.mailbox == nil {
		t.Fatal("no built-in mailbox")
	}
	arch := account(t, a, "erika", RoleArchivar)
	sub := account(t, a, "max", RoleSubmitter)
	a.cfg.SMTPHost, a.cfg.MailFrom = "mail.example.org", "archiv@example.org"
	res, _ := a.db.Exec("INSERT INTO submissions (owner, title, state, created, updated, submitted) VALUES ('2', 'Kaypro-Handbücher', ?, 1, 1, 1)", StModeration)
	id, _ := res.LastInsertId()
	a.notifyEvent(EvNew, id, "2", "")
	a.notifyEvent(EvQuestion, id, "1", "Welches Baujahr?")

	if n := a.unread("1"); n != 1 {
		t.Errorf("archivar: %d unread", n)
	}
	if n := a.unread("2"); n != 2 {
		t.Errorf("submitter: %d unread", n)
	}
	body := sub.get("/nachrichten").Body.String()
	for _, want := range []string{"Eingegangen: Kaypro-Handbücher", "Welches Baujahr?"} {
		if !strings.Contains(body, want) {
			t.Errorf("inbox: %q missing", want)
		}
	}
	if n := a.unread("2"); n != 0 {
		t.Errorf("shown messages still unread: %d", n)
	}
	if !strings.Contains(arch.get("/").Body.String(), `Nachrichten <b class="unread">1</b>`) {
		t.Errorf("unread count not in the header")
	}
	var mails int
	a.db.QueryRow("SELECT COUNT(*) FROM mail_jobs").Scan(&mails)
	if mails != 3 {
		t.Errorf("%d mails queued", mails)
	}
	var text string
	a.db.QueryRow("SELECT body FROM mail_jobs WHERE to_addr = 'max@example.org' ORDER BY id DESC").Scan(&text)
	if !strings.Contains(text, "Welches Baujahr?") || strings.Contains(text, "<p>") || !strings.Contains(text, "/einreichen/e/") {
		t.Errorf("mail text:\n%s", text)
	}
	// who switched email off gets none
	a.people.setMailOff("2", true)
	a.notifyEvent(EvRejected, id, "1", "doppelt")
	var after int
	a.db.QueryRow("SELECT COUNT(*) FROM mail_jobs").Scan(&after)
	if after != mails {
		t.Errorf("mail despite mail_off")
	}
}

func TestPasswordHash(t *testing.T) {
	h := hashPassword("richtig-und-lang")
	if !checkPassword(h, "richtig-und-lang") || checkPassword(h, "falsch") || checkPassword("", "") {
		t.Errorf("password check")
	}
}
