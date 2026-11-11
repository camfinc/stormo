package core

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/learning"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/loop"
	"github.com/camfinc/stormo/pkg/manifest"
)

// Linked devices (docs/devices.md): a watch or a phone signs in through the device gateway, an
// ingress capability, while the core stays on loopback. The core keeps the devices (their public
// keys, grants, revocation) and the pairings waiting for the owner, and serves the gateway's
// /api/ingress/* routes: on behalf of a device, each call is checked here against that device's
// grants, whatever the gateway says. The owner links, changes and revokes devices through
// /api/devices/* (the app, `stormo device`).

const (
	pairingTTL        = 10 * time.Minute
	maxPairings       = 20
	maxPairingsPerIP  = 3
	linkFailsAllowed  = 5
	linkFailWindow    = 10 * time.Minute
	deviceTurnLimit   = 10 * time.Minute
	deviceTurnKeep    = 15 * time.Minute
	deviceTouchEvery  = time.Minute
	deviceMessageMax  = 4000
	deviceSummaryMax  = 200
	maxAttestation    = 8
	maxAttestationLen = 8 << 10
)

// userCodeAlphabet is RFC 8628's suggestion: consonants only, no look-alikes, no words.
const userCodeAlphabet = "BCDFGHJKLMNPQRSTVWXZ"

// DeviceGrants are what a linked device may do.
type DeviceGrants struct {
	Status bool `json:"status"`
	Review bool `json:"review"`
	// Chat are the agents it may talk with; "*" is every agent.
	Chat []string `json:"chat"`
}

func (g DeviceGrants) chats(agent string) bool {
	return slices.Contains(g.Chat, "*") || slices.Contains(g.Chat, agent)
}

// JWK is an EC P-256 public key as a device sends it.
type JWK struct {
	Kty string `json:"kty"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// Device is a linked device as the owner sees it (never its key material beyond the fingerprint).
type Device struct {
	ID            string       `json:"id"`
	Name          string       `json:"name"`
	Model         string       `json:"model"`
	OS            string       `json:"os"`
	AppVersion    string       `json:"appVersion"`
	Fingerprint   string       `json:"fingerprint"`
	Grants        DeviceGrants `json:"grants"`
	Created       int64        `json:"created"`
	LastSeen      *int64       `json:"lastSeen"`
	LastIP        string       `json:"lastIp"`
	Country       string       `json:"country"`
	Attestation   int          `json:"attestation"`
	Revoked       *int64       `json:"revoked"`
	RevokedReason string       `json:"revokedReason,omitempty"`
	publicKey     string
}

// Pairing is a device waiting for the owner to type its code.
type Pairing struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Model       string `json:"model"`
	OS          string `json:"os"`
	AppVersion  string `json:"appVersion"`
	IP          string `json:"ip"`
	Country     string `json:"country"`
	Fingerprint string `json:"fingerprint"`
	Created     int64  `json:"created"`
	Expires     int64  `json:"expires"`
}

// deviceKey checks a device's key is a point on P-256 and gives it back in canonical form.
func deviceKey(j JWK) (JWK, *ecdsa.PublicKey, error) {
	if j.Kty != "EC" || j.Crv != "P-256" {
		return JWK{}, nil, errors.New("publicKey is an EC P-256 JWK")
	}
	x, err1 := base64.RawURLEncoding.DecodeString(j.X)
	y, err2 := base64.RawURLEncoding.DecodeString(j.Y)
	if err1 != nil || err2 != nil || len(x) != 32 || len(y) != 32 {
		return JWK{}, nil, errors.New("publicKey x and y are 32 bytes each, base64url without padding")
	}
	pub, err := ecdsa.ParseUncompressedPublicKey(elliptic.P256(), append(append([]byte{4}, x...), y...))
	if err != nil {
		return JWK{}, nil, errors.New("publicKey is not a point on P-256")
	}
	return JWK{Kty: "EC", Crv: "P-256", X: base64.RawURLEncoding.EncodeToString(x), Y: base64.RawURLEncoding.EncodeToString(y)}, pub, nil
}

// KeyFingerprint is what the device and the owner compare: the first 12 hex digits of SHA-256 over
// the key's RFC 7638 thumbprint input, grouped XXXX-XXXX-XXXX.
func KeyFingerprint(j JWK) string {
	h := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + j.X + `","y":"` + j.Y + `"}`))
	s := strings.ToUpper(hex.EncodeToString(h[:]))[:12]
	return s[:4] + "-" + s[4:8] + "-" + s[8:]
}

