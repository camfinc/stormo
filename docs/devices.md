# Linked devices

A linked device (a watch, a phone) signs in once and then reaches the fleet from anywhere, without
the core ever being exposed. The only thing reachable from the internet is the **device gateway**: a
small service capability (template `device-gateway`, docs/capabilities.md) that is always on, never
offered to agents, and holds one credential: a key, minted by the core at each boot, that reaches
`/api/ingress/*` and nothing else. The core stays on loopback, keeps the list of linked devices and
checks every call a device makes against that device's grants.

```
 device ──HTTPS──► your edge (tunnel, proxy) ──► 127.0.0.1:<port> ─► device gateway (container)
                                                                       /device/v1/* only
                                                                         │ ingress key, typed calls
                                                                         ▼
 owner: app / CLI (local) ──► /api/devices/*                       core (loopback only)
                                                                    /api/ingress/*: ingress key only
```

## Setting it up

```yaml
core:
  loopback_trust: false          # required: the gateway container reaches the core's loopback port
  devices:
    idle_days: 30                # a device unseen this long is unlinked (0: never)

capabilities:
  devices.gateway:
    template: device-gateway
    publish: "127.0.0.1:18650"   # the stable address your tunnel or proxy points at (loopback only)
    env:
      CLIENT_IP_HEADER: Cf-Connecting-Ip      # the header your edge puts the client's IP in ("" : the peer)
      CLIENT_COUNTRY_HEADER: Cf-Ipcountry     # optional
```

- The core boots the gateway when it starts and keeps it running. `stormo capability sleep
  devices.gateway` takes every device offline until `stormo capability wake devices.gateway`; a wake
  mints a new ingress key, so a stolen one is dead.
- Point a tunnel or reverse proxy at the `publish` address: Cloudflare Tunnel (`cloudflared tunnel
  --url http://127.0.0.1:18650`, or a named tunnel), Tailscale Funnel (`tailscale funnel 18650`), Caddy.
  Serve nothing else on that hostname, allow only `/device/v1/`, rate-limit `/device/v1/pair*`, and
  cache nothing. Never point a tunnel at the core's own port.
- A Cloudflare or other edge that terminates TLS sees the traffic in clear, chats included. Tailscale
  Funnel terminates TLS on the host.

## Threat model

| If an attacker has | they can | they cannot |
|---|---|---|
| the internet | create pending pairings (rate-limited, evicted oldest first) | link a device: the owner types the code shown on the device |
| a device's session token (copied in transit) | act as that device until the token expires (`TOKEN_MINUTES`, default 60) | mint a new one: that needs the device's hardware key |
| a lost device, locked | nothing: the key signs only while the device is unlocked | |
| a lost device, unlocked | act as it within its grants until the owner revokes it | reach anything outside its grants |
| the gateway (a compromise) | act as any linked device within that device's grants; submit pairings | reach the operator API, secrets, ChatGPT sign-ins, agent keys, files, lifecycle, skills or scripts. This holds only with `core.loopback_trust: false` (enforced) |

Recovery: revoke the device (`stormo device revoke`), or sleep the gateway (every device offline,
the ingress key dies with it).

## Grants

Each device gets grants when it is linked; the owner can change them later.

```json
{"status": true, "review": true, "chat": ["support"]}
```

- `status`: the compact fleet view.
- `review`: proposals, and **accept or reject** (never promote) on **lessons** the agent keeps to itself
  (not about a person, nothing the scrubber flagged) and **settings** that neither widen what the
  agent may do nor carry a flag. Skills, schedules and scripts, and everything else, are shown with
  `appOnly: true` and a `why`: they are decided in the app, by a person reading them in full.
- `chat`: the agents it may talk with; `["*"]` is every agent, `[]` none.

Nothing else is reachable by a device: no lifecycle, secrets, config, files, tokens, capabilities or
learning runs.

## The device API (the gateway, `/device/v1/*`)

JSON in and out; request bodies at most 16 KB (a pairing request, with its attestation chain, 96 KB),
unknown fields refused. Every answer carries
`Cache-Control: no-store`. Errors are `{"error": {"code": "...", "message": "..."}}`; the codes a
client branches on:

