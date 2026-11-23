package archiv

import (
	"sync"
	"time"
)

// People is what the archive knows about the people behind the user ids,
// beyond their names (names.go): their email address, whether they
// moderate, and whether they want messages by email. With auth = local the
// accounts say it; with auth = header the archive learns it from the login
// headers as people come by (seen).
type People struct {
	db   *DB
	mu   sync.Mutex
	last map[string]string // id -> what was stored last, so that a visit writes only changes
}

func newPeople(db *DB) *People {
	for _, c := range []struct{ col, def string }{
		{"email", "TEXT NOT NULL DEFAULT ''"},
		{"moderator", "INTEGER NOT NULL DEFAULT 0"},
		{"seen", "INTEGER NOT NULL DEFAULT 0"},
		{"mail_off", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('users') WHERE name = ?", c.col).Scan(&n)
		if n == 0 {
			db.Exec("ALTER TABLE users ADD COLUMN " + c.col + " " + c.def)
		}
	}
	return &People{db: db, last: map[string]string{}}
}

// seen notes a visit of id with the email address and moderator flag the
// login gave, at most once a day unless something changed.
func (p *People) seen(id, email string, moderator bool) {
	day := time.Now().Format("2006-01-02")
	key := email + "\x00" + boolStr(moderator) + "\x00" + day
	p.mu.Lock()
	same := p.last[id] == key
	p.last[id] = key
	p.mu.Unlock()
	if same {
		return
	}
	p.db.Exec(`INSERT INTO users (id, name, updated, email, moderator, seen) VALUES (?, '', ?, ?, ?, ?)
	    ON CONFLICT(id) DO UPDATE SET email = CASE WHEN excluded.email != '' THEN excluded.email ELSE users.email END,
	    moderator = excluded.moderator, seen = excluded.seen`, id, now(), email, moderator, now())
}

// set stores what an account says about its owner.
func (p *People) set(id, name, email string, moderator bool) {
	p.db.Exec(`INSERT INTO users (id, name, updated, email, moderator, seen) VALUES (?, ?, ?, ?, ?, ?)
	    ON CONFLICT(id) DO UPDATE SET name = excluded.name, email = excluded.email, moderator = excluded.moderator, seen = excluded.seen`,
		id, name, now(), email, moderator, now())
}

// moderators are the ids of those who moderate; with auth = header those
// seen in the last half year.
func (p *People) moderators() []string {
	rows, err := p.db.Query("SELECT id FROM users WHERE moderator = 1 AND seen > ? ORDER BY id", now()-183*24*3600)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		rows.Scan(&id)
		out = append(out, id)
	}
	return out
}

// mailTo is the address messages to id go to by email, or "".
func (p *People) mailTo(id string) string {
	var email string
	var off bool
	p.db.QueryRow("SELECT email, mail_off FROM users WHERE id = ?", id).Scan(&email, &off)
	if off {
		return ""
	}
	return email
}

func (p *People) mailOff(id string) bool {
	var off bool
	p.db.QueryRow("SELECT mail_off FROM users WHERE id = ?", id).Scan(&off)
	return off
}

func (p *People) setMailOff(id string, off bool) {
	p.db.Exec("UPDATE users SET mail_off = ? WHERE id = ?", off, id)
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}
