package archiv

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Accounts are the people of an archive with auth = local: each has a login
// name, a password and a role. Archivars create accounts and hand out a
// one-time link with which the person sets their password; the first
// archivar is created on the setup page, whose link the service writes into
// its log at the start (or with "archiv user add"). A login lasts 30 days
// from the last visit.
type Accounts struct {
	a          *App
	db         *DB
	setupToken string
	set        atomic.Bool // there is at least one account

	mu       sync.Mutex
	failures map[string][]time.Time // failed logins per address and per login name
}

const accountSchema = `
CREATE TABLE IF NOT EXISTS accounts (
    id        INTEGER PRIMARY KEY,
    login     TEXT NOT NULL UNIQUE COLLATE NOCASE,
    name      TEXT NOT NULL,
    email     TEXT NOT NULL DEFAULT '',
    role      TEXT NOT NULL,              -- reader, submitter, archivar
    password  TEXT NOT NULL DEFAULT '',   -- pbkdf2-sha256$iterations$salt$key, '' until set
    disabled  INTEGER NOT NULL DEFAULT 0,
    created   INTEGER NOT NULL,
    last_login INTEGER
);
CREATE TABLE IF NOT EXISTS sessions (
    token    TEXT PRIMARY KEY,            -- sha256 of the cookie's value
    account  INTEGER NOT NULL REFERENCES accounts ON DELETE CASCADE,
    created  INTEGER NOT NULL,
    expires  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS password_links (
    token    TEXT PRIMARY KEY,            -- sha256 of the link's token
    account  INTEGER NOT NULL REFERENCES accounts ON DELETE CASCADE,
    expires  INTEGER NOT NULL
);
`

const (
	sessionCookie   = "archiv_sitzung"
	sessionLifetime = 30 * 24 * time.Hour
	linkLifetime    = 7 * 24 * time.Hour
)

// pbkdf2Rounds is the work factor of new password hashes.
var pbkdf2Rounds = 600000

var accountRoleLabels = map[string]string{RoleReader: "Leser", RoleSubmitter: "Einreicher", RoleArchivar: "Archivar"}

func newAccounts(a *App) (*Accounts, error) {
	if _, err := a.db.Exec(accountSchema); err != nil {
		return nil, err
	}
	ac := &Accounts{a: a, db: a.db, failures: map[string][]time.Time{}}
	var n int
	a.db.QueryRow("SELECT COUNT(*) FROM accounts").Scan(&n)
	ac.set.Store(n > 0)
	if a.cfg.Auth == "local" && n == 0 {
		ac.setupToken = randomToken()
		logf("Das Archiv hat noch keine Konten. Einrichten unter %s/einrichten?schluessel=%s", a.cfg.SiteURL, ac.setupToken)
	}
	return ac, nil
}

func (ac *Accounts) needsSetup() bool { return !ac.set.Load() }

// Account is one account as the pages show it.
type Account struct {
	ID        int64
	Login     string
	Name      string
	Email     string
	Role      string
	Disabled  bool
	HasPass   bool
	Created   int64
	LastLogin sql.NullInt64
}

func (acc *Account) RoleLabel() string { return accountRoleLabels[acc.Role] }

const accountCols = "id, login, name, email, role, disabled, password != '', created, last_login"

func scanAccount(row interface{ Scan(...any) error }) (*Account, error) {
	acc := &Account{}
	err := row.Scan(&acc.ID, &acc.Login, &acc.Name, &acc.Email, &acc.Role, &acc.Disabled, &acc.HasPass, &acc.Created, &acc.LastLogin)
	return acc, err
}

func (ac *Accounts) byID(id int64) (*Account, error) {
	return scanAccount(ac.db.QueryRow("SELECT "+accountCols+" FROM accounts WHERE id = ?", id))
}

func (ac *Accounts) byLogin(login string) (*Account, error) {
	return scanAccount(ac.db.QueryRow("SELECT "+accountCols+" FROM accounts WHERE login = ?", strings.TrimSpace(login)))
}

func (ac *Accounts) list() []*Account {
	rows, err := ac.db.Query("SELECT " + accountCols + " FROM accounts ORDER BY disabled, name COLLATE NOCASE")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*Account
	for rows.Next() {
		if acc, err := scanAccount(rows); err == nil {
			out = append(out, acc)
		}
	}
	return out
}

