package archiv

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// States of a submission.
const (
	StDraft      = "draft"      // being edited by its owner
	StAnalysing  = "analysing"  // submitted, final AI classification running
	StModeration = "moderation" // waiting for / with a moderator
	StQuestion   = "question"   // sent back to the owner with a question
	StIntake     = "intake"     // accepted, files being moved into the archive
	StDone       = "done"       // in the archive
	StRejected   = "rejected"
	StFailed     = "failed" // intake failed, needs a moderator
)

// States of a file.
const (
	FUploading  = "uploading"
	FProcessing = "processing" // hashing, extracting, previews
	FReady      = "ready"
	FError      = "error"
	FTaken      = "taken"   // moved into the archive
	FSkipped    = "skipped" // left out at intake
)

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS submissions (
    id            INTEGER PRIMARY KEY,
    owner         TEXT NOT NULL,          -- user id from the login
    title         TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    credit        INTEGER NOT NULL DEFAULT 0,
    owner_note    TEXT NOT NULL DEFAULT '', -- why the owner keeps their own choice
    state         TEXT NOT NULL DEFAULT 'draft',
    created       INTEGER NOT NULL,
    updated       INTEGER NOT NULL,
    submitted     INTEGER,
    claimed_by    TEXT,
    claimed_at    INTEGER,
    last_activity INTEGER,                -- last moderator action or comment
    reminded_at   INTEGER,
    reminders     INTEGER NOT NULL DEFAULT 0,
    decided_by    TEXT,
    decided_at    INTEGER,
    decision_note TEXT NOT NULL DEFAULT '',
    question      TEXT NOT NULL DEFAULT '',
    analysis      TEXT NOT NULL DEFAULT '', -- final classification of the whole submission (json)
    analysis_state TEXT NOT NULL DEFAULT '' -- '', pending, done, failed
);
CREATE INDEX IF NOT EXISTS submissions_owner ON submissions(owner);
CREATE INDEX IF NOT EXISTS submissions_state ON submissions(state);

