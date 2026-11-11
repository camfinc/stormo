The device gateway is not a capability agents call: no agent can be granted it. It is the one thing
an instance exposes to the internet so that linked devices (a watch, a phone) can sign in and reach
the fleet while the core stays on loopback. Everything about it is in docs/devices.md.

- It serves `/device/v1/*` only: pairing (the owner types the code the device shows), sessions opened
  with the device's hardware key, and the fleet, review and chat views the device's grants allow.
- It holds one credential: a key the core mints at each boot, which reaches the core's
  `/api/ingress/*` and nothing else. Its state (pending pairings, sessions) lives in memory.
- Settings (`env:`): `CLIENT_IP_HEADER` and `CLIENT_COUNTRY_HEADER` (the headers your edge sets, e.g.
  `Cf-Connecting-Ip` and `Cf-Ipcountry`; empty: the peer address) and `TOKEN_MINUTES` (session
  length, default 60). Which pairings stay pending is the core's to decide (20 at most, 3 per
  address, the oldest evicted).
- `stormo capability sleep <name>` takes every device offline until `stormo capability wake <name>`.