| status | code | meaning, what to do |
|---|---|---|
| 400 | `bad_request` | the body is wrong; fix the client |
| 400 | `authorization_pending` | pairing: not approved yet; poll again after `interval` |
| 400 | `slow_down` | pairing: polled too fast; add 5 s to the interval |
| 400 | `expired_token` | pairing: the code expired or was used; start again |
| 400 | `access_denied` | pairing: the owner refused it |
| 400 | `expired_nonce` | session: the challenge expired or was used; ask for a new one |
| 400 | `app_only` | review: this item is decided in the app |
| 401 | `unauthorized` | no session token, or it expired: open a new session |
| 401 | `bad_signature` | session: the signature does not verify with the linked key |
| 401 | `device_unknown` | the device is not linked (revoked, expired, never linked): pair again |
| 403 | `forbidden` | the device's grants do not cover this |
| 404 | `not_found` | no such route, agent, conversation, turn or review item |
| 409 | `busy` | chat: this device already has a turn running |
| 429 | `rate_limited` | too many requests; wait `Retry-After` seconds |
| 503 | `core_unreachable` | the core did not answer; retry later |

### Pairing (RFC 8628, device authorization grant)

The device makes an EC P-256 key pair it can't export (Android Keystore: `KeyProperties.KEY_ALGORITHM_EC`,
`secp256r1`, `PURPOSE_SIGN`, `DIGEST_SHA256`, `setUnlockedDeviceRequired(true)`).

`POST /device/v1/pair`

```json
{"name": "Watch", "model": "Pixel Watch 3", "os": "Wear OS 6", "appVersion": "0.1.0",
 "publicKey": {"kty": "EC", "crv": "P-256", "x": "<base64url, 32 bytes>", "y": "<base64url, 32 bytes>"},
 "attestation": ["<base64 DER certificate>", "…"]}
```

`name`, `model` 1–64 characters, `os`, `appVersion` up to 32; `attestation` is optional (up to 8
certificates, each up to 8 KB, leaf first) and recorded, not verified yet.

→ 200

```json
{"deviceCode": "<base64url, 32 bytes>", "userCode": "WDJB-MJHT", "fingerprint": "3F2A-91C0-7B4E",
 "expiresIn": 600, "interval": 5}
```

- `userCode`: 8 letters from `BCDFGHJKLMNPQRSTVWXZ`, shown as `XXXX-XXXX`. The device shows it large;
  the owner types it in the app or CLI (case, spaces and dashes ignored).
- `fingerprint`: the key's fingerprint, also shown to the owner before approving: the first 12 hex
  digits, upper case, of SHA-256 over the key's RFC 7638 JWK thumbprint input
  (`{"crv":"P-256","kty":"EC","x":"…","y":"…"}`, no spaces), grouped `XXXX-XXXX-XXXX`.
- `deviceCode` is a secret: only the gateway and the device know it.

`POST /device/v1/pair/poll` `{"deviceCode": "…"}` every `interval` seconds →

- 400 `authorization_pending`, `slow_down`, `expired_token`, `access_denied`;
- 200 `{"deviceId": "dev_<16 hex>", "name": "Watch"}` once approved (the name the owner gave). The
  device code is then used up.

### Sessions

`POST /device/v1/session/challenge` `{"deviceId": "dev_…"}` → 200
`{"nonce": "<base64url, 32 bytes>", "expiresIn": 60}`. One challenge per device at a time: a new one
replaces the last.

`POST /device/v1/session`

```json
{"deviceId": "dev_…", "nonce": "<as received>", "signature": "<base64url of the DER ECDSA signature>"}
```

The signature is ECDSA P-256 with SHA-256 (`SHA256withECDSA`, ASN.1 DER, as Android's `Signature`
produces it) over the UTF-8 bytes of

```
stormo-device-session
<deviceId>
<nonce>
```

(three lines joined by `\n`, no trailing newline). → 200 `{"token": "std_<base64url>", "expiresIn": 3600}`;
401 `device_unknown` or `bad_signature`; 400 `expired_nonce`.

Every other route takes `Authorization: Bearer std_…`. A token lives only in the gateway's memory: a
gateway restart ends every session (the device opens a new one, no pairing needed).

### Routes for a linked device

`GET /device/v1/health` → `{"status": "ok"}` (no token).

`GET /device/v1/me` →

```json
{"device": {"id": "dev_…", "name": "Watch", "model": "Pixel Watch 3", "grants": {"status": true, "review": true, "chat": ["*"]},
            "linked": 1762790000000},
 "tokenExpires": 1762793600000, "instance": "Acme"}
```

`GET /device/v1/fleet` (grant `status`) →

```json
{"agents": [{"id": "support", "name": "Ada", "unit": "ops", "state": "running", "busy": true, "live": "web_search",
             "waiting": 0, "unread": 2, "pendingReview": 3}],
 "polledAt": "2026-11-10T21:30:00Z"}
```

`state` is `running`, `starting`, `stopped`, `exited`, `restarting` or `unknown`; `live` the tool it
runs now or null; `waiting` approvals it waits on; `pendingReview` its open proposals.

`GET /device/v1/review` (grant `review`) →