// user is the User an account logs in as.
func (ac *Accounts) user(acc *Account) *User {
	u := &User{ID: strconv.FormatInt(acc.ID, 10), Role: acc.Role, Reader: acc.Role == RoleReader}
	if acc.Role == RoleArchivar {
		u.Moderator = true
		for _, g := range []string{ac.a.cfg.ModeratorGrp, ac.a.cfg.EditGroup, ac.a.cfg.TagCreateGrp} {
			if !u.inGroup(g) {
				u.Groups = append(u.Groups, g)
			}
		}
	}
	return u
}

// publish makes the account's name, email and role known to the rest of the
// archive (names, messages).
func (ac *Accounts) publish(acc *Account) {
	id := strconv.FormatInt(acc.ID, 10)
	ac.a.names.remember(id, acc.Name)
	ac.a.people.set(id, acc.Name, acc.Email, acc.Role == RoleArchivar && !acc.Disabled)
}

// create makes an account; the password is set through a link.
func (ac *Accounts) create(login, name, email, role string) (*Account, error) {
	login, name, email = strings.TrimSpace(login), strings.TrimSpace(name), strings.TrimSpace(email)
	switch {
	case login == "" || strings.ContainsAny(login, " \t/"):
		return nil, errors.New("Der Anmeldename darf nicht leer sein und keine Leerzeichen enthalten.")
	case accountRoleLabels[role] == "":
		return nil, errors.New("Unbekannte Rolle.")
	}
	if name == "" {
		name = login
	}
	res, err := ac.db.Exec("INSERT INTO accounts (login, name, email, role, created) VALUES (?, ?, ?, ?, ?)", login, name, email, role, now())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, errors.New("Diesen Anmeldenamen gibt es schon.")
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	ac.set.Store(true)
	acc, err := ac.byID(id)
	if err == nil {
		ac.publish(acc)
	}
	return acc, err
}

// archivars counts the active archivars.
func (ac *Accounts) archivars() int {
	var n int
	ac.db.QueryRow("SELECT COUNT(*) FROM accounts WHERE role = ? AND disabled = 0", RoleArchivar).Scan(&n)
	return n
}

// ---------------------------------------------------------------- passwords

func hashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, _ := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Rounds, 32)
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", pbkdf2Rounds, hex.EncodeToString(salt), hex.EncodeToString(key))
}

func checkPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	rounds, err := strconv.Atoi(parts[1])
	salt, err1 := hex.DecodeString(parts[2])
	want, err2 := hex.DecodeString(parts[3])
	if err != nil || err1 != nil || err2 != nil {
		return false
	}
	key, err := pbkdf2.Key(sha256.New, pw, salt, rounds, len(want))
	return err == nil && subtle.ConstantTimeCompare(key, want) == 1
}

func passwordProblem(pw string) string {
	if len([]rune(pw)) < 10 {
		return "Das Passwort braucht mindestens 10 Zeichen."
	}
	return ""
}

func (ac *Accounts) setPassword(id int64, pw string) {
	ac.db.Exec("UPDATE accounts SET password = ? WHERE id = ?", hashPassword(pw), id)
	// a new password ends all logins
	ac.db.Exec("DELETE FROM sessions WHERE account = ?", id)
}

func randomToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func tokenHash(t string) string {
	h := sha256.Sum256([]byte(t))
	return hex.EncodeToString(h[:])
}

// passwordLink makes a one-time link with which the account's owner sets
// a password; it replaces earlier links.
func (ac *Accounts) passwordLink(id int64) string {
	t := randomToken()
	ac.db.Exec("DELETE FROM password_links WHERE account = ?", id)
	ac.db.Exec("INSERT INTO password_links (token, account, expires) VALUES (?, ?, ?)", tokenHash(t), id, now()+int64(linkLifetime.Seconds()))
	return ac.a.cfg.SiteURL + "/passwort/" + t
}

func (ac *Accounts) linkAccount(t string) *Account {
	var id int64
	if ac.db.QueryRow("SELECT account FROM password_links WHERE token = ? AND expires > ?", tokenHash(t), now()).Scan(&id) != nil {
		return nil
	}
	acc, err := ac.byID(id)
	if err != nil || acc.Disabled {
		return nil
	}
	return acc
}

// ---------------------------------------------------------------- sessions

func (ac *Accounts) fromSession(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil
	}
	h := tokenHash(c.Value)
	var id, expires int64
	if ac.db.QueryRow("SELECT account, expires FROM sessions WHERE token = ? AND expires > ?", h, now()).Scan(&id, &expires) != nil {
		return nil
	}
	acc, err := ac.byID(id)
	if err != nil || acc.Disabled {
		return nil
	}
	// the login lasts from the last visit; renewed at most once a day
	if fresh := now() + int64(sessionLifetime.Seconds()); fresh-expires > 24*3600 {
		ac.db.Exec("UPDATE sessions SET expires = ? WHERE token = ?", fresh, h)
	}
	return ac.user(acc)
}

