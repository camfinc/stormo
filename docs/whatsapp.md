# WhatsApp connector

In the macOS app, edit an agent → Channels → Add Whatsapp. Add allowed phone numbers in international format (for example `+393331234567`) and save. Click **Link / reconnect**, then scan the displayed QR code using the dedicated bot account's WhatsApp → Settings → Linked devices → Link a device. Message that account from an allowed phone number. To use it in a group, add the bot account to the group, then pick the group under **Allowed groups** (the picker lists the linked account's groups) and save.

The connector runs in the core using Whatsmeow; no Node service or API token is required. The agent must run locally on that core with its conversation API credential configured, as for app chat. Both Hermes and Needletail use their existing conversation adapters. Each agent has its own linked account/session. Do not link one account to multiple agents: each would receive the same messages.

**What it answers.** A direct text message from an allowed phone number (or from anyone, with the “Anyone may message” switch). In a group listed under **Allowed groups** (`allowed_groups:`, group ids `…@g.us`; the app lists the account's groups by name once it is linked), a text that @mentions the agent or replies to one of its messages; anything else in the group is not read. Any member of an allowed group can address the agent there; `allowed_users` still decides whom it takes instructions from: the turn's input names the sender and the group, and marks a sender who is not on the list (their questions may be answered, their instructions are not to be acted on). A group is one conversation for all its members, and the reply quotes the message it answers. A **reaction** to one of the agent's messages, from someone it would answer there, is a turn of its own, saying who reacted with what to which message; the agent may stay silent. It ignores broadcasts, status updates, Channels, its own messages, reactions to other people's messages, and media (images, voice notes, documents, stickers, locations; a captioned photo too).

**Its reactions.** The agent reacts 👀 to a message when it starts working on it, removes the reaction once its reply is posted, and leaves ❌ when the turn or the delivery failed. A reply that is only `[SILENT]` (or `SILENT`, `NO_REPLY`, `NO REPLY`; or that token as its own first or last line, which is dropped) posts nothing, as the scheduler's rule.

```yaml
channels:
  - kind: whatsapp
    allowed_users: ["+393331234567"]
    allowed_groups: ["120363012345678901@g.us"]   # local: overrides it, as allowed_users
```

It does not provide cron delivery, attachments, voice transcription, typing indicators, read receipts, or remote agent routing. Access is denied by default. Channel removal disables task dispatch immediately; use **Unlink** before removing the channel to revoke the device session as well.

Sessions are restored on core startup. Device keys, received-message IDs and the agent's sent messages (their ids and first 200 characters, kept 90 days, to know a reaction or a reply is to the agent) are stored in `.swarm/core/whatsapp/<agent>/session.db` with file mode 0600 inside a 0700 directory. Protect this directory like a password; do not publish or share it. Pairing codes are exposed only to authenticated operators and are not cached. Unlink removes the local session after successful remote logout; if WhatsApp is unreachable, remove the device from the phone's Linked devices screen and retry.

Messages are processed sequentially with a bounded queue and a ten-minute turn deadline. IDs are claimed durably before agent execution to avoid repeating tool calls after reconnect or restart. A crash or delivery failure can therefore leave a request without a reply: check its agent conversation before retrying. Queue overflow appears in connector status. Conversations have stable `whatsapp-…` IDs and use the engines' existing session APIs (their displayed source may be API).

Whatsmeow is an unofficial linked-device integration. WhatsApp changes can break connectivity or restrict accounts. Use a dedicated number. Live pairing and delivery require an account owner's scan and are not exercised by automated tests.