// NormalizeUserCode is a typed code as stored: upper case letters only.
func NormalizeUserCode(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if r >= 'A' && r <= 'Z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func newUserCode() string {
	out := make([]byte, 0, 8)
	buf := make([]byte, 1)
	for len(out) < 8 {
		if _, err := rand.Read(buf); err != nil {
			panic(err)
		}
		if buf[0] >= 240 { // 240 = 12 × 20: no bias
			continue
		}
		out = append(out, userCodeAlphabet[int(buf[0])%len(userCodeAlphabet)])
	}
	return string(out[:4]) + "-" + string(out[4:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func codeHash(code string) string { return hashSecret("stormo-user-code:" + NormalizeUserCode(code)) }

// --- store ---

// PairingRequest is what the gateway submits for a device asking to pair.
type PairingRequest struct {
	Name        string   `json:"name"`
	Model       string   `json:"model"`
	OS          string   `json:"os"`
	AppVersion  string   `json:"appVersion"`
	PublicKey   JWK      `json:"publicKey"`
	Attestation []string `json:"attestation"`
	IP          string   `json:"ip"`
	Country     string   `json:"country"`
}

func checkText(field, s string, min, max int) error {
	if n := utf8.RuneCountInString(s); n < min || n > max || !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return fmt.Errorf("%s is %d to %d characters, no control characters", field, min, max)
	}
	return nil
}

// AddPairing stores a pending pairing and gives its id and the code the device shows. Expired ones
// go first; then, past the caps, the oldest pending from that IP, then the oldest of all.
func (d *DB) AddPairing(req PairingRequest, now time.Time) (id, code, fingerprint string, err error) {
	for _, c := range []error{checkText("name", req.Name, 1, 64), checkText("model", req.Model, 1, 64), checkText("os", req.OS, 0, 32),
		checkText("appVersion", req.AppVersion, 0, 32), checkText("ip", req.IP, 0, 64), checkText("country", req.Country, 0, 8)} {
		if c != nil {
			return "", "", "", c
		}
	}
	key, _, err := deviceKey(req.PublicKey)
	if err != nil {
		return "", "", "", err
	}
	if len(req.Attestation) > maxAttestation {
		return "", "", "", fmt.Errorf("attestation is at most %d certificates", maxAttestation)
	}
	for _, a := range req.Attestation {
		if b, err := base64.StdEncoding.DecodeString(a); err != nil || len(b) == 0 || len(b) > maxAttestationLen {
			return "", "", "", fmt.Errorf("attestation certificates are base64 DER, up to %d bytes", maxAttestationLen)
		}
	}
	att, _ := json.Marshal(req.Attestation)
	if req.Attestation == nil {
		att = []byte("[]")
	}
	pk, _ := json.Marshal(key)
	fingerprint = KeyFingerprint(key)
	ms := now.UnixMilli()
	tx, err := d.Begin()
	if err != nil {
		return "", "", "", err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM device_pairings WHERE state = 'pending' AND expires <= ?`, ms); err != nil {
		return "", "", "", err
	}
	if _, err := tx.Exec(`DELETE FROM device_pairings WHERE state != 'pending' AND created < ?`, ms-int64(24*time.Hour/time.Millisecond)); err != nil {
		return "", "", "", err
	}
	evict := func(where string, args []any, keep int) error {
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM device_pairings WHERE state = 'pending'`+where, args...).Scan(&n); err != nil {
			return err
		}
		if n >= keep {
			_, err := tx.Exec(`DELETE FROM device_pairings WHERE id IN (SELECT id FROM device_pairings WHERE state = 'pending'`+where+
				` ORDER BY created ASC LIMIT ?)`, append(args, n-keep+1)...)
			return err
		}
		return nil
	}
	if err := evict(` AND ip = ?`, []any{req.IP}, maxPairingsPerIP); err != nil {
		return "", "", "", err
	}
	if err := evict(``, nil, maxPairings); err != nil {
		return "", "", "", err
	}
	for range 5 {
		code = newUserCode()
		var taken int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM device_pairings WHERE code_hash = ? AND state = 'pending'`, codeHash(code)).Scan(&taken); err != nil {
			return "", "", "", err
		}
		if taken == 0 {
			break
		}
		code = ""
	}
	if code == "" {
		return "", "", "", errors.New("no free pairing code; try again")
	}
	id = "pr_" + randomHex(8)
	if _, err := tx.Exec(`INSERT INTO device_pairings (id, code_hash, name, model, os, app_version, public_key, fingerprint, attestation, ip, country, created, expires, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		id, codeHash(code), req.Name, req.Model, req.OS, req.AppVersion, string(pk), fingerprint, string(att), req.IP, req.Country, ms, now.Add(pairingTTL).UnixMilli()); err != nil {
		return "", "", "", err
	}
	return id, code, fingerprint, tx.Commit()
}

// PairingState is what the gateway polls: pending | approved | denied | expired | consumed (and
// gone: a pairing evicted or never made). Reading approved consumes it, so a device code works once.
func (d *DB) PairingState(id string, now time.Time) (state, deviceID, name string, err error) {
	var expires int64
	err = d.QueryRow(`SELECT state, expires, device_id FROM device_pairings WHERE id = ?`, id).Scan(&state, &expires, &deviceID)
	if errors.Is(err, sql.ErrNoRows) {
		return "expired", "", "", nil
	}
	if err != nil {
		return "", "", "", err
	}
	switch {
	case state == "pending" && expires <= now.UnixMilli():
		return "expired", "", "", nil
	case state == "approved":
		res, err := d.Exec(`UPDATE device_pairings SET state = 'consumed' WHERE id = ? AND state = 'approved'`, id)
		if err != nil {
			return "", "", "", err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return "consumed", "", "", nil
		}
		if err := d.QueryRow(`SELECT name FROM devices WHERE id = ?`, deviceID).Scan(&name); err != nil {
			return "", "", "", err
		}
		return "approved", deviceID, name, nil
	case state == "consumed":
		return "consumed", "", "", nil
	}
	return state, "", "", nil
}

const pairingColumns = `id, name, model, os, app_version, ip, country, fingerprint, created, expires`

func scanPairing(row interface{ Scan(...any) error }) (Pairing, error) {
	var p Pairing
	err := row.Scan(&p.ID, &p.Name, &p.Model, &p.OS, &p.AppVersion, &p.IP, &p.Country, &p.Fingerprint, &p.Created, &p.Expires)
	return p, err
}

// PendingPairing finds the pending, unexpired pairing a typed code names.
func (d *DB) PendingPairing(code string, now time.Time) (Pairing, bool, error) {
	p, err := scanPairing(d.QueryRow(`SELECT `+pairingColumns+` FROM device_pairings WHERE code_hash = ? AND state = 'pending' AND expires > ?`,
		codeHash(code), now.UnixMilli()))
	if errors.Is(err, sql.ErrNoRows) {
		return Pairing{}, false, nil
	}
	return p, err == nil, err
}

// PendingPairings are the pairings waiting for the owner, newest first (never their codes).
func (d *DB) PendingPairings(now time.Time) ([]Pairing, error) {
	rows, err := d.Query(`SELECT `+pairingColumns+` FROM device_pairings WHERE state = 'pending' AND expires > ? ORDER BY created DESC`, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Pairing{}
	for rows.Next() {
		p, err := scanPairing(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApprovePairing links the device a pending pairing asked for.
func (d *DB) ApprovePairing(pairing, name string, grants DeviceGrants, now time.Time) (Device, error) {
	tx, err := d.Begin()
	if err != nil {
		return Device{}, err
	}
	defer tx.Rollback()
	var claimed, model, osv, app, pk, fp, att, ip, country string
	err = tx.QueryRow(`SELECT name, model, os, app_version, public_key, fingerprint, attestation, ip, country FROM device_pairings
		WHERE id = ? AND state = 'pending' AND expires > ?`, pairing, now.UnixMilli()).Scan(&claimed, &model, &osv, &app, &pk, &fp, &att, &ip, &country)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, errNotPending
	}
	if err != nil {
		return Device{}, err
	}
	if name == "" {
		name = claimed
	}
	g, _ := json.Marshal(grants)
	id := "dev_" + randomHex(8)
	if _, err := tx.Exec(`INSERT INTO devices (id, name, model, os, app_version, public_key, fingerprint, attestation, grants, created, last_ip, country)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, name, model, osv, app, pk, fp, att, string(g), now.UnixMilli(), ip, country); err != nil {
		return Device{}, err
	}
	if _, err := tx.Exec(`UPDATE device_pairings SET state = 'approved', device_id = ? WHERE id = ?`, id, pairing); err != nil {
		return Device{}, err
	}
	if err := tx.Commit(); err != nil {
		return Device{}, err
	}
	dv, _, err := d.Device(id)
	return dv, err
}

var errNotPending = errors.New("no pending pairing has that code (it expired, was used or was never made)")

// DenyPairing refuses a pending pairing.
func (d *DB) DenyPairing(pairing string) error {
	_, err := d.Exec(`UPDATE device_pairings SET state = 'denied' WHERE id = ? AND state = 'pending'`, pairing)
	return err
}

const deviceColumns = `id, name, model, os, app_version, public_key, fingerprint, attestation, grants, created, last_seen, last_ip, country, revoked, revoked_reason`

func scanDevice(row interface{ Scan(...any) error }) (Device, error) {
	var dv Device
	var att, grants string
	var seen, revoked sql.NullInt64
	if err := row.Scan(&dv.ID, &dv.Name, &dv.Model, &dv.OS, &dv.AppVersion, &dv.publicKey, &dv.Fingerprint, &att, &grants, &dv.Created, &seen, &dv.LastIP,
		&dv.Country, &revoked, &dv.RevokedReason); err != nil {
		return dv, err
	}
	var certs []string
	_ = json.Unmarshal([]byte(att), &certs)
	dv.Attestation = len(certs)
	_ = json.Unmarshal([]byte(grants), &dv.Grants)
	if dv.Grants.Chat == nil {
		dv.Grants.Chat = []string{}
	}
	dv.LastSeen, dv.Revoked = nullable(seen), nullable(revoked)
	return dv, nil
}

// Device finds a device by id (revoked ones too).
func (d *DB) Device(id string) (Device, bool, error) {
	dv, err := scanDevice(d.QueryRow(`SELECT `+deviceColumns+` FROM devices WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, false, nil
	}
	return dv, err == nil, err
}

// Devices are every device, linked first, newest first.
func (d *DB) Devices() ([]Device, error) {
	rows, err := d.Query(`SELECT ` + deviceColumns + ` FROM devices ORDER BY revoked IS NOT NULL, created DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Device{}
	for rows.Next() {
		dv, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, dv)
	}
	return out, rows.Err()
}

// UpdateDevice renames a linked device or changes its grants.
func (d *DB) UpdateDevice(id string, name *string, grants *DeviceGrants) error {
	if name != nil {
		if _, err := d.Exec(`UPDATE devices SET name = ? WHERE id = ? AND revoked IS NULL`, *name, id); err != nil {
			return err
		}
	}
	if grants != nil {
		g, _ := json.Marshal(grants)
		if _, err := d.Exec(`UPDATE devices SET grants = ? WHERE id = ? AND revoked IS NULL`, string(g), id); err != nil {
			return err
		}
	}
	return nil
}

// RevokeDevice unlinks a device at once.
func (d *DB) RevokeDevice(id, reason string, now time.Time) (bool, error) {
	res, err := d.Exec(`UPDATE devices SET revoked = ?, revoked_reason = ? WHERE id = ? AND revoked IS NULL`, now.UnixMilli(), reason, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ExpireIdleDevices unlinks devices unseen (or, never seen, linked) for idle; 0 never.
func (d *DB) ExpireIdleDevices(idle time.Duration, now time.Time) ([]string, error) {
	if idle <= 0 {
		return nil, nil
	}
	cut := now.Add(-idle).UnixMilli()
	rows, err := d.Query(`SELECT id FROM devices WHERE revoked IS NULL AND COALESCE(last_seen, created) < ?`, cut)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if _, err := d.RevokeDevice(id, "idle", now); err != nil {
			return nil, err
		}
	}
	return ids, nil
}

func (d *DB) touchDevice(id, ip, country string, now time.Time) {
	_, _ = d.Exec(`UPDATE devices SET last_seen = ?, last_ip = CASE WHEN ? = '' THEN last_ip ELSE ? END, country = CASE WHEN ? = '' THEN country ELSE ? END
		WHERE id = ? AND revoked IS NULL AND (last_seen IS NULL OR last_seen < ?)`, now.UnixMilli(), ip, ip, country, country, id, now.Add(-deviceTouchEvery).UnixMilli())
}

// LiveDevice is a linked device as an authorization sees it: one idle past idle is unlinked here
// and then (one statement, before any activity is recorded), so it never revives between sweeps.
func (d *DB) LiveDevice(id string, idle time.Duration, now time.Time) (Device, bool, error) {
	if idle > 0 {
		res, err := d.Exec(`UPDATE devices SET revoked = ?, revoked_reason = 'idle' WHERE id = ? AND revoked IS NULL AND COALESCE(last_seen, created) < ?`,
			now.UnixMilli(), id, now.Add(-idle).UnixMilli())
		if err != nil {
			return Device{}, false, err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			d.Audit("core", "device.revoke", id, "idle")
		}
	}
	dv, ok, err := d.Device(id)
	if err != nil || !ok || dv.Revoked != nil {
		return Device{}, false, err
	}
	return dv, true, nil
}

// --- the core's handlers ---

// deviceState is the core's in-memory part: wrong codes typed, and devices' chat turns.
type deviceState struct {
	mu    sync.Mutex
	fails []time.Time
	turns map[string]*deviceTurn
}

type deviceTurn struct {
	ID           string `json:"turn"`
	State        string `json:"state"` // running | done | failed
	Conversation string `json:"conversation"`
	Reply        string `json:"reply,omitempty"`
	Error        string `json:"error,omitempty"`
	device       string
	agent        string
	finished     time.Time
}

const (
	maxDeviceTurns = 500
	maxReplyRunes  = 16000
)

func (c *Core) now() time.Time { return time.Now() }

// idle is how long a device may go unseen before it is unlinked (0: never).
func (c *Core) idle() time.Duration {
	return time.Duration(c.inst.CoreDevicesIdleDays) * 24 * time.Hour
}

// conversationsOf drives an agent's conversations through its engine API (tests replace it).
func (c *Core) conversationsOf(agent string) (engine.Conversations, error) {
	if c.conversations != nil {
		return c.conversations(agent)
	}
	return local.Conversations(c.inst, agent)
}

// decodeStrict reads a body of at most max bytes holding exactly one JSON value with known fields.
func decodeStrict(r *http.Request, max int64, v any) error {
	b, err := io.ReadAll(io.LimitReader(r.Body, max+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > max {
		return errors.New("body too large")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&json.RawMessage{}); err != io.EOF {
		return errors.New("one JSON value")
	}
	return nil
}

func (c *Core) checkGrants(g DeviceGrants) (DeviceGrants, error) {
	if g.Chat == nil {
		g.Chat = []string{}
	}
	agents := manifest.AgentIDs(c.inst.Root)
	for _, a := range g.Chat {
		if a != "*" && !slices.Contains(agents, a) {
			return g, fmt.Errorf("chat: no agent %q", a)
		}
	}
	if slices.Contains(g.Chat, "*") {
		g.Chat = []string{"*"}
	}
	return g, nil
}

var devicePath = regexp.MustCompile(`^/api/devices/(dev_[0-9a-f]{16})$`)

// handleDevices serves the owner's /api/devices/*; needs() already required read (GET) or admin.
func (c *Core) handleDevices(w http.ResponseWriter, r *http.Request, who Caller) {
	w.Header().Set("Cache-Control", "no-store")
	path, now := r.URL.Path, c.now()
	switch {
	case path == "/api/devices" && r.Method == http.MethodGet:
		devs, err := c.DB.Devices()
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read devices"))
			return
		}
		ps, err := c.DB.PendingPairings(now)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read pairings"))
			return
		}
		writeJSONBody(w, 200, map[string]any{"devices": devs, "pairings": ps})
	case path == "/api/devices/link" && r.Method == http.MethodPost:
		var req struct {
			UserCode string        `json:"userCode"`
			Preview  bool          `json:"preview"`
			Deny     bool          `json:"deny"`
			Name     string        `json:"name"`
			Grants   *DeviceGrants `json:"grants"`
		}
		if err := decodeStrict(r, 8<<10, &req); err != nil || len(NormalizeUserCode(req.UserCode)) != 8 {
			writeJSONBody(w, 400, errBody("bad_request", "body is {userCode (8 letters), preview | deny | grants, name}"))
			return
		}
		if c.linkLimited(now) {
			w.Header().Set("Retry-After", "600")
			writeJSONBody(w, 429, errBody("rate_limited", "too many wrong codes: wait 10 minutes"))
			return
		}
		p, ok, err := c.DB.PendingPairing(req.UserCode, now)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read pairings"))
			return
		}
		if !ok {
			c.linkFailed(now)
			writeJSONBody(w, 404, errBody("not_found", errNotPending.Error()))
			return
		}
		switch {
		case req.Preview:
			writeJSONBody(w, 200, map[string]any{"pairing": p})
		case req.Deny:
			if err := c.DB.DenyPairing(p.ID); err != nil {
				writeJSONBody(w, 500, errBody("core_internal", "could not deny"))
				return
			}
			c.DB.Audit(who.Actor(), "device.deny", p.ID, p.Name+" "+p.Model+" "+p.Fingerprint)
			writeJSONBody(w, 200, map[string]any{"denied": p.ID})
		default:
			if req.Grants == nil {
				writeJSONBody(w, 400, errBody("bad_request", "linking needs grants {status, review, chat}"))
				return
			}
			g, err := c.checkGrants(*req.Grants)
			if err == nil && req.Name != "" {
				err = checkText("name", req.Name, 1, 64)
			}
			if err != nil {
				writeJSONBody(w, 400, errBody("bad_request", err.Error()))
				return
			}
			dv, err := c.DB.ApprovePairing(p.ID, req.Name, g, now)
			if errors.Is(err, errNotPending) {
				writeJSONBody(w, 404, errBody("not_found", err.Error()))
				return
			}
			if err != nil {
				writeJSONBody(w, 500, errBody("core_internal", "could not link the device"))
				return
			}
			c.DB.Audit(who.Actor(), "device.link", dv.ID, fmt.Sprintf("%s %s %s grants=%s", dv.Name, dv.Model, dv.Fingerprint, grantsText(g)))
			writeJSONBody(w, 201, map[string]any{"device": dv})
		}
	case devicePath.MatchString(path) && r.Method == http.MethodPatch:
		id := devicePath.FindStringSubmatch(path)[1]
		var req struct {
			Name   *string       `json:"name"`
			Grants *DeviceGrants `json:"grants"`
		}
		if err := decodeStrict(r, 8<<10, &req); err != nil {
			writeJSONBody(w, 400, errBody("bad_request", "body is {name, grants}"))
			return
		}
		dv, ok, err := c.DB.Device(id)
		if err != nil || !ok || dv.Revoked != nil {
			writeJSONBody(w, 404, errBody("not_found", "no linked device "+id))
			return
		}
		if req.Name != nil {
			if err := checkText("name", *req.Name, 1, 64); err != nil {
				writeJSONBody(w, 400, errBody("bad_request", err.Error()))
				return
			}
		}
		if req.Grants != nil {
			g, err := c.checkGrants(*req.Grants)
			if err != nil {
				writeJSONBody(w, 400, errBody("bad_request", err.Error()))
				return
			}
			req.Grants = &g
		}
		if err := c.DB.UpdateDevice(id, req.Name, req.Grants); err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not change the device"))
			return
		}
		dv, _, _ = c.DB.Device(id)
		c.DB.Audit(who.Actor(), "device.change", id, dv.Name+" grants="+grantsText(dv.Grants))
		writeJSONBody(w, 200, map[string]any{"device": dv})
	case devicePath.MatchString(path) && r.Method == http.MethodDelete:
		id := devicePath.FindStringSubmatch(path)[1]
		ok, err := c.DB.RevokeDevice(id, "owner", now)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not revoke"))
			return
		}
		if !ok {
			writeJSONBody(w, 404, errBody("not_found", "no linked device "+id))
			return
		}
		c.DB.Audit(who.Actor(), "device.revoke", id, "")
		writeJSONBody(w, 200, map[string]any{"revoked": id})
	default:
		writeJSONBody(w, 404, errBody("not_found", r.Method+" "+path))
	}
}

func grantsText(g DeviceGrants) string {
	parts := []string{}
	if g.Status {
		parts = append(parts, "status")
	}
	if g.Review {
		parts = append(parts, "review")
	}
	if len(g.Chat) > 0 {
		parts = append(parts, "chat:"+strings.Join(g.Chat, ","))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, " ")
}

func (c *Core) linkLimited(now time.Time) bool {
	c.devices.mu.Lock()
	defer c.devices.mu.Unlock()
	keep := c.devices.fails[:0]
	for _, t := range c.devices.fails {
		if now.Sub(t) < linkFailWindow {
			keep = append(keep, t)
		}
	}
	c.devices.fails = keep
	return len(keep) >= linkFailsAllowed
}

func (c *Core) linkFailed(now time.Time) {
	c.devices.mu.Lock()
	c.devices.fails = append(c.devices.fails, now)
	c.devices.mu.Unlock()
}

var (
	ingressPairingPath = regexp.MustCompile(`^/api/ingress/pairings/(pr_[0-9a-f]{16})$`)
	ingressDevicePath  = regexp.MustCompile(`^/api/ingress/devices/(dev_[0-9a-f]{16})$`)
	ingressConvPath    = regexp.MustCompile(`^/api/ingress/agents/([a-z][a-z0-9-]*)/conversations(?:/([A-Za-z0-9._:-]{1,128}))?$`)
	ingressChatPath    = regexp.MustCompile(`^/api/ingress/agents/([a-z][a-z0-9-]*)/chat$`)
	ingressTurnPath    = regexp.MustCompile(`^/api/ingress/turns/(trn_[0-9a-f]{16})$`)
	deviceIDShape      = regexp.MustCompile(`^dev_[0-9a-f]{16}$`)
	conversationShape  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
	reviewVersionShape = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

// handleIngress serves the device gateway's /api/ingress/*; needs() already required its key.
func (c *Core) handleIngress(w http.ResponseWriter, r *http.Request, who Caller, roster func() []BusAgent) {
	w.Header().Set("Cache-Control", "no-store")
	path, now := r.URL.Path, c.now()
	get, post := r.Method == http.MethodGet, r.Method == http.MethodPost
	switch {
	case path == "/api/ingress/pairings" && post:
		var req PairingRequest
		if err := decodeStrict(r, 96<<10, &req); err != nil {
			writeJSONBody(w, 400, errBody("bad_request", "body is {name, model, os, appVersion, publicKey, attestation, ip, country}"))
			return
		}
		id, code, fp, err := c.DB.AddPairing(req, now)
		if err != nil {
			writeJSONBody(w, 400, errBody("bad_request", err.Error()))
			return
		}
		writeJSONBody(w, 201, map[string]any{"pairing": id, "userCode": code, "fingerprint": fp, "expiresIn": int(pairingTTL.Seconds())})
		return
	case ingressPairingPath.MatchString(path) && get:
		state, dev, name, err := c.DB.PairingState(ingressPairingPath.FindStringSubmatch(path)[1], now)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read the pairing"))
			return
		}
		out := map[string]any{"state": state}
		if state == "approved" {
			out["deviceId"], out["name"] = dev, name
		}
		writeJSONBody(w, 200, out)
		return
	case ingressDevicePath.MatchString(path) && get:
		dv, ok, err := c.DB.LiveDevice(ingressDevicePath.FindStringSubmatch(path)[1], c.idle(), now)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read the device"))
			return
		}
		if !ok {
			writeJSONBody(w, 401, errBody("device_unknown", "this device is not linked"))
			return
		}
		writeJSONBody(w, 200, map[string]any{"id": dv.ID, "publicKey": json.RawMessage(dv.publicKey)})
		return
	}
	// Everything else is on behalf of a linked device, checked here against its grants.
	id := r.Header.Get("X-Stormo-Device")
	if !deviceIDShape.MatchString(id) {
		writeJSONBody(w, 401, errBody("device_unknown", "X-Stormo-Device names no device"))
		return
	}
	dv, ok, err := c.DB.LiveDevice(id, c.idle(), now)
	if err != nil {
		writeJSONBody(w, 500, errBody("core_internal", "could not read the device"))
		return
	}
	if !ok {
		writeJSONBody(w, 401, errBody("device_unknown", "this device is not linked"))
		return
	}
	who.Device, who.DeviceName = dv.ID, dv.Name
	ip, country := clip(r.Header.Get("X-Stormo-Client-Ip"), 64), clip(r.Header.Get("X-Stormo-Client-Country"), 8)
	c.DB.touchDevice(dv.ID, ip, country, now)
	forbid := func(what string) {
		writeJSONBody(w, 403, errBody("forbidden", "this device's grants do not cover "+what))
	}
	switch {
	case path == "/api/ingress/me" && get:
		writeJSONBody(w, 200, map[string]any{"device": map[string]any{"id": dv.ID, "name": dv.Name, "model": dv.Model, "grants": dv.Grants, "linked": dv.Created},
			"instance": c.inst.Name})
	case path == "/api/ingress/fleet" && get:
		if !dv.Grants.Status {
			forbid("status")
			return
		}
		writeJSONBody(w, 200, c.deviceFleet())
	case path == "/api/ingress/review" && get:
		if !dv.Grants.Review {
			forbid("review")
			return
		}
		items, err := c.deviceReview(roster)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read the review"))
			return
		}
		writeJSONBody(w, 200, map[string]any{"items": items})
	case path == "/api/ingress/review" && post:
		if !dv.Grants.Review {
			forbid("review")
			return
		}
		var req struct {
			Agent    string `json:"agent"`
			Kind     string `json:"kind"`
			ID       string `json:"id"`
			Version  string `json:"version"`
			Decision string `json:"decision"`
		}
		if err := decodeStrict(r, 4<<10, &req); err != nil || (req.Decision != "accept" && req.Decision != "reject") || !reviewVersionShape.MatchString(req.Version) {
			writeJSONBody(w, 400, errBody("bad_request", "body is {agent, kind, id, version, decision: accept | reject}"))
			return
		}
		items, err := c.deviceReview(roster)
		if err != nil {
			writeJSONBody(w, 500, errBody("core_internal", "could not read the review"))
			return
		}
		i := slices.IndexFunc(items, func(it deviceReviewItem) bool { return it.Agent == req.Agent && it.Kind == req.Kind && it.ID == req.ID })
		if i < 0 {
			writeJSONBody(w, 404, errBody("not_found", "no open proposal "+req.Kind+" "+req.ID))
			return
		}
		if items[i].Version != req.Version {
			writeJSONBody(w, 409, errBody("changed", errChanged.Error()))
			return
		}
		if items[i].AppOnly {
			writeJSONBody(w, 400, errBody("app_only", "decide this one in the app: "+items[i].Why))
			return
		}
		if err := c.deviceDecide(req.Agent, req.Kind, req.ID, req.Version, req.Decision); errors.Is(err, errChanged) {
			writeJSONBody(w, 409, errBody("changed", err.Error()))
			return
		} else if errors.Is(err, learning.ErrLedgerBusy) {
			writeJSONBody(w, 409, errBody("busy", err.Error()))
			return
		} else if err != nil {
			writeJSONBody(w, 400, errBody("bad_request", err.Error()))
			return
		}
		c.DB.Audit(who.Actor(), "review."+req.Decision, req.Agent, req.Kind+" "+req.ID)
		writeJSONBody(w, 200, map[string]any{"ok": true})
	case ingressConvPath.MatchString(path) && get:
		m := ingressConvPath.FindStringSubmatch(path)
		if !c.deviceMayChat(w, dv, m[1]) {
			return
		}
		conv, err := c.conversationsOf(m[1])
		if err != nil {
			writeJSONBody(w, 503, errBody("agent_unreachable", "the agent's conversations are not reachable now"))
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		if m[2] == "" {
			list, err := conv.List(ctx, 20)
			if err != nil {
				writeJSONBody(w, 503, errBody("agent_unreachable", "the agent did not answer"))
				return
			}
			out := []map[string]any{}
			for _, cv := range list {
				out = append(out, map[string]any{"id": cv.ID, "title": cv.Title, "preview": clip(cv.Preview, deviceSummaryMax), "lastActive": cv.LastActive, "messages": cv.Messages})
			}
			writeJSONBody(w, 200, map[string]any{"conversations": out})
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 100 {
			limit = 30
		}
		tr, err := conv.Transcript(ctx, m[2], 500)
		if errors.Is(err, engine.ErrNotFound) {
			writeJSONBody(w, 404, errBody("not_found", "no conversation "+m[2]))
			return
		}
		if err != nil {
			writeJSONBody(w, 503, errBody("agent_unreachable", "the agent did not answer"))
			return
		}
		msgs := []map[string]any{}
		for _, msg := range tr.Messages {
			if msg.Kind != engine.MessageText || (msg.Role != "user" && msg.Role != "assistant") {
				continue
			}
			msgs = append(msgs, map[string]any{"id": msg.ID, "role": msg.Role, "content": clipRunes(msg.Content, deviceMessageMax), "at": msg.At})
		}
		if len(msgs) > limit {
			msgs = msgs[len(msgs)-limit:]
		}
		writeJSONBody(w, 200, map[string]any{"id": tr.ID, "compacted": tr.Compacted, "messages": msgs})
	case ingressChatPath.MatchString(path) && post:
		agent := ingressChatPath.FindStringSubmatch(path)[1]
		if !c.deviceMayChat(w, dv, agent) {
			return
		}
		var req struct {
			Conversation string `json:"conversation"`
			Message      string `json:"message"`
		}
		if err := decodeStrict(r, 32<<10, &req); err != nil || checkChat(req.Message) != nil ||
			(req.Conversation != "" && !conversationShape.MatchString(req.Conversation)) {
			writeJSONBody(w, 400, errBody("bad_request", "body is {conversation?, message (1 to 4000 characters)}"))
			return
		}
		t, err := c.startDeviceTurn(who, agent, req.Conversation, req.Message)
		if errors.Is(err, errDeviceBusy) {
			writeJSONBody(w, 409, errBody("busy", err.Error()))
			return
		}
		if err != nil {
			writeJSONBody(w, 503, errBody("agent_unreachable", clip(err.Error(), 200)))
			return
		}
		writeJSONBody(w, 202, map[string]any{"turn": t.ID, "conversation": t.Conversation})
	case ingressTurnPath.MatchString(path) && get:
		t, ok, allowed := c.deviceTurn(dv, ingressTurnPath.FindStringSubmatch(path)[1])
		if !ok {
			writeJSONBody(w, 404, errBody("not_found", "no such turn"))
			return
		}
		if !allowed {
			forbid("chat with " + t.agent)
			return
		}
		writeJSONBody(w, 200, t)
	default:
		writeJSONBody(w, 404, errBody("not_found", r.Method+" "+path))
	}
}

func checkChat(s string) error {
	if n := utf8.RuneCountInString(strings.TrimSpace(s)); n == 0 || n > deviceMessageMax || !utf8.ValidString(s) {
		return errors.New("message is 1 to 4000 characters")
	}
	return nil
}

func clipRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}

func (c *Core) deviceMayChat(w http.ResponseWriter, dv Device, agent string) bool {
	if !slices.Contains(manifest.AgentIDs(c.inst.Root), agent) {
		writeJSONBody(w, 404, errBody("not_found", "no agent "+agent))
		return false
	}
	if !dv.Grants.chats(agent) {
		writeJSONBody(w, 403, errBody("forbidden", "this device's grants do not cover chat with "+agent))
		return false
	}
	return true
}

// deviceFleetAgent is an agent as a device sees it.
type deviceFleetAgent struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Unit          string  `json:"unit"`
	State         string  `json:"state"`
	Busy          bool    `json:"busy"`
	Live          *string `json:"live"`
	Waiting       int     `json:"waiting"`
	Unread        int     `json:"unread"`
	PendingReview int     `json:"pendingReview"`
}

func (c *Core) deviceFleet() map[string]any {
	fs := FleetSnapshot{Agents: []FleetAgent{}}
	if c.Fleet != nil {
		fs = c.Fleet.Snapshot()
	}
	counts, _ := c.Bus.Counts()
	out := []deviceFleetAgent{}
	for _, a := range fs.Agents {
		v := deviceFleetAgent{ID: a.ID, Name: a.Name, Unit: a.Unit, State: a.State, Unread: counts[a.ID].Unread,
			PendingReview: a.PendingLearnings + a.PendingSkills + a.PendingChanges}
		if l := c.Monitor.Live(a.ID); l != nil {
			v.Busy, v.Waiting = l.Running > 0, l.Waiting
			if l.Tool != "" {
				tool := l.Tool
				v.Live = &tool
			}
		}
		out = append(out, v)
	}
	return map[string]any{"agents": out, "polledAt": fs.PolledAt}
}

// deviceReviewItem is one open proposal as a device sees it.
type deviceReviewItem struct {
	Agent     string `json:"agent"`
	AgentName string `json:"agentName"`
	Kind      string `json:"kind"` // lesson | setting | skill | schedules
	ID        string `json:"id"`
	// Version names exactly what was shown: a decision sent with another is refused (changed).
	Version string    `json:"version"`
	Summary string    `json:"summary"`
	AI      *ReviewAI `json:"ai"`
	AppOnly bool      `json:"appOnly"`
	Why     string    `json:"why,omitempty"`
}

// deviceReview lists the open proposals. A device decides lessons the agent keeps to itself (not
// about a person, nothing the scrubber flagged) and settings that neither widen what the agent may
// do nor carry a flag; skills, schedules and scripts, and everything else, are decided in the app by
// a person reading them in full.
func (c *Core) deviceReview(roster func() []BusAgent) ([]deviceReviewItem, error) {
	byID := map[string]BusAgent{}
	for _, a := range roster() {
		byID[a.ID] = a
	}
	v, err := c.Learner.review(byID)
	if err != nil {
		return nil, err
	}
	items := []deviceReviewItem{}
	for _, a := range v.Agents {
		add := func(kind, id, version, summary string, ai *ReviewAI, why string) {
			items = append(items, deviceReviewItem{Agent: a.ID, AgentName: a.Name, Kind: kind, ID: id, Version: version, Summary: clipRunes(summary, deviceSummaryMax),
				AI: ai, AppOnly: why != "", Why: why})
		}
		for _, l := range a.Lessons {
			if l.Status != string(learning.Proposed) {
				continue
			}
			add("lesson", l.ID, lessonVersion(l.ID, l.Kind, l.Scope, l.Status, l.Text, l.PII), l.Text, l.AI,
				lessonAppOnly(l.Kind, l.Scope, l.PII))
		}
		for _, s := range a.Settings {
			summary := s.Key + ": " + s.Current + " → " + s.Value
			if s.Sensitive || s.Opaque {
				summary = "Setting " + s.Key
			}
			add("setting", s.Key, settingVersion(s.SettingProposal), summary, s.AI, settingAppOnly(s.SettingProposal))
		}
		for _, s := range a.Skills {
			what := "Change to skill "
			if s.New {
				what = "New skill "
			}
			add("skill", s.Skill, versionOf("skill", a.ID, s.Skill, s.Nap), fmt.Sprintf("%s%s (%d files)", what, s.Skill, len(s.Files)), s.AI,
				"skills are decided in the app")
		}
		if a.Schedules != nil {
			add("schedules", "schedules", versionOf("schedules", a.ID, a.Schedules.Nap),
				fmt.Sprintf("Schedules: %d jobs, %d scripts", len(a.Schedules.Jobs), len(a.Schedules.Scripts)), a.Schedules.AI,
				"schedules and scripts are decided in the app")
		}
	}
	return items, nil
}

// lessonAppOnly is why a lesson is decided in the app, "" when a device may decide it.
func lessonAppOnly(kind, scope string, pii []string) string {
	switch {
	case kind == string(learning.KindUser):
		return "it is about a person"
	case len(pii) > 0:
		return "the scrubber flagged " + strings.Join(pii, ", ")
	case scope != string(manifest.ScopeAgent):
		return "it is shared beyond the agent (" + scope + ")"
	}
	return ""
}

// settingAppOnly is why a setting is decided in the app, "" when a device may decide it.
func settingAppOnly(p loop.SettingProposal) string {
	switch {
	case p.Sensitive || p.Opaque:
		return "it widens what the agent may do"
	case len(p.PII) > 0:
		return "the scrubber flagged " + strings.Join(p.PII, ", ")
	}
	return ""
}

func versionOf(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(h[:8])
}

func lessonVersion(id, kind, scope, status, text string, pii []string) string {
	return versionOf("lesson", id, kind, scope, status, text, strings.Join(pii, ","))
}

func settingVersion(p loop.SettingProposal) string {
	return versionOf("setting", p.Key, p.Value, p.Current, strconv.FormatBool(p.Sensitive), strconv.FormatBool(p.Opaque), strings.Join(p.PII, ","))
}

var errChanged = errors.New("the proposal changed since it was shown: reload the review")

// deviceDecide applies a device's decision to exactly the proposal it was shown: the lesson by its
// exact id, checked again in the same load and save as the write; the setting by its key, checked
// again just before. A lesson that changed (flagged, shared, rewritten) is refused.
func (c *Core) deviceDecide(agent, kind, id, version, decision string) error {
	if kind == "lesson" {
		status := learning.Accepted
		if decision == "reject" {
			status = learning.Rejected
		}
		_, err := loop.DecideLesson(c.inst.Root, agent, id, status, func(e *learning.Learning) error {
			if e.Status != learning.Proposed || lessonAppOnly(string(e.Kind), string(e.Scope), e.PII) != "" ||
				lessonVersion(e.ID, string(e.Kind), string(e.Scope), string(e.Status), e.Text, e.PII) != version {
				return errChanged
			}
			return nil
		})
		if errors.Is(err, loop.ErrNoLesson) {
			return errChanged // decided or gone since it was shown
		}
		if err == nil && decision == "accept" {
			c.Learner.markUnapplied(agent)
		}
		return err
	}
	dec := map[string]string{"accept": "accepted", "reject": "rejected"}[decision]
	_, err := loop.DecideSettingIf(c.inst, agent, id, dec, loop.Decider{}, func(p loop.SettingProposal) error {
		if settingAppOnly(p) != "" || settingVersion(p) != version {
			return errChanged
		}
		return nil
	})
	if err != nil {
		if !errors.Is(err, errChanged) && !errors.Is(err, learning.ErrLedgerBusy) && strings.Contains(err.Error(), "no setting proposal") {
			return errChanged
		}
		return err
	}
	c.Learner.markUnapplied(agent)
	return nil
}

var errDeviceBusy = errors.New("this device already has a turn running")

// startDeviceTurn runs one chat turn for a device in the background: one at a time per device, the
// slot taken before anything else so two requests at once never both run.
func (c *Core) startDeviceTurn(who Caller, agent, conversation, message string) (deviceTurn, error) {
	t := &deviceTurn{ID: "trn_" + randomHex(8), State: "running", device: who.Device, agent: agent}
	c.devices.mu.Lock()
	c.pruneTurns()
	for _, o := range c.devices.turns {
		if o.device == who.Device && o.State == "running" {
			c.devices.mu.Unlock()
			return deviceTurn{}, errDeviceBusy
		}
	}
	if c.devices.turns == nil {
		c.devices.turns = map[string]*deviceTurn{}
	}
	c.devices.turns[t.ID] = t
	c.devices.mu.Unlock()
	release := func() {
		c.devices.mu.Lock()
		delete(c.devices.turns, t.ID)
		c.devices.mu.Unlock()
	}
	conv, err := c.conversationsOf(agent)
	if err != nil {
		release()
		return deviceTurn{}, errors.New("the agent's conversations are not reachable now")
	}
	if conversation == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		conversation, err = conv.New(ctx, "", "")
		cancel()
		if err != nil {
			release()
			return deviceTurn{}, errors.New("could not start a conversation")
		}
	}
	c.devices.mu.Lock()
	t.Conversation = conversation
	view := *t
	c.devices.mu.Unlock()
	c.DB.Audit(who.Actor(), "device.chat", agent, "conversation "+conversation)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), deviceTurnLimit)
		defer cancel()
		reply, tip, err := conv.Send(ctx, conversation, message)
		c.devices.mu.Lock()
		defer c.devices.mu.Unlock()
		t.finished = time.Now()
		if err != nil {
			t.State, t.Error = "failed", clip(err.Error(), 300)
			return
		}
		t.State, t.Reply = "done", clipRunes(reply, maxReplyRunes)
		if tip != "" {
			t.Conversation = tip
		}
		c.pruneTurns() // the bound holds as turns finish, not only at the next start
	}()
	return view, nil
}

// pruneTurns drops turns finished long enough ago, and past maxDeviceTurns the oldest finished;
// the caller holds c.devices.mu.
func (c *Core) pruneTurns() {
	for id, t := range c.devices.turns {
		if t.State != "running" && time.Since(t.finished) > deviceTurnKeep {
			delete(c.devices.turns, id)
		}
	}
	for len(c.devices.turns) > maxDeviceTurns {
		oldest := ""
		for id, t := range c.devices.turns {
			if t.State != "running" && (oldest == "" || t.finished.Before(c.devices.turns[oldest].finished)) {
				oldest = id
			}
		}
		if oldest == "" {
			return
		}
		delete(c.devices.turns, oldest)
	}
}

// PruneDeviceTurns drops finished replies past their time, with no device traffic needed (the core
// calls it every minute).
func (c *Core) PruneDeviceTurns() {
	c.devices.mu.Lock()
	defer c.devices.mu.Unlock()
	c.pruneTurns()
}

// deviceTurn is one of device's turns (found), and whether its grants still cover the agent it
// talks with (allowed: only then is the turn's content given).
func (c *Core) deviceTurn(dv Device, id string) (t deviceTurn, found, allowed bool) {
	c.devices.mu.Lock()
	defer c.devices.mu.Unlock()
	c.pruneTurns()
	tn, ok := c.devices.turns[id]
	if !ok || tn.device != dv.ID {
		return deviceTurn{}, false, false
	}
	if !dv.Grants.chats(tn.agent) {
		return deviceTurn{agent: tn.agent}, true, false
	}
	return *tn, true, true
}