func (ac *Accounts) startSession(w http.ResponseWriter, r *http.Request, acc *Account) {
	t := randomToken()
	ac.db.Exec("DELETE FROM sessions WHERE expires < ?", now())
	ac.db.Exec("INSERT INTO sessions (token, account, created, expires) VALUES (?, ?, ?, ?)", tokenHash(t), acc.ID, now(), now()+int64(sessionLifetime.Seconds()))
	ac.db.Exec("UPDATE accounts SET last_login = ? WHERE id = ?", now(), acc.ID)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode,
		Secure: secureRequest(r, ac.a.cfg), MaxAge: int(sessionLifetime.Seconds())})
}

func (ac *Accounts) endSession(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		ac.db.Exec("DELETE FROM sessions WHERE token = ?", tokenHash(c.Value))
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", HttpOnly: true, MaxAge: -1})
}

func secureRequest(r *http.Request, c *Config) bool {
	return r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" || strings.HasPrefix(c.SiteURL, "https:")
}

// clientIP is the address a request came from; behind a proxy on the same
// machine, the one the proxy names.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			parts := strings.Split(f, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}

// ---------------------------------------------------------------- throttling

// blocked tells whether logins from this address or for this name have
// failed five times in the last 15 minutes.
func (ac *Accounts) blocked(keys ...string) bool {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	cut := time.Now().Add(-15 * time.Minute)
	for _, k := range keys {
		var keep []time.Time
		for _, t := range ac.failures[k] {
			if t.After(cut) {
				keep = append(keep, t)
			}
		}
		ac.failures[k] = keep
		if len(keep) >= 5 {
			return true
		}
	}
	return false
}

func (ac *Accounts) failed(keys ...string) {
	ac.mu.Lock()
	defer ac.mu.Unlock()
	for _, k := range keys {
		ac.failures[k] = append(ac.failures[k], time.Now())
	}
}

// ---------------------------------------------------------------- pages

func (a *App) accountRoutes(mux *http.ServeMux) {
	page := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	page("GET /anmelden", a.loginPage)
	page("POST /anmelden", a.post(a.login))
	page("POST /abmelden", a.post(a.logout))
	page("GET /einrichten", a.setupPage)
	page("POST /einrichten", a.post(a.setup))
	page("GET /passwort/{token}", a.setPasswordPage)
	page("POST /passwort/{token}", a.post(a.setPasswordFromLink))
	page("POST /passwort-vergessen", a.post(a.forgotPassword))
	page("GET /konto", a.accountPage)
	page("POST /konto", a.post(a.saveAccount))
	page("GET /benutzer", a.archivar(a.usersPage))
	page("POST /benutzer", a.archivar(a.post(a.createUser)))
	page("GET /benutzer/{id}", a.archivar(a.userPage))
	page("POST /benutzer/{id}", a.archivar(a.post(a.saveUser)))
	page("POST /benutzer/{id}/link", a.archivar(a.post(a.newPasswordLink)))
}

// archivar lets only archivars of an archive with auth = local through.
func (a *App) archivar(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.cfg.Auth != "local" {
			a.fail(w, r, http.StatusNotFound, "Die Benutzer verwaltet hier der Anmeldedienst.")
			return
		}
		if u := currentUser(r); u == nil || u.Role != RoleArchivar {
			a.fail(w, r, http.StatusForbidden, "Das dürfen nur Archivare.")
			return
		}
		h(w, r)
	}
}

func (a *App) setupNotice(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusServiceUnavailable)
	fmt.Fprint(w, `<!doctype html><html lang="de"><head><meta charset="utf-8"><title>Einrichtung</title>
<link rel="stylesheet" href="/assets/oc.css"></head><body><div class="wrap"><h1>Das Archiv ist noch nicht eingerichtet</h1>
<p>Es gibt noch kein Konto. Den Link zur Einrichtung schreibt der Dienst beim Start in sein Protokoll
(bei Docker: <code>docker compose logs archiv</code>). Alternativ legt
<code>archiv user add -login NAME -role archivar</code> das erste Konto an und nennt den Link, mit dem es sein Passwort bekommt.</p>
</div></body></html>`)
}

