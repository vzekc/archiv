package archiv

import (
	"strings"
	"sync"
	"time"
)

// Names maps user ids to display names, cached in the database. The login
// may pass the name along (name_header); otherwise a Directory extension
// looks names up in the background.
type Names struct {
	db      *DB
	mu      sync.Mutex
	cache   map[string]string
	missing map[string]bool
}

func newNames(db *DB) *Names {
	n := &Names{db: db, cache: map[string]string{}, missing: map[string]bool{}}
	rows, err := db.Query("SELECT id, name FROM users")
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var id, name string
			rows.Scan(&id, &name)
			n.cache[id] = name
		}
	}
	return n
}

func (n *Names) Name(id string) string {
	if id == "" {
		return ""
	}
	if strings.HasPrefix(id, "ai:") {
		return "automatischer Vorschlag"
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if name, ok := n.cache[id]; ok {
		return name
	}
	n.missing[id] = true
	return "Mitglied #" + id
}

// want makes sure the name of id gets looked up.
func (n *Names) want(id string) { n.Name(id) }

func (n *Names) remember(id, name string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.cache[id] == name {
		return
	}
	n.cache[id] = name
	delete(n.missing, id)
	n.db.Exec("INSERT INTO users (id, name, updated) VALUES (?,?,?) ON CONFLICT(id) DO UPDATE SET name = excluded.name, updated = excluded.updated",
		id, name, time.Now().Unix())
}

func (n *Names) takeMissing() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	var out []string
	for id := range n.missing {
		out = append(out, id)
	}
	n.missing = map[string]bool{}
	return out
}
