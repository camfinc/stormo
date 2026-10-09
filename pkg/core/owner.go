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

// The owner token. The core binds to loopback, but agent containers reach loopback ports too
// (OrbStack and Docker Desktop forward host.docker.internal to it), so the bind is no boundary
// for reads that return message bodies, activity previews or file history. Those need either
// the owner token, a random value in .swarm/core/owner.token (0600; containers never mount
// .swarm/core) that the CLI sends, or an agent's own core key, which only sees that agent's part.

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
	Owner bool
	Agent string // set when an agent's core key authenticated it
}

func callerOf(r *http.Request, owner string, keys *AgentKeys) Caller {
	h := r.Header.Get("Authorization")
	if m := bearer.FindStringSubmatch(h); m != nil && owner != "" && subtle.ConstantTimeCompare([]byte(m[1]), []byte(owner)) == 1 {
		return Caller{Owner: true}
	}
	if keys != nil {
		if a, ok := keys.AgentFor(h); ok {
			return Caller{Agent: a}
		}
	}
	return Caller{}
}