CREATE TABLE IF NOT EXISTS files (
    id           INTEGER PRIMARY KEY,
    submission   INTEGER NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    size         INTEGER NOT NULL,
    upload_id    TEXT UNIQUE,
    path         TEXT NOT NULL DEFAULT '', -- staged file, relative to the staging dir
    sha256       TEXT NOT NULL DEFAULT '',
    kind         TEXT NOT NULL DEFAULT '', -- pdf, image, archive, image-disk, iso, text, other
    description  TEXT NOT NULL DEFAULT '',
    tags         TEXT NOT NULL DEFAULT '', -- tags chosen by the moderator, one "Facette: A › B" per line
    unpack       INTEGER NOT NULL DEFAULT 0,
    split        INTEGER NOT NULL DEFAULT 0, -- unpacked: each top folder and file an item of its own
    duplicates   TEXT NOT NULL DEFAULT '', -- json list of {where, path}
    info         TEXT NOT NULL DEFAULT '', -- json: pages, text excerpt, listing ...
    status       TEXT NOT NULL DEFAULT 'uploading',
    error        TEXT NOT NULL DEFAULT '',
    suggestion   TEXT NOT NULL DEFAULT '', -- latest AI answer for this file (json)
    suggest_key  TEXT NOT NULL DEFAULT '', -- inputs the suggestion was made for
    suggest_due  INTEGER,                  -- when to ask the AI (draft debounce)
    archive_path TEXT NOT NULL DEFAULT '', -- where it went
    created      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS files_submission ON files(submission);
CREATE INDEX IF NOT EXISTS files_sha ON files(sha256);
CREATE INDEX IF NOT EXISTS files_status ON files(status);

CREATE TABLE IF NOT EXISTS quotas (
    member         TEXT PRIMARY KEY,
    max_file       INTEGER NOT NULL,
    max_submission INTEGER NOT NULL,
    max_pending    INTEGER NOT NULL,
    until          INTEGER,          -- NULL = permanent
    set_by         TEXT NOT NULL,
    set_at         INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS comments (
    id          INTEGER PRIMARY KEY,
    submission  INTEGER NOT NULL REFERENCES submissions(id) ON DELETE CASCADE,
    author      TEXT NOT NULL,
    time        INTEGER NOT NULL,
    text        TEXT NOT NULL,
    kind        TEXT NOT NULL DEFAULT 'comment' -- comment, question, answer
);

CREATE TABLE IF NOT EXISTS events (           -- audit log
    id          INTEGER PRIMARY KEY,
    time        INTEGER NOT NULL,
    actor       TEXT NOT NULL,
    submission  INTEGER,
    action      TEXT NOT NULL,
    detail      TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS events_submission ON events(submission);

CREATE TABLE IF NOT EXISTS users (            -- display names, cached
    id       TEXT PRIMARY KEY,
    name     TEXT NOT NULL,
    updated  INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS archive_hashes (   -- sha256 of archive files, for duplicates
    path   TEXT PRIMARY KEY,
    size   INTEGER NOT NULL,
    mtime  INTEGER NOT NULL,
    sha256 TEXT NOT NULL
);
`

type DB struct{ *sql.DB }

func openDB(path string) (*DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(10000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // sqlite: one writer; keeps things simple and safe
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	if err := upgradeDB(db); err != nil {
		return nil, fmt.Errorf("schema: %w", err)
	}
	os.Chmod(path, 0o660)
	return &DB{db}, nil
}

// upgradeDB brings a database of the folder era up to the schema: a file
// carries tags where it carried a folder, and the AI proposes no folders.
func upgradeDB(db *sql.DB) error {
	var n int
	db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('files') WHERE name = 'target'").Scan(&n)
	if n == 1 {
		for _, q := range []string{
			"ALTER TABLE files RENAME COLUMN target TO tags",
			"UPDATE files SET tags = ''",
		} {
			if _, err := db.Exec(q); err != nil {
				return err
			}
		}
	}
	db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('submissions') WHERE name = 'area_hint'").Scan(&n)
	if n == 1 {
		if _, err := db.Exec("ALTER TABLE submissions DROP COLUMN area_hint"); err != nil {
			return err
		}
	}
	db.QueryRow("SELECT COUNT(*) FROM pragma_table_info('files') WHERE name = 'split'").Scan(&n)
	if n == 0 {
		if _, err := db.Exec("ALTER TABLE files ADD COLUMN split INTEGER NOT NULL DEFAULT 0"); err != nil {
			return err
		}
	}
	_, err := db.Exec("DROP TABLE IF EXISTS proposals")
	return err
}

func now() int64 { return time.Now().Unix() }

type Submission struct {
	ID            int64
	Owner         string
	Title         string
	Description   string
	Credit        bool
	OwnerNote     string
	State         string
	Created       int64
	Updated       int64
	Submitted     sql.NullInt64
	ClaimedBy     sql.NullString
	ClaimedAt     sql.NullInt64
	LastActivity  sql.NullInt64
	RemindedAt    sql.NullInt64
	Reminders     int
	DecidedBy     sql.NullString
	DecidedAt     sql.NullInt64
	DecisionNote  string
	Question      string
	Analysis      string
	AnalysisState string
}

const submissionCols = `id, owner, title, description, credit, owner_note, state, created,
    updated, submitted, claimed_by, claimed_at, last_activity, reminded_at, reminders, decided_by,
    decided_at, decision_note, question, analysis, analysis_state`

func scanSubmission(row interface{ Scan(...any) error }) (*Submission, error) {
	s := &Submission{}
	err := row.Scan(&s.ID, &s.Owner, &s.Title, &s.Description, &s.Credit, &s.OwnerNote,
		&s.State, &s.Created, &s.Updated, &s.Submitted, &s.ClaimedBy, &s.ClaimedAt, &s.LastActivity,
		&s.RemindedAt, &s.Reminders, &s.DecidedBy, &s.DecidedAt, &s.DecisionNote, &s.Question,
		&s.Analysis, &s.AnalysisState)
	return s, err
}

// ErrNotFound is the answer for a submission that does not exist.
var ErrNotFound = errors.New("not found")

var errNotFound = ErrNotFound

func (db *DB) submission(id int64) (*Submission, error) {
	s, err := scanSubmission(db.QueryRow("SELECT "+submissionCols+" FROM submissions WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return s, err
}

func (db *DB) submissions(where string, args ...any) ([]*Submission, error) {
	rows, err := db.Query("SELECT "+submissionCols+" FROM submissions WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Submission
	for rows.Next() {
		s, err := scanSubmission(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

type File struct {
	ID          int64
	Submission  int64
	Name        string
	Size        int64
	UploadID    sql.NullString
	Path        string
	SHA256      string
	Kind        string
	Description string
	Tags        string // one tag per line, "Facette: A › B"
	Unpack      bool
	Split       bool // unpacked into an item per top folder and file
	Duplicates  string
	Info        string
	Status      string
	Error       string
	Suggestion  string
	SuggestKey  string
	SuggestDue  sql.NullInt64
	ArchivePath string
	Created     int64
}

const fileCols = `id, submission, name, size, upload_id, path, sha256, kind, description, tags, unpack, split,
    duplicates, info, status, error, suggestion, suggest_key, suggest_due, archive_path, created`

func scanFile(row interface{ Scan(...any) error }) (*File, error) {
	f := &File{}
	err := row.Scan(&f.ID, &f.Submission, &f.Name, &f.Size, &f.UploadID, &f.Path, &f.SHA256, &f.Kind,
		&f.Description, &f.Tags, &f.Unpack, &f.Split, &f.Duplicates, &f.Info, &f.Status, &f.Error,
		&f.Suggestion, &f.SuggestKey, &f.SuggestDue, &f.ArchivePath, &f.Created)
	return f, err
}

func (db *DB) file(id int64) (*File, error) {
	f, err := scanFile(db.QueryRow("SELECT "+fileCols+" FROM files WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errNotFound
	}
	return f, err
}

func (db *DB) files(where string, args ...any) ([]*File, error) {
	rows, err := db.Query("SELECT "+fileCols+" FROM files WHERE "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*File
	for rows.Next() {
		f, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (db *DB) filesOf(sub int64) ([]*File, error) {
	return db.files("submission = ? AND status <> ? ORDER BY name COLLATE NOCASE, id", sub, FUploading)
}

// logEvent writes the audit log; failures are only printed, never fatal.
func (db *DB) logEvent(actor string, sub int64, action string, detail any) {
	var d string
	switch v := detail.(type) {
	case nil:
	case string:
		d = v
	default:
		b, _ := json.Marshal(v)
		d = string(b)
	}
	if _, err := db.Exec("INSERT INTO events (time, actor, submission, action, detail) VALUES (?,?,?,?,?)",
		now(), actor, sub, action, d); err != nil {
		logf("event %s/%d/%s: %v", actor, sub, action, err)
	}
}

type Comment struct {
	ID     int64
	Author string
	Time   int64
	Text   string
	Kind   string
}

func (db *DB) comments(sub int64) ([]Comment, error) {
	rows, err := db.Query("SELECT id, author, time, text, kind FROM comments WHERE submission = ? ORDER BY time, id", sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Comment
	for rows.Next() {
		var c Comment
		if err := rows.Scan(&c.ID, &c.Author, &c.Time, &c.Text, &c.Kind); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type Event struct {
	Time   int64
	Actor  string
	Action string
	Detail string
}

func (db *DB) events(sub int64) ([]Event, error) {
	rows, err := db.Query("SELECT time, actor, action, detail FROM events WHERE submission = ? ORDER BY time, id", sub)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.Time, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

type Quota struct {
	MaxFile, MaxSubmission, MaxPending int64
	Until                              sql.NullInt64
	SetBy                              string
	Custom                             bool
}

func (db *DB) quota(c *Config, member string) Quota {
	q := Quota{MaxFile: c.MaxFile, MaxSubmission: c.MaxSubmission, MaxPending: c.MaxPending}
	var u Quota
	err := db.QueryRow("SELECT max_file, max_submission, max_pending, until, set_by FROM quotas WHERE member = ?",
		member).Scan(&u.MaxFile, &u.MaxSubmission, &u.MaxPending, &u.Until, &u.SetBy)
	if err != nil || (u.Until.Valid && u.Until.Int64 < now()) {
		return q
	}
	u.Custom = true
	return u
}

// pendingBytes is what a member has staged and not yet taken in.
func (db *DB) pendingBytes(member string) int64 {
	var n sql.NullInt64
	db.QueryRow(`SELECT SUM(f.size) FROM files f JOIN submissions s ON s.id = f.submission
	             WHERE s.owner = ? AND s.state NOT IN (?, ?) AND f.status NOT IN (?, ?)`,
		member, StDone, StRejected, FTaken, FSkipped).Scan(&n)
	return n.Int64
}

func (db *DB) submissionBytes(sub int64) int64 {
	var n sql.NullInt64
	db.QueryRow("SELECT SUM(size) FROM files WHERE submission = ?", sub).Scan(&n)
	return n.Int64
}
