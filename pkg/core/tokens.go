package core

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"
)

// Personal access tokens (docs/remote-core.md §3): named, scoped, optionally expiring, revocable.
// A token is `stc_<id>_<secret>`; core.db keeps the id and sha256(secret) only, so the value is
// shown once, when it is made. The owner token file (.swarm/core/owner.token) stays the first
// admin credential: the local CLI and app use it, and it mints the others.

// Scope is what a principal may do. Each scope includes the ones below it.
type Scope int

const (
	ScopeNone Scope = iota
	// ScopeRead sees everything the core serves to people: fleet, activity, messages, findings,
	// files, review.
	ScopeRead
	// ScopeOperate also acts: review decisions, learning, restarts, lifecycle.
	ScopeOperate
	// ScopeAdmin also manages tokens, secrets and the instance's configuration.
	ScopeAdmin
)

func (s Scope) String() string {
	return [...]string{"none", "read", "operate", "admin"}[s]
}

// ParseScope reads read, operate or admin.
func ParseScope(s string) (Scope, error) {
	switch s {
	case "read":
		return ScopeRead, nil
	case "operate":
		return ScopeOperate, nil
	case "admin":
		return ScopeAdmin, nil
	}
	return ScopeNone, fmt.Errorf("scope is read, operate or admin, not %q", s)
}

var tokenShape = regexp.MustCompile(`^stc_([0-9a-f]{12})_([0-9a-f]{48})$`)

// TokenInfo is a token as listed (never its value).
type TokenInfo struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Scope    string `json:"scope"`
	Created  int64  `json:"created"`
	Expires  *int64 `json:"expires,omitempty"`
	LastUsed *int64 `json:"lastUsed,omitempty"`
	Revoked  *int64 `json:"revoked,omitempty"`
}

var tokenName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._@-]{0,63}$`)

// AddToken makes a token and returns its value (shown once). ttl 0 never expires.
func (d *DB) AddToken(name string, scope Scope, ttl time.Duration) (string, TokenInfo, error) {
	if !tokenName.MatchString(name) || name == "owner" {
		return "", TokenInfo{}, fmt.Errorf("token name %q: letters, digits, . _ @ - (up to 64), and not \"owner\"", name)
	}
	if scope == ScopeNone {
		return "", TokenInfo{}, errors.New("a token needs a scope")
	}
	idb, secret := make([]byte, 6), make([]byte, 24)
	if _, err := rand.Read(idb); err != nil {
		return "", TokenInfo{}, err
	}
	if _, err := rand.Read(secret); err != nil {
		return "", TokenInfo{}, err
	}
	id, sec := hex.EncodeToString(idb), hex.EncodeToString(secret)
	now := time.Now().UnixMilli()
	info := TokenInfo{ID: id, Name: name, Scope: scope.String(), Created: now}
	var exp any
	if ttl > 0 {
		e := now + ttl.Milliseconds()
		info.Expires, exp = &e, e
	}
	if _, err := d.Exec(`INSERT INTO tokens (id, name, scope, hash, created, expires) VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, scope.String(), hashSecret(sec), now, exp); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return "", TokenInfo{}, fmt.Errorf("a token named %q exists; revoke it first", name)
		}
		return "", TokenInfo{}, err
	}
	return "stc_" + id + "_" + sec, info, nil
}

// Tokens lists every token, newest first; revoked ones only with all.
func (d *DB) Tokens(all bool) ([]TokenInfo, error) {
	q := `SELECT id, name, scope, created, expires, last_used, revoked FROM tokens`
	if !all {
		q += ` WHERE revoked IS NULL`
	}
	rows, err := d.Query(q + ` ORDER BY created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TokenInfo{}
	for rows.Next() {
		var t TokenInfo
		var exp, used, rev sql.NullInt64
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &t.Created, &exp, &used, &rev); err != nil {
			return nil, err
		}
		t.Expires, t.LastUsed, t.Revoked = nullable(exp), nullable(used), nullable(rev)
		out = append(out, t)
	}
	return out, rows.Err()
}

func nullable(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

// RevokeToken revokes a token by name or id; it stops working at once.
func (d *DB) RevokeToken(nameOrID string) error {
	res, err := d.Exec(`UPDATE tokens SET revoked = ? WHERE (name = ? OR id = ?) AND revoked IS NULL`, time.Now().UnixMilli(), nameOrID, nameOrID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("no live token named %q", nameOrID)
	}
	return nil
}

// tokenCaller checks a presented token: live, unexpired and the secret's hash matching.
func (d *DB) tokenCaller(value string) (Caller, bool) {
	m := tokenShape.FindStringSubmatch(value)
	if m == nil || d == nil {
		return Caller{}, false
	}
	var name, scope, hash string
	var exp, rev sql.NullInt64
	err := d.QueryRow(`SELECT name, scope, hash, expires, revoked FROM tokens WHERE id = ?`, m[1]).Scan(&name, &scope, &hash, &exp, &rev)
	if err != nil || rev.Valid || subtle.ConstantTimeCompare([]byte(hash), []byte(hashSecret(m[2]))) != 1 {
		return Caller{}, false
	}
	now := time.Now().UnixMilli()
	if exp.Valid && exp.Int64 <= now {
		return Caller{}, false
	}
	sc, err := ParseScope(scope)
	if err != nil {
		return Caller{}, false
	}
	_, _ = d.Exec(`UPDATE tokens SET last_used = ? WHERE id = ?`, now, m[1])
	return Caller{Owner: true, Name: name, Scope: sc}, true
}

func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	At     int64  `json:"at"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Target string `json:"target,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// Audit records what a principal did. Failures to record are logged, never fatal.
func (d *DB) Audit(actor, action, target, detail string) {
	if d == nil {
		return
	}
	_, _ = d.Exec(`INSERT INTO audit (at, actor, action, target, detail) VALUES (?, ?, ?, ?, ?)`,
		time.Now().UnixMilli(), actor, action, target, clip(detail, 500))
}

// AuditLog is the newest entries first.
func (d *DB) AuditLog(limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := d.Query(`SELECT at, actor, action, target, detail FROM audit ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.At, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// fromLoopback: the request reached the loopback listener directly (no proxy in front).
func fromLoopback(r *http.Request) bool {
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if !ok {
		return false
	}
	ta, ok := la.(*net.TCPAddr)
	return ok && ta.IP.IsLoopback()
}
