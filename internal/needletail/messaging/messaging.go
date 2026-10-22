// Package messaging is the runtime's one policy for messages the agent sends on its own
// (docs/needletail.md §4.9): the send_message tool, the Luau send_message binding and the
// delivery of the jobs the agent made all go through it. A platform's gateway (Slack) applies it.
package messaging

// Tool names: send_message (Hermes') posts a message; no_reply lets the agent stay out of a
// conversation with other people. Neither is ever offered to a scheduled run (its reply is what
// is delivered).
const (
	SendTool    = "send_message"
	NoReplyTool = "no_reply"
)
