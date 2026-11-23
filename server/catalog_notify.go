package archiv

import (
	"context"
	"fmt"
	"html"
	"strconv"
	"strings"
	"time"
)

// Notifications about the catalog: an answer to a comment or report reaches
// its author, the decision on a proposal reaches whoever proposed it, and the
// moderators get a daily digest of what is open. They are queued and handed
// to the Messenger extension; without one, none are kept.

func (a *App) notify(user, subject, body string) {
	if a.messenger() == nil || (user == "" && subject != digestSubject) {
		return
	}
	a.cat.Exec("INSERT INTO notify_jobs (user, subject, html) VALUES (?, ?, ?)", user, subject, body)
}

// notifyReply tells the author of a comment that someone answered.
func (a *App) notifyReply(parent, reply int64, back string) {
	var author, replier, body string
	if a.cat.QueryRow("SELECT user FROM comments WHERE id = ?", parent).Scan(&author) != nil {
		return
	}
	a.cat.QueryRow("SELECT user, body FROM comments WHERE id = ?", reply).Scan(&replier, &body)
	if author == replier {
		return
	}
	link := siteURL + back + "#k" + strconv.FormatInt(reply, 10)
	a.notify(author, "Antwort im Archiv", fmt.Sprintf(`<p>%s hat auf deinen Beitrag geantwortet:</p><blockquote>%s</blockquote><p><a href="%s">%s</a></p>`,
		html.EscapeString(a.names.Name(replier)), html.EscapeString(limitBytes(body, 2000)), link, link))
}

// notifyDecided tells the proposers what became of their proposals.
func (a *App) notifyDecided(ids []int64, accepted bool) {
	byUser := map[string][]RevView{}
	for _, id := range ids {
		for _, rv := range a.revisions("id = ?", []any{id}, 1) {
			byUser[rv.User] = append(byUser[rv.User], rv)
		}
	}
	verb := "übernommen"
	if !accepted {
		verb = "nicht übernommen"
	}
	for u, revs := range byUser {
		var b strings.Builder
		fmt.Fprintf(&b, "<p>Deine Vorschläge zum Archiv wurden %s:</p><ul>", verb)
		for _, rv := range revs {
			fmt.Fprintf(&b, `<li><a href="%s%s">%s</a>: %s</li>`, siteURL, rv.TargetHref, html.EscapeString(rv.TargetLabel), html.EscapeString(rv.What))
		}
		b.WriteString("</ul>")
		a.notify(u, "Vorschläge zum Archiv", b.String())
	}
}

const digestSubject = "Archiv: offene Vorschläge und Meldungen"

// queueDigest sends the moderators the open proposals and reports, once a
// day in the morning, when there are any.
func (a *App) queueDigest() {
	t := time.Now().In(berlin)
	if t.Hour() < 8 {
		return
	}
	day := t.Format("2006-01-02")
	if a.cat.meta("last_digest") == day {
		return
	}
	var props, reports int
	a.cat.QueryRow("SELECT COUNT(*) FROM revisions WHERE state = 'proposed'").Scan(&props)
	a.cat.QueryRow("SELECT COUNT(*) FROM comments WHERE kind != 'comment' AND state = 'open'").Scan(&reports)
	a.cat.setMeta("last_digest", day)
	if props+reports == 0 {
		return
	}
	link := siteURL + "/moderation"
	a.notify("", digestSubject, fmt.Sprintf(`<p>Im Archiv warten %s und %s auf eine Entscheidung:</p><p><a href="%s">%s</a></p>`,
		plural(props, "Vorschlag", "Vorschläge"), plural(reports, "Meldung", "Meldungen"), link, link))
}

// runNotify sends what is queued.
func (a *App) runNotify(ctx context.Context) {
	m := a.messenger()
	if m == nil {
		return
	}
	rows, err := a.cat.Query("SELECT id, user, subject, html, tries FROM notify_jobs WHERE next_try <= ? ORDER BY id LIMIT 20", now())
	if err != nil {
		return
	}
	type job struct {
		id                  int64
		user, subject, body string
		tries               int
	}
	var jobs []job
	for rows.Next() {
		var j job
		rows.Scan(&j.id, &j.user, &j.subject, &j.body, &j.tries)
		jobs = append(jobs, j)
	}
	rows.Close()
	for _, j := range jobs {
		if ctx.Err() != nil {
			return
		}
		if err := m.Send(ctx, j.user, j.subject, j.body); err != nil {
			delay := time.Duration(1<<min(j.tries, 8)) * time.Minute
			logf("notify job %d (%s): %v; retry in %v", j.id, j.user, err, delay)
			a.cat.Exec("UPDATE notify_jobs SET tries = tries + 1, next_try = ?, error = ? WHERE id = ?", now()+int64(delay.Seconds()), err.Error(), j.id)
			continue
		}
		a.cat.Exec("DELETE FROM notify_jobs WHERE id = ?", j.id)
	}
}