// safeNext is where to go after the login: a path of this site, else the start page.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/") && !strings.HasPrefix(next, "//") && !strings.HasPrefix(next, "/\\") {
		return next
	}
	return "/"
}

type LoginData struct {
	Next, Login, Error string
	CanReset           bool
}

func (a *App) loginPage(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Auth != "local" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	a.render(w, r, "login", "Anmelden", &LoginData{Next: safeNext(r.URL.Query().Get("weiter")), CanReset: a.mail.enabled()})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	ac := a.accounts
	login := strings.TrimSpace(r.PostFormValue("login"))
	data := &LoginData{Next: safeNext(r.PostFormValue("weiter")), Login: login, CanReset: a.mail.enabled()}
	ip, name := "ip:"+clientIP(r), "login:"+strings.ToLower(login)
	if ac.blocked(ip, name) {
		data.Error = "Zu viele Fehlversuche. Bitte warte eine Viertelstunde."
		w.WriteHeader(http.StatusTooManyRequests)
		a.render(w, r, "login", "Anmelden", data)
		return
	}
	var stored string
	var id int64
	var disabled bool
	err := ac.db.QueryRow("SELECT id, password, disabled FROM accounts WHERE login = ?", login).Scan(&id, &stored, &disabled)
	if err != nil || disabled || !checkPassword(stored, r.PostFormValue("password")) {
		ac.failed(ip, name)
		data.Error = "Anmeldename oder Passwort stimmen nicht."
		w.WriteHeader(http.StatusUnauthorized)
		a.render(w, r, "login", "Anmelden", data)
		return
	}
	acc, _ := ac.byID(id)
	ac.startSession(w, r, acc)
	http.Redirect(w, r, data.Next, http.StatusSeeOther)
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	a.accounts.endSession(w, r)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) forgotPassword(w http.ResponseWriter, r *http.Request) {
	ac := a.accounts
	who := strings.TrimSpace(r.PostFormValue("login"))
	ip := "ip:" + clientIP(r)
	if a.mail.enabled() && who != "" && !ac.blocked(ip) {
		ac.failed(ip) // counts as a try, so that the form cannot send mail endlessly
		var id int64
		var email string
		if ac.db.QueryRow("SELECT id, email FROM accounts WHERE (login = ? OR email = ? COLLATE NOCASE) AND disabled = 0 AND email != ''", who, who).Scan(&id, &email) == nil {
			link := ac.passwordLink(id)
			a.mail.queue(email, "Neues Passwort für "+a.cfg.SiteTitle,
				"Mit diesem Link setzt du ein neues Passwort (gültig 7 Tage):\n\n"+link+"\n\nWenn du das nicht angefordert hast, kannst du diese Nachricht ignorieren.\n")
		}
	}
	a.render(w, r, "login", "Anmelden", &LoginData{Next: "/", Login: who, CanReset: true,
		Error: "Wenn es zu diesem Namen ein Konto mit E-Mail-Adresse gibt, ist ein Link zum Setzen des Passworts unterwegs."})
}

type SetupData struct {
	Key, Login, Name, Email, Error string
}

func (a *App) setupPage(w http.ResponseWriter, r *http.Request) {
	ac := a.accounts
	if a.cfg.Auth != "local" || !ac.needsSetup() {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	key := r.URL.Query().Get("schluessel")
	if subtle.ConstantTimeCompare([]byte(key), []byte(ac.setupToken)) != 1 {
		a.setupNotice(w, r)
		return
	}
	a.render(w, r, "setup", "Einrichten", &SetupData{Key: key})
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	ac := a.accounts
	key := r.PostFormValue("schluessel")
	if a.cfg.Auth != "local" || !ac.needsSetup() || subtle.ConstantTimeCompare([]byte(key), []byte(ac.setupToken)) != 1 {
		a.setupNotice(w, r)
		return
	}
	data := &SetupData{Key: key, Login: r.PostFormValue("login"), Name: r.PostFormValue("name"), Email: r.PostFormValue("email")}
	pw := r.PostFormValue("password")
	if msg := passwordProblem(pw); msg != "" {
		data.Error = msg
	} else if pw != r.PostFormValue("password2") {
		data.Error = "Die beiden Passwörter sind verschieden."
	}
	var acc *Account
	var err error
	if data.Error == "" {
		if acc, err = ac.create(data.Login, data.Name, data.Email, RoleArchivar); err != nil {
			data.Error = err.Error()
		}
	}
	if data.Error != "" {
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, r, "setup", "Einrichten", data)
		return
	}
	ac.setPassword(acc.ID, pw)
	ac.setupToken = ""
	ac.startSession(w, r, acc)
	a.redirect(w, r, "/benutzer", "eingerichtet")
}

