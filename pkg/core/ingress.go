package core

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"regexp"
	"sync"
)

// Ingress keys (docs/devices.md): an ingress capability (the device gateway) is the only way in from
// outside, and it holds one credential: a key the core mints at each boot of its worker and hands
// over in the worker's environment. The key reaches /api/ingress/* and nothing else, lives only in
// memory (a new boot, or a core restart, replaces it), and stops working while its worker is asleep.

// IngressKeys are the live ingress keys, one per ingress capability.
type IngressKeys struct {
	mu   sync.Mutex
	byID map[string]ingressKey
}

type ingressKey struct {
	capability string
	hash       string
}

var ingressShape = regexp.MustCompile(`^sti_([0-9a-f]{12})_([0-9a-f]{48})$`)

// NewIngressKeys makes an empty set.
func NewIngressKeys() *IngressKeys { return &IngressKeys{byID: map[string]ingressKey{}} }

// Mint makes a new key for capability, replacing the one it had: `sti_<id>_<secret>`.
func (k *IngressKeys) Mint(capability string) string {
	idb, secret := make([]byte, 6), make([]byte, 24)
	if _, err := rand.Read(idb); err != nil {
		panic(err)
	}
	if _, err := rand.Read(secret); err != nil {
		panic(err)
	}
	id, sec := hex.EncodeToString(idb), hex.EncodeToString(secret)
	k.mu.Lock()
	defer k.mu.Unlock()
	for old, v := range k.byID {
		if v.capability == capability {
			delete(k.byID, old)
		}
	}
	k.byID[id] = ingressKey{capability: capability, hash: hashSecret(sec)}
	return "sti_" + id + "_" + sec
}

// Drop forgets capability's key (its worker went to sleep).
func (k *IngressKeys) Drop(capability string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	for id, v := range k.byID {
		if v.capability == capability {
			delete(k.byID, id)
		}
	}
}

// Capability is the ingress capability a presented key belongs to.
func (k *IngressKeys) Capability(value string) (string, bool) {
	m := ingressShape.FindStringSubmatch(value)
	if m == nil || k == nil {
		return "", false
	}
	k.mu.Lock()
	v, ok := k.byID[m[1]]
	k.mu.Unlock()
	if !ok || subtle.ConstantTimeCompare([]byte(v.hash), []byte(hashSecret(m[2]))) != 1 {
		return "", false
	}
	return v.capability, true
}
