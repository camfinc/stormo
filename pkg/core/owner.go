package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// The owner token. The core listens on loopback (and on core.listen when set), and agent
// containers reach loopback ports too (OrbStack and Docker Desktop forward host.docker.internal to
// it), so no bind is a boundary: every route but /health needs a credential (access.go). People
// use the owner token, a random value in .swarm/core/owner.token (0600; containers never mount
// .swarm/core) that the CLI sends, or a personal token (tokens.go); an agent uses its own core
// key, which only sees that agent's part.

// OwnerTokenPath is the owner token file under the instance.
func OwnerTokenPath(root string) string { return filepath.Join(CoreDir(root), "owner.token") }

// EnsureOwnerToken reads the owner token, minting it on first use.
func EnsureOwnerToken(root string) (string, error) {
	if t := ReadOwnerToken(root); t != "" {
		return t, nil
	}
	if err := os.MkdirAll(CoreDir(root), 0o700); err != nil {
		return "", err
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	t := hex.EncodeToString(b)
	f, err := os.OpenFile(OwnerTokenPath(root), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, os.ErrExist) {
		return ReadOwnerToken(root), nil // another process minted it first
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(t + "\n"); err != nil {
		return "", err
	}
	return t, nil
}

// ReadOwnerToken is the owner token, or "" when none was minted.
func ReadOwnerToken(root string) string {
	b, err := os.ReadFile(OwnerTokenPath(root))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// Caller is who a request comes from.
type Caller struct {
	// Owner: a person (the owner token or a personal token), at Scope.
	Owner bool
	// Name is the person's principal for the audit log: "owner" or the token's name.
	Name  string
	Scope Scope
	Agent string // set when an agent's core key authenticated it
}

// Can reports whether the caller holds at least the scope.
func (c Caller) Can(s Scope) bool { return c.Owner && c.Scope >= s }

// Actor names the caller in the audit log.
func (c Caller) Actor() string {
	switch {
	case c.Owner:
		return c.Name
	case c.Agent != "":
		return "agent:" + c.Agent
	}
	return "anonymous"
}

func callerOf(r *http.Request, owner string, keys *AgentKeys, db *DB) Caller {
	h := r.Header.Get("Authorization")
	if m := bearer.FindStringSubmatch(h); m != nil {
		if owner != "" && subtle.ConstantTimeCompare([]byte(m[1]), []byte(owner)) == 1 {
			return Caller{Owner: true, Name: "owner", Scope: ScopeAdmin}
		}
		if c, ok := db.tokenCaller(m[1]); ok {
			return c
		}
	}
	if keys != nil {
		if a, ok := keys.AgentFor(h); ok {
			return Caller{Agent: a}
		}
	}
	return Caller{}
}