type SetPasswordData struct {
	Token string
	Acc   *Account
	Error string
}

func (a *App) setPasswordPage(w http.ResponseWriter, r *http.Request) {
	acc := a.accounts.linkAccount(r.PathValue("token"))
	if acc == nil {
		a.fail(w, r, http.StatusNotFound, "Dieser Link ist abgelaufen oder schon benutzt. Ein Archivar kann dir einen neuen geben.")
		return
	}
	a.render(w, r, "setpw", "Passwort setzen", &SetPasswordData{Token: r.PathValue("token"), Acc: acc})
}

func (a *App) setPasswordFromLink(w http.ResponseWriter, r *http.Request) {
	ac := a.accounts
	t := r.PathValue("token")
	acc := ac.linkAccount(t)
	if acc == nil {
		a.fail(w, r, http.StatusNotFound, "Dieser Link ist abgelaufen oder schon benutzt. Ein Archivar kann dir einen neuen geben.")
		return
	}
	pw := r.PostFormValue("password")
	msg := passwordProblem(pw)
	if msg == "" && pw != r.PostFormValue("password2") {
		msg = "Die beiden Passwörter sind verschieden."
	}
	if msg != "" {
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, r, "setpw", "Passwort setzen", &SetPasswordData{Token: t, Acc: acc, Error: msg})
		return
	}
	ac.setPassword(acc.ID, pw)
	ac.db.Exec("DELETE FROM password_links WHERE account = ?", acc.ID)
	ac.startSession(w, r, acc)
	a.redirect(w, r, "/", "")
}

type AccountData struct {
	Acc     *Account // auth = local
	Email   string
	MailOff bool
	Mail    bool // email is configured
	Error   string
}

func (a *App) accountPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil || u.Anonymous {
		http.Redirect(w, r, "/anmelden?weiter=/konto", http.StatusSeeOther)
		return
	}
	a.render(w, r, "account", "Mein Konto", a.accountData(u))
}

func (a *App) accountData(u *User) *AccountData {
	d := &AccountData{MailOff: a.people.mailOff(u.ID), Mail: a.mail.enabled()}
	a.db.QueryRow("SELECT email FROM users WHERE id = ?", u.ID).Scan(&d.Email)
	if a.cfg.Auth == "local" {
		id, _ := strconv.ParseInt(u.ID, 10, 64)
		d.Acc, _ = a.accounts.byID(id)
	}
	return d
}

func (a *App) saveAccount(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	if u == nil || u.Anonymous {
		http.Redirect(w, r, "/anmelden", http.StatusSeeOther)
		return
	}
	a.people.setMailOff(u.ID, r.PostFormValue("mail") != "1")
	if a.cfg.Auth == "local" {
		ac := a.accounts
		id, _ := strconv.ParseInt(u.ID, 10, 64)
		acc, err := ac.byID(id)
		if err != nil {
			a.fail(w, r, http.StatusNotFound, "Dieses Konto gibt es nicht.")
			return
		}
		fail := func(msg string) {
			d := a.accountData(u)
			d.Error = msg
			w.WriteHeader(http.StatusBadRequest)
			a.render(w, r, "account", "Mein Konto", d)
		}
		name := strings.TrimSpace(r.PostFormValue("name"))
		if name == "" {
			fail("Bitte gib einen Namen an.")
			return
		}
		if pw := r.PostFormValue("password"); pw != "" {
			var stored string
			ac.db.QueryRow("SELECT password FROM accounts WHERE id = ?", id).Scan(&stored)
			switch {
			case !checkPassword(stored, r.PostFormValue("current")):
				fail("Das bisherige Passwort stimmt nicht.")
				return
			case passwordProblem(pw) != "":
				fail(passwordProblem(pw))
				return
			case pw != r.PostFormValue("password2"):
				fail("Die beiden neuen Passwörter sind verschieden.")
				return
			}
			ac.setPassword(id, pw)
			ac.startSession(w, r, acc)
		}
		ac.db.Exec("UPDATE accounts SET name = ?, email = ? WHERE id = ?", name, strings.TrimSpace(r.PostFormValue("email")), id)
		if acc, err = ac.byID(id); err == nil {
			ac.publish(acc)
		}
	}
	a.redirect(w, r, "/konto", "gespeichert")
}

