package core

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go, no cgo: the core stays one static binary
)

// core.db (docs/core.md, Data): the core's own records under .swarm/core/, never in git, S3 or
// Slack. It is a persisted format: the schema only moves forward through migrations, and
// `PRAGMA user_version` is how far a file has come. Times are Unix milliseconds.

// DBPath is the core's database under the instance.
func DBPath(root string) string { return filepath.Join(CoreDir(root), "core.db") }

// migrations[i] takes a database from user_version i to i+1. Append only; never edit one that shipped.
var migrations = []string{
	// 1: activity from Hermes' outbound hooks (phase 3). preview can hold client data: 7-day retention.
	`CREATE TABLE activity (
		id       INTEGER PRIMARY KEY,
		agent    TEXT NOT NULL,
		at       INTEGER NOT NULL,
		event    TEXT NOT NULL,
		session  TEXT NOT NULL DEFAULT '',
		tool     TEXT NOT NULL DEFAULT '',
		call     TEXT NOT NULL DEFAULT '',
		preview  TEXT NOT NULL DEFAULT '',
		delivery TEXT NOT NULL UNIQUE
	);
	CREATE INDEX activity_agent_at ON activity(agent, at);`,
	// 2: the agent message bus (phase 5) and findings (rule breaches, phase 7 builds on them).
	`CREATE TABLE messages (
		id       INTEGER PRIMARY KEY,
		thread   INTEGER NOT NULL,
		sender   TEXT NOT NULL,
		audience TEXT NOT NULL,
		subject  TEXT NOT NULL,
		body     TEXT NOT NULL,
		priority TEXT NOT NULL,
		attach   TEXT NOT NULL DEFAULT '[]',
		hop      INTEGER NOT NULL,
		created  INTEGER NOT NULL
	);
	CREATE INDEX messages_thread ON messages(thread);
	CREATE INDEX messages_sender ON messages(sender, created);
	CREATE TABLE deliveries (
		message    INTEGER NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
		agent      TEXT NOT NULL,
		read_at    INTEGER,
		acked_at   INTEGER,
		woken_at   INTEGER,
		wake_tries INTEGER NOT NULL DEFAULT 0,
		wake_error TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (message, agent)
	);
	CREATE INDEX deliveries_agent ON deliveries(agent, read_at);
	CREATE TABLE findings (
		id         INTEGER PRIMARY KEY,
		agent      TEXT NOT NULL,
		rule       TEXT NOT NULL,
		severity   TEXT NOT NULL,
		evidence   TEXT NOT NULL,
		first_seen INTEGER NOT NULL,
		last_seen  INTEGER NOT NULL,
		count      INTEGER NOT NULL DEFAULT 1,
		status     TEXT NOT NULL DEFAULT 'open',
		UNIQUE (agent, rule, evidence)
	);`,
	// 3: the shared workdir (phase 4): what is in it, who changed what, and locks. Paths are
	// "<layer>/<relative path>" under workdir/; agents see them as /shared/<layer>/….
	`CREATE TABLE files (
		path    TEXT PRIMARY KEY,
		size    INTEGER NOT NULL,
		mtime   INTEGER NOT NULL,
		sha256  TEXT NOT NULL DEFAULT '',
		creator TEXT NOT NULL DEFAULT '',
		created INTEGER NOT NULL,
		writer  TEXT NOT NULL DEFAULT '',
		how     TEXT NOT NULL DEFAULT '',
		changed INTEGER NOT NULL
	);
	CREATE TABLE file_events (
		id        INTEGER PRIMARY KEY,
		path      TEXT NOT NULL,
		at        INTEGER NOT NULL,
		kind      TEXT NOT NULL,
		agent     TEXT NOT NULL,
		how       TEXT NOT NULL,
		violation TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX file_events_path ON file_events(path, at);
	CREATE TABLE locks (
		id      INTEGER PRIMARY KEY,
		owner   TEXT NOT NULL,
		pattern TEXT NOT NULL,
		kind    TEXT NOT NULL,
		reason  TEXT NOT NULL DEFAULT '',
		created INTEGER NOT NULL,
		expires INTEGER NOT NULL,
		UNIQUE (owner, pattern)
	);`,
	// 4: learning cycles (phase 6): one row per cycle, one per agent in it. Counts only.
	`CREATE TABLE learn_cycles (
		id       INTEGER PRIMARY KEY,
		trigger  TEXT NOT NULL,
		started  INTEGER NOT NULL,
		finished INTEGER,
		status   TEXT NOT NULL,
		digest   TEXT NOT NULL DEFAULT ''
	);
	CREATE TABLE learn_runs (
		id                INTEGER PRIMARY KEY,
		cycle             INTEGER NOT NULL REFERENCES learn_cycles(id) ON DELETE CASCADE,
		agent             TEXT NOT NULL,
		started           INTEGER NOT NULL,
		finished          INTEGER NOT NULL,
		napped            INTEGER NOT NULL,
		nap_note          TEXT NOT NULL DEFAULT '',
		naps              INTEGER NOT NULL DEFAULT 0,
		new_learnings     INTEGER NOT NULL DEFAULT 0,
		resighted         INTEGER NOT NULL DEFAULT 0,
		skill_proposals   INTEGER NOT NULL DEFAULT 0,
		cron_proposal     INTEGER NOT NULL DEFAULT 0,
		pending_learnings INTEGER NOT NULL DEFAULT 0,
		pending_skills    INTEGER NOT NULL DEFAULT 0,
		error             TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX learn_runs_cycle ON learn_runs(cycle);`,
}

