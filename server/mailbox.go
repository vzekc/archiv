package archiv

import (
	"context"
	"crypto/tls"
	"fmt"
	"html"
	"mime"
	"net"
	"net/http"
	"net/smtp"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The archive's own handling of submissions, for installations without an
// extension that does it (a forum, say): every person has messages in the
// archive (/nachrichten), and with a mail server configured they get them
// by email too. Archivars hear of new submissions, answers and reminders;
// submitters hear what becomes of theirs; the catalog's messages (answers
// to comments, decisions on proposals, the daily digest) arrive the same
// way. The discussion of a submission happens in its comments.

const mailboxSchema = `
CREATE TABLE IF NOT EXISTS inbox (
    id       INTEGER PRIMARY KEY,
    user     TEXT NOT NULL,
    at       INTEGER NOT NULL,
    subject  TEXT NOT NULL,
    html     TEXT NOT NULL,
    read_at  INTEGER
);
CREATE INDEX IF NOT EXISTS inbox_user ON inbox(user, read_at);
CREATE TABLE IF NOT EXISTS mail_jobs (
    id       INTEGER PRIMARY KEY,
    to_addr  TEXT NOT NULL,
    subject  TEXT NOT NULL,
    body     TEXT NOT NULL,
    tries    INTEGER NOT NULL DEFAULT 0,
    next_try INTEGER NOT NULL DEFAULT 0,
    error    TEXT NOT NULL DEFAULT ''
);
`

// mailbox is the built-in Notifier and Messenger.
type mailbox struct{ a *App }

func (m *mailbox) Init(a *App) error {
	m.a = a
	_, err := a.db.Exec(mailboxSchema)
	return err
}

// deliver puts a message into the inbox of user and queues the email.
func (m *mailbox) deliver(user, subject, body string) {
	if user == "" {
		return
	}
	m.a.db.Exec("INSERT INTO inbox (user, at, subject, html) VALUES (?, ?, ?, ?)", user, now(), subject, body)
	if to := m.a.people.mailTo(user); to != "" && m.a.mail.enabled() {
		m.a.mail.queue(to, subject, htmlToText(body)+"\n\n-- \nAlle Nachrichten: "+m.a.cfg.SiteURL+"/nachrichten\n")
	}
}

// moderators are the ones messages to all moderators go to, but not who
// caused the message.
func (m *mailbox) moderators(except string) []string {
	var out []string
	ids := m.a.people.moderators()
	if m.a.cfg.Auth == "local" {
		ids = nil
		rows, err := m.a.db.Query("SELECT id FROM accounts WHERE role = ? AND disabled = 0 ORDER BY id", RoleArchivar)
		if err == nil {
			for rows.Next() {
				var id int64
				rows.Scan(&id)
				ids = append(ids, strconv.FormatInt(id, 10))
			}
			rows.Close()
		}
	}
	for _, id := range ids {
		if id != except {
			out = append(out, id)
		}
	}
	return out
}

// Send delivers a message about the catalog.
func (m *mailbox) Send(_ context.Context, user, subject, body string) error {
	if user == "" {
		for _, id := range m.moderators("") {
			m.deliver(id, subject, body)
		}
		return nil
	}
	m.deliver(user, subject, body)
	return nil
}

// Notify tells the people concerned what happened to a submission.
func (m *mailbox) Notify(ev SubmissionEvent) {
	a := m.a
	s, err := a.db.submission(ev.Submission)
	if err != nil {
		return
	}
	esc := html.EscapeString
	link := a.absURL("/e/%d", s.ID)
	title := esc(s.Title)
	at := fmt.Sprintf(`<p><a href="%s">%s</a></p>`, link, link)
	// the one working on it, or else all moderators
	handlers := func(except string) []string {
		if s.ClaimedBy.Valid {
			if s.ClaimedBy.String == except {
				return nil
			}
			return []string{s.ClaimedBy.String}
		}
		return m.moderators(except)
	}
	toAll := func(ids []string, subject, body string) {
		for _, id := range ids {
			m.deliver(id, subject, body)
		}
	}
	owner := s.Owner
	switch ev.Kind {
	case EvNew:
		if !(s.ClaimedBy.Valid && s.ClaimedBy.String == owner) {
			m.deliver(owner, "Eingegangen: "+s.Title, fmt.Sprintf("<p>Deine Einreichung <b>%s</b> ist eingegangen und liegt jetzt bei den Archivaren. Du bekommst Bescheid, sobald sie aufgenommen ist oder es eine Rückfrage gibt.</p>%s", title, at))
		}
		toAll(handlers(owner), "Neue Einreichung: "+s.Title, fmt.Sprintf("<p><b>%s</b> hat <b>%s</b> eingereicht.</p>%s", esc(a.names.Name(owner)), title, at))
	case EvComment:
		toAll(handlers(ev.User), "Kommentar: "+s.Title, fmt.Sprintf("<p>%s schreibt zu <b>%s</b>:</p>%s%s", esc(a.names.Name(ev.User)), title, paragraphs(ev.Text), at))
	case EvQuestion:
		m.deliver(owner, "Rückfrage: "+s.Title, fmt.Sprintf("<p>Zu deiner Einreichung <b>%s</b> gibt es eine Rückfrage:</p>%s<p>Antworten kannst du auf der Seite der Einreichung; dort kannst du auch Dateien und Beschreibungen ändern.</p>%s", title, paragraphs(ev.Text), at))
	case EvAnswered:
		body := fmt.Sprintf("<p>%s hat <b>%s</b> überarbeitet und erneut eingereicht.</p>%s", esc(a.names.Name(owner)), title, at)
		if ev.Text != "" {
			body = fmt.Sprintf("<p>Antwort von %s zu <b>%s</b>:</p>%s%s", esc(a.names.Name(owner)), title, paragraphs(ev.Text), at)
		}
		toAll(handlers(owner), "Antwort: "+s.Title, body)
	case EvRejected:
		if ev.User != owner {
			m.deliver(owner, "Nicht aufgenommen: "+s.Title, fmt.Sprintf("<p>Deine Einreichung <b>%s</b> wurde leider nicht aufgenommen. Begründung:</p>%s%s", title, paragraphs(ev.Text), at))
		}
	case EvDone:
		if s.DecidedBy.String != owner {
			m.deliver(owner, "Im Archiv: "+s.Title, fmt.Sprintf("<p>Deine Einreichung <b>%s</b> ist jetzt im Archiv. Vielen Dank!</p>%s", title, at))
		}
	case EvFailed:
		toAll(handlers(""), "Aufnahme fehlgeschlagen: "+s.Title, fmt.Sprintf("<p>Die Aufnahme von <b>%s</b> ist fehlgeschlagen: %s</p>%s", title, esc(ev.Text), at))
	case EvRemindClaim:
		m.deliver(ev.User, "Erinnerung: "+s.Title, fmt.Sprintf("<p><b>%s</b> wartet seit %s auf deine Entscheidung. Wenn du nicht dazu kommst, gib sie bitte zurück.</p>%s", title, esc(ev.Text), at))
	case EvAutoRelease:
		toAll(m.moderators(""), "Wieder frei: "+s.Title, fmt.Sprintf("<p>Die Übernahme von <b>%s</b> durch %s ist nach %s ohne Entscheidung zurückgegeben worden.</p>%s", title, esc(a.names.Name(ev.User)), days(a.cfg.ClaimRelease), at))
	case EvRemindUnclaimed:
		toAll(m.moderators(""), "Wartet: "+s.Title, fmt.Sprintf("<p><b>%s</b> wartet seit %s darauf, dass sie jemand übernimmt.</p>%s", title, esc(ev.Text), at))
	case EvRemindQuestion:
		m.deliver(owner, "Erinnerung: "+s.Title, fmt.Sprintf("<p>Zu deiner Einreichung <b>%s</b> ist noch eine Rückfrage offen.</p>%s", title, at))
	}
}

// Work sends the queued emails.
func (m *mailbox) Work(ctx context.Context) { m.a.mail.run(ctx) }

func paragraphs(text string) string {
	var out []string
	for _, p := range strings.Split(strings.TrimSpace(text), "\n\n") {
		out = append(out, "<p>"+strings.ReplaceAll(html.EscapeString(strings.TrimSpace(p)), "\n", "<br>")+"</p>")
	}
	return strings.Join(out, "")
}

var (
	tagRE   = regexp.MustCompile(`<[^>]*>`)
	blockRE = regexp.MustCompile(`(?i)</p>|<br\s*/?>|</li>|</h\d>|</blockquote>`)
	hrefRE  = regexp.MustCompile(`(?i)<a\s[^>]*href="([^"]*)"[^>]*>([^<]*)</a>`)
)

// htmlToText is a message's text for an email.
func htmlToText(h string) string {
	h = hrefRE.ReplaceAllStringFunc(h, func(m string) string {
		p := hrefRE.FindStringSubmatch(m)
		if p[1] == p[2] {
			return p[1]
		}
		return p[2] + " (" + p[1] + ")"
	})
	h = blockRE.ReplaceAllString(h, "\n")
	h = strings.ReplaceAll(h, "<li>", "- ")
	h = html.UnescapeString(tagRE.ReplaceAllString(h, ""))
	var lines []string
	for _, l := range strings.Split(h, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	return strings.TrimSpace(regexp.MustCompile(`\n{3,}`).ReplaceAllString(strings.Join(lines, "\n"), "\n\n"))
}

// ---------------------------------------------------------------- inbox pages

type InboxMessage struct {
	ID      int64
	At      int64
	Subject string
	HTML    string
	Unread  bool
}

func (a *App) unread(user string) int {
	if user == "" || a.mailbox == nil {
		return 0
	}
	var n int
	a.db.QueryRow("SELECT COUNT(*) FROM inbox WHERE user = ? AND read_at IS NULL", user).Scan(&n)
	return n
}

func (a *App) inboxPage(w http.ResponseWriter, r *http.Request) {
	u := currentUser(r)
	rows, err := a.db.Query("SELECT id, at, subject, html, read_at IS NULL FROM inbox WHERE user = ? ORDER BY id DESC LIMIT 200", u.ID)
	if err != nil {
		a.fail(w, r, 500, "Datenbankfehler.")
		return
	}
	var msgs []InboxMessage
	for rows.Next() {
		var m InboxMessage
		rows.Scan(&m.ID, &m.At, &m.Subject, &m.HTML, &m.Unread)
		msgs = append(msgs, m)
	}
	rows.Close()
	// shown is read
	a.db.Exec("UPDATE inbox SET read_at = ? WHERE user = ? AND read_at IS NULL", now(), u.ID)
	a.render(w, r, "inbox", "Nachrichten", msgs)
}

// ---------------------------------------------------------------- email

// Mailer sends email through the configured mail server; queued, so that
// a slow or unavailable server never holds up a page.
type Mailer struct {
	a *App
}

func (ml *Mailer) enabled() bool { return ml.a.cfg.SMTPHost != "" && ml.a.cfg.MailFrom != "" }

func (ml *Mailer) queue(to, subject, body string) {
	if !ml.enabled() || to == "" {
		return
	}
	ml.a.db.Exec("CREATE TABLE IF NOT EXISTS mail_jobs (id INTEGER PRIMARY KEY, to_addr TEXT NOT NULL, subject TEXT NOT NULL, body TEXT NOT NULL, tries INTEGER NOT NULL DEFAULT 0, next_try INTEGER NOT NULL DEFAULT 0, error TEXT NOT NULL DEFAULT '')")
	ml.a.db.Exec("INSERT INTO mail_jobs (to_addr, subject, body) VALUES (?, ?, ?)", to, subject, body)
	ml.a.poke()
}

// run sends what is queued; failures are tried again with growing pauses,
// for at most a day.
func (ml *Mailer) run(ctx context.Context) {
	if !ml.enabled() {
		return
	}
	rows, err := ml.a.db.Query("SELECT id, to_addr, subject, body, tries FROM mail_jobs WHERE next_try <= ? ORDER BY id LIMIT 50", now())
	if err != nil {
		return
	}
	type job struct {
		id                int64
		to, subject, body string
		tries             int
	}
	var jobs []job
	for rows.Next() {
		var j job
		rows.Scan(&j.id, &j.to, &j.subject, &j.body, &j.tries)
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if err := ml.send(j.to, j.subject, j.body); err != nil {
			if j.tries >= 10 {
				logf("mail to %s given up: %v", j.to, err)
				ml.a.db.Exec("DELETE FROM mail_jobs WHERE id = ?", j.id)
				continue
			}
			delay := time.Duration(1<<min(j.tries, 7)) * time.Minute
			logf("mail to %s: %v; retry in %v", j.to, err, delay)
			ml.a.db.Exec("UPDATE mail_jobs SET tries = tries + 1, next_try = ?, error = ? WHERE id = ?", now()+int64(delay.Seconds()), err.Error(), j.id)
			continue
		}
		ml.a.db.Exec("DELETE FROM mail_jobs WHERE id = ?", j.id)
	}
}

// send delivers one email: implicit TLS on port 465, otherwise STARTTLS
// when the server offers it.
func (ml *Mailer) send(to, subject, body string) error {
	c := ml.a.cfg
	addr := net.JoinHostPort(c.SMTPHost, strconv.Itoa(c.SMTPPort))
	var conn net.Conn
	var err error
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	if c.SMTPPort == 465 {
		conn, err = tls.DialWithDialer(dialer, "tcp", addr, &tls.Config{ServerName: c.SMTPHost})
	} else {
		conn, err = dialer.Dial("tcp", addr)
	}
	if err != nil {
		return err
	}
	conn.SetDeadline(time.Now().Add(2 * time.Minute))
	cl, err := smtp.NewClient(conn, c.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer cl.Close()
	if ok, _ := cl.Extension("STARTTLS"); ok && c.SMTPPort != 465 {
		if err := cl.StartTLS(&tls.Config{ServerName: c.SMTPHost}); err != nil {
			return err
		}
	}
	if c.SMTPUser != "" {
		pw, err := os.ReadFile(c.SMTPPassFile)
		if err != nil {
			return fmt.Errorf("smtp_password_file: %v", err)
		}
		if err := cl.Auth(smtp.PlainAuth("", c.SMTPUser, strings.TrimSpace(string(pw)), c.SMTPHost)); err != nil {
			return err
		}
	}
	if err := cl.Mail(c.MailFrom); err != nil {
		return err
	}
	if err := cl.Rcpt(to); err != nil {
		return err
	}
	wc, err := cl.Data()
	if err != nil {
		return err
	}
	msg := "From: " + mimeHeader(c.SiteTitle) + " <" + c.MailFrom + ">\r\n" +
		"To: " + to + "\r\n" +
		"Subject: " + mimeHeader(subject) + "\r\n" +
		"Date: " + time.Now().Format(time.RFC1123Z) + "\r\n" +
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" +
		strings.ReplaceAll(body, "\n", "\r\n")
	if _, err := wc.Write([]byte(msg)); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return cl.Quit()
}

// mimeHeader encodes a header value that is not plain ASCII.
func mimeHeader(s string) string { return mime.BEncoding.Encode("utf-8", s) }