type UsersData struct {
	Accounts []*Account
	Roles    [][2]string
	Error    string
	Form     map[string]string
}

var roleChoices = [][2]string{{RoleReader, "Leser"}, {RoleSubmitter, "Einreicher"}, {RoleArchivar, "Archivar"}}

func (a *App) usersPage(w http.ResponseWriter, r *http.Request) {
	a.render(w, r, "users", "Benutzer", &UsersData{Accounts: a.accounts.list(), Roles: roleChoices, Form: map[string]string{"role": RoleSubmitter}})
}

func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	form := map[string]string{}
	for _, k := range []string{"login", "name", "email", "role"} {
		form[k] = strings.TrimSpace(r.PostFormValue(k))
	}
	acc, err := a.accounts.create(form["login"], form["name"], form["email"], form["role"])
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, r, "users", "Benutzer", &UsersData{Accounts: a.accounts.list(), Roles: roleChoices, Error: err.Error(), Form: form})
		return
	}
	a.showLink(w, r, acc, true)
}

// showLink makes a password link for the account and shows it, sending it
// by email when that is possible.
func (a *App) showLink(w http.ResponseWriter, r *http.Request, acc *Account, invite bool) {
	link := a.accounts.passwordLink(acc.ID)
	sent := false
	if a.mail.enabled() && acc.Email != "" {
		text := "Für dich gibt es ein Konto im Archiv " + a.cfg.SiteTitle + " (Anmeldename: " + acc.Login + ").\nMit diesem Link setzt du dein Passwort (gültig 7 Tage):\n\n" + link + "\n"
		subject := "Dein Konto im Archiv " + a.cfg.SiteTitle
		if !invite {
			text = "Mit diesem Link setzt du ein neues Passwort für das Archiv " + a.cfg.SiteTitle + " (gültig 7 Tage):\n\n" + link + "\n"
			subject = "Neues Passwort für " + a.cfg.SiteTitle
		}
		a.mail.queue(acc.Email, subject, text)
		sent = true
	}
	a.render(w, r, "userlink", acc.Name, map[string]any{"Acc": acc, "Link": link, "Sent": sent, "Invite": invite})
}

func (a *App) loadAccount(w http.ResponseWriter, r *http.Request) *Account {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	acc, err := a.accounts.byID(id)
	if err != nil {
		a.fail(w, r, http.StatusNotFound, "Dieses Konto gibt es nicht.")
		return nil
	}
	return acc
}

func (a *App) userPage(w http.ResponseWriter, r *http.Request) {
	if acc := a.loadAccount(w, r); acc != nil {
		a.render(w, r, "user", acc.Name, map[string]any{"Acc": acc, "Roles": roleChoices})
	}
}

func (a *App) saveUser(w http.ResponseWriter, r *http.Request) {
	ac := a.accounts
	acc := a.loadAccount(w, r)
	if acc == nil {
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	role := r.PostFormValue("role")
	disabled := r.PostFormValue("disabled") == "1"
	fail := func(msg string) {
		w.WriteHeader(http.StatusBadRequest)
		a.render(w, r, "user", acc.Name, map[string]any{"Acc": acc, "Roles": roleChoices, "Error": msg})
	}
	if name == "" {
		name = acc.Login
	}
	if accountRoleLabels[role] == "" {
		fail("Unbekannte Rolle.")
		return
	}
	// the archive keeps at least one archivar
	if acc.Role == RoleArchivar && !acc.Disabled && (role != RoleArchivar || disabled) && ac.archivars() <= 1 {
		fail("Das ist der letzte Archivar. Lege zuerst einen anderen an.")
		return
	}
	ac.db.Exec("UPDATE accounts SET name = ?, email = ?, role = ?, disabled = ? WHERE id = ?",
		name, strings.TrimSpace(r.PostFormValue("email")), role, disabled, acc.ID)
	if disabled {
		ac.db.Exec("DELETE FROM sessions WHERE account = ?", acc.ID)
	}
	if acc, err := ac.byID(acc.ID); err == nil {
		ac.publish(acc)
	}
	a.redirect(w, r, "/benutzer", "gespeichert")
}

func (a *App) newPasswordLink(w http.ResponseWriter, r *http.Request) {
	if acc := a.loadAccount(w, r); acc != nil {
		a.showLink(w, r, acc, !acc.HasPass)
	}
}

// accountNextURL is the login page's address for a path, for templates.
func loginURL(next string) string { return "/anmelden?weiter=" + url.QueryEscape(next) }
