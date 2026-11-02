# WhatsApp connector

In the macOS app, edit an agent → Channels → Add Whatsapp. Add allowed phone numbers in international format (for example `+393331234567`) and save. Click **Link / reconnect**, then scan the displayed QR code using the dedicated bot account's WhatsApp → Settings → Linked devices → Link a device. Message that account from an allowed phone number.

The connector runs in the core using Whatsmeow; no Node service or API token is required. The agent must run locally on that core with its conversation API credential configured, as for app chat. Both Hermes and Needletail use their existing conversation adapters. Each agent has its own linked account/session. Do not link one account to multiple agents: each would receive the same messages.

The first implementation supports incoming direct text messages and text replies. It ignores groups, broadcasts, self messages, and media. It does not provide cron delivery, attachments, voice transcription, or remote agent routing. Access is denied by default; the optional “Anyone may message” switch grants access to all direct senders. Channel removal disables task dispatch immediately; use **Unlink** before removing the channel to revoke the device session as well.

Sessions are restored on core startup. Device keys and received-message IDs are stored in `.swarm/core/whatsapp/<agent>/session.db` with file mode 0600 inside a 0700 directory. Protect this directory like a password; do not publish or share it. Pairing codes are exposed only to authenticated operators and are not cached. Unlink removes the local session after successful remote logout; if WhatsApp is unreachable, remove the device from the phone's Linked devices screen and retry.

Messages are processed sequentially with a bounded queue and a ten-minute turn deadline. IDs are claimed durably before agent execution to avoid repeating tool calls after reconnect or restart. A crash or delivery failure can therefore leave a request without a reply: check its agent conversation before retrying. Queue overflow appears in connector status. Conversations have stable `whatsapp-…` IDs and use the engines' existing session APIs (their displayed source may be API).

Whatsmeow is an unofficial linked-device integration. WhatsApp changes can break connectivity or restrict accounts. Use a dedicated number. Live pairing and delivery require an account owner's scan and are not exercised by automated tests.