// DB is core.db.
type DB struct {
	*sql.DB
	Path string
}

// OpenDB opens (creating) the database at path and migrates it to the current schema.
func OpenDB(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Created 0600 before SQLite touches it: activity previews and messages are private to this machine.
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		f.Close()
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(wal)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	// One connection: writes are small and serialised anyway, and it keeps WAL readers simple.
	db.SetMaxOpenConns(1)
	d := &DB{DB: db, Path: path}
	if err := d.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("core.db %s: %w", path, err)
	}
	return d, nil
}

// Version is the schema version of the open database.
func (d *DB) Version() (int, error) {
	var v int
	err := d.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}

func (d *DB) migrate() error {
	v, err := d.Version()
	if err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("schema version %d is newer than this stormo understands (%d); upgrade stormo", v, len(migrations))
	}
	for ; v < len(migrations); v++ {
		tx, err := d.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[v]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", v+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, v+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func nowMs() int64 { return time.Now().UnixMilli() }

// Finding is a rule breach the core saw (docs/core.md §7). Repeats of the same agent, rule and
// evidence count up instead of adding rows. Evidence never holds message bodies or file contents.
type Finding struct {
	ID        int64  `json:"id"`
	Agent     string `json:"agent"`
	Rule      string `json:"rule"`
	Severity  string `json:"severity"`
	Evidence  string `json:"evidence"`
	FirstSeen string `json:"firstSeen"`
	LastSeen  string `json:"lastSeen"`
	Count     int    `json:"count"`
	Status    string `json:"status"`
}

// RaiseFinding records (or counts again) a finding.
func (d *DB) RaiseFinding(agent, rule, severity, evidence string, at int64) error {
	_, err := d.Exec(`INSERT INTO findings(agent, rule, severity, evidence, first_seen, last_seen) VALUES(?,?,?,?,?,?)
		ON CONFLICT(agent, rule, evidence) DO UPDATE SET last_seen = excluded.last_seen, count = count + 1, severity = excluded.severity,
		status = CASE WHEN status = 'resolved' THEN 'open' ELSE status END`, agent, rule, severity, evidence, at, at)
	return err
}

// Findings are the open findings, newest first (all when all is true).
func (d *DB) Findings(all bool, limit int) ([]Finding, error) {
	q := `SELECT id, agent, rule, severity, evidence, first_seen, last_seen, count, status FROM findings`
	if !all {
		q += ` WHERE status = 'open'`
	}
	rows, err := d.Query(q+` ORDER BY last_seen DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Finding{}
	for rows.Next() {
		var f Finding
		var first, last int64
		if err := rows.Scan(&f.ID, &f.Agent, &f.Rule, &f.Severity, &f.Evidence, &first, &last, &f.Count, &f.Status); err != nil {
			return nil, err
		}
		f.FirstSeen, f.LastSeen = isoMs(first), isoMs(last)
		out = append(out, f)
	}
	return out, rows.Err()
}

func isoMs(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z") }