```json
{"items": [{"agent": "support", "agentName": "Ada", "kind": "lesson", "id": "L-7f3a…", "summary": "Answer refund questions with…",
            "ai": {"approve": true, "reason": "…"}, "appOnly": false},
           {"agent": "support", "agentName": "Ada", "kind": "skill", "id": "refunds", "summary": "New skill refunds (3 files)",
            "ai": null, "appOnly": true, "why": "skills are decided in the app"}]}
```

`kind` is `lesson`, `setting`, `skill` or `schedules`; `id` is the lesson id, the setting key, the
skill name, or `schedules`. `summary` is at most 200 characters.

`POST /device/v1/review` `{"agent": "support", "kind": "lesson", "id": "L-7f3a…", "decision": "accept"}`
(`decision`: `accept` | `reject`) → 200 `{"ok": true}`; 400 `app_only`; 404 `not_found`.

`GET /device/v1/agents/<agent>/conversations` (grant `chat` covering the agent) →
`{"conversations": [{"id", "title", "preview", "lastActive", "messages"}]}`, the 20 most recently
active.

`GET /device/v1/agents/<agent>/conversations/<id>?limit=30` →
`{"id", "compacted", "messages": [{"id", "role": "user" | "assistant", "content", "at"}]}`: text rows
only (no tool rows), oldest first, the latest `limit` (at most 100), each `content` at most 4,000
characters.

`POST /device/v1/agents/<agent>/chat` `{"conversation": "<id, or omitted for a new one>", "message": "…"}`
(`message` 1–4,000 characters) → 202 `{"turn": "trn_<16 hex>", "conversation": "<id>"}`. One turn per
device at a time (409 `busy`).

`GET /device/v1/turns/<turn>` → `{"turn", "state": "running" | "done" | "failed", "conversation",
"reply" (done), "error" (failed)}`. Poll every 2–5 s. A finished turn is kept 15 minutes; a turn runs
at most 10 minutes. A device sees only its own turns.

## The core, toward the gateway (`/api/ingress/*`)

Only the ingress principal (`Authorization: Bearer sti_…`, minted per gateway boot and handed over as
`STORMO_INGRESS_KEY` with `STORMO_CORE_URL`) reaches these, and it reaches nothing else. Routes on
behalf of a device carry `X-Stormo-Device: dev_…` and `X-Stormo-Client-Ip`; the core refuses them
(401 `device_unknown`) unless the device is linked, and (403 `forbidden`) unless its grants cover
the route.

| route | body → answer |
|---|---|
| `POST /api/ingress/pairings` | `{name, model, os, appVersion, publicKey, attestation, ip, country}` → 201 `{pairing: "pr_…", userCode, fingerprint, expiresIn}`: the core makes the code (unique among pending ones) and checks the key is a point on P-256 |
| `GET /api/ingress/pairings/<id>` | → `{state: pending \| approved \| denied \| expired \| consumed, deviceId?, name?}`; reading `approved` consumes it |
| `GET /api/ingress/devices/<id>` | → `{id, publicKey}`, 401 `device_unknown` when not linked |
| `GET /api/ingress/me`, `/fleet`, `/review`; `POST /api/ingress/review` | as the device routes |
| `GET /api/ingress/agents/<a>/conversations[/<id>]`, `POST /api/ingress/agents/<a>/chat`, `GET /api/ingress/turns/<t>` | as the device routes |

## The owner (`/api/devices/*`, `stormo device …`)

| route | scope | body → answer |
|---|---|---|
| `GET /api/devices` | read | `{devices: [{id, name, model, os, appVersion, fingerprint, grants, created, lastSeen, lastIp, country, attestation (count of certificates recorded), revoked, revokedReason}], pairings: [{id, name, model, os, ip, country, fingerprint, created, expires}]}`: linked and revoked devices, and pairings still pending (never their codes) |
| `POST /api/devices/link` | admin | `{userCode, preview?: true}` → the pending pairing's claims (`pairing`), changing nothing; `{userCode, deny: true}` → `{denied}`; `{userCode, name?, grants}` → 201 `{device}`. 404 when no pending pairing has that code; 429 after 5 wrong codes in 10 minutes |
| `PATCH /api/devices/<id>` | admin | `{name?, grants?}` → `{device}` |
| `DELETE /api/devices/<id>` | admin | → `{revoked}`; its sessions stop at their next call |

Every link, denial, change and revocation is audited; so is each device's review decision and chat
turn (actor `device:<name>`, never the message text).

CLI: `stormo device list`, `stormo device link <code> [--name n] [--grant status,review] [--agents a,b|*]
[--yes]` (shows what the device claims and asks before linking), `stormo device deny <code>`,
`stormo device grants <device> [--grant …] [--agents …]`, `stormo device revoke <device>`; all with
`--json`. A device is named by its id or its name.
