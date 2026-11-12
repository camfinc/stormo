package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/camfinc/stormo/pkg/engine"
	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/whatsapp"
	"go.mau.fi/whatsmeow/types"
)

var whatsappPath = regexp.MustCompile(`^/api/agents/([a-z][a-z0-9-]*)/whatsapp(/groups)?$`)

type whatsappConnections struct {
	sync.Mutex
	clients map[string]*whatsapp.Connector
}

func (c *Core) whatsappPolicy(id string) whatsapp.Policy {
	a, err := manifest.Load(c.inst.Root, id, c.inst.Names.Secret)
	if err != nil {
		return whatsapp.Policy{}
	}
	for _, ch := range a.Channels {
		if ch.Kind == "whatsapp" {
			s := ch.SlackSettings
			if ch.Local != nil {
				if ch.Local.AllowedUsers != nil {
					s.AllowedUsers = ch.Local.AllowedUsers
				}
				if ch.Local.AllowAllUsers != nil {
					s.AllowAllUsers = ch.Local.AllowAllUsers
				}
				if ch.Local.AllowedGroups != nil {
					s.AllowedGroups = ch.Local.AllowedGroups
				}
			}
			return whatsapp.Policy{Enabled: true, Users: s.AllowedUsers, Groups: s.AllowedGroups, All: s.AllowAllUsers != nil && *s.AllowAllUsers}
		}
	}
	return whatsapp.Policy{}
}
func (c *Core) openWhatsApp(id string) (*whatsapp.Connector, error) {
	if c.whatsapp.clients == nil {
		c.whatsapp.clients = map[string]*whatsapp.Connector{}
	}
	if conn := c.whatsapp.clients[id]; conn != nil {
		return conn, nil
	}
	conn, err := whatsapp.Open(filepath.Join(CoreDir(c.inst.Root), "whatsapp", id, "session.db"), func() whatsapp.Policy { return c.whatsappPolicy(id) }, func(ctx context.Context, session, text string) (string, error) {
		if !c.whatsappPolicy(id).Enabled {
			return "", fmt.Errorf("channel disabled")
		}
		conversations, err := local.Conversations(c.inst, id)
		if err != nil {
			return "", err
		}
		reply, _, err := conversations.Send(ctx, session, text)
		return reply, err
	})
	if err == nil {
		// Through the runtime's Exchanger when it has one: a person's voice note goes to the agent
		// to transcribe, and its voice reply comes back as a voice note (Ogg Opus).
		conn.UseExchange(func(ctx context.Context, session string, t whatsapp.Turn) (whatsapp.Answer, error) {
			if !c.whatsappPolicy(id).Enabled {
				return whatsapp.Answer{}, fmt.Errorf("channel disabled")
			}
			conversations, err := local.Conversations(c.inst, id)
			if err != nil {
				return whatsapp.Answer{}, err
			}
			ex, ok := conversations.(engine.Exchanger)
			if !ok {
				reply, _, err := conversations.Send(ctx, session, t.Text)
				return whatsapp.Answer{Text: reply}, err
			}
			in := engine.Input{Text: t.Text, AudioFormat: "opus"}
			if a := t.Audio; a != nil {
				in.Files = []engine.File{{Name: "voice-note.ogg", MIME: a.MIME, Data: a.Data, Marked: a.Marked}}
			}
			r, err := ex.Exchange(ctx, session, in)
			if err != nil {
				return whatsapp.Answer{}, err
			}
			ans := whatsapp.Answer{Text: r.Text}
			if a := r.Audio; a != nil && strings.HasPrefix(a.MIME, "audio/ogg") {
				ans.Voice, ans.WithText = a.Data, a.WithText
			}
			return ans, nil
		})
		c.whatsapp.clients[id] = conn
	}
	return conn, err
}
func (c *Core) startWhatsApp() {
	c.whatsapp.Lock()
	defer c.whatsapp.Unlock()
	for _, id := range manifest.AgentIDs(c.inst.Root) {
		if !c.whatsappPolicy(id).Enabled {
			continue
		}
		if _, err := os.Stat(filepath.Join(CoreDir(c.inst.Root), "whatsapp", id, "session.db")); err != nil {
			continue
		}
		conn, err := c.openWhatsApp(id)
		if err == nil && conn.Paired() {
			_ = conn.Connect()
		}
	}
}
func (c *Core) closeWhatsApp() {
	c.whatsapp.Lock()
	defer c.whatsapp.Unlock()
	for _, conn := range c.whatsapp.clients {
		conn.Close()
	}
	c.whatsapp.clients = nil
}
func (c *Core) handleWhatsApp(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	// Pairing QR codes are credentials: restrict reads as well as mutations to operators.
	if !c.caller(r).Can(ScopeOperate) {
		writeJSONBody(w, 403, errBody("forbidden", "An operator credential is required"))
		return
	}
	if r.Method != "GET" && r.Method != "POST" && r.Method != "DELETE" {
		writeJSONBody(w, 405, errBody("method", "Use GET, POST or DELETE"))
		return
	}
	c.whatsapp.Lock()
	defer c.whatsapp.Unlock()
	conn := c.whatsapp.clients[id]
	if r.Method == "DELETE" {
		if conn == nil {
			if _, err := os.Stat(filepath.Join(CoreDir(c.inst.Root), "whatsapp", id, "session.db")); err == nil {
				var err error
				conn, err = c.openWhatsApp(id)
				if err != nil {
					writeJSONBody(w, 500, errBody("session_failed", "Could not open saved session to unlink"))
					return
				}
				if conn.Paired() {
					if err = conn.Connect(); err != nil {
						writeJSONBody(w, 502, errBody("connect_failed", "Reconnect before unlinking, or remove Stormo from Linked devices on the phone."))
						return
					}
				}
			}
		}
		if conn != nil {
			if err := conn.Logout(r.Context()); err != nil {
				writeJSONBody(w, 502, errBody("unlink_failed", "Could not unlink. Reconnect and try again, or remove Stormo from WhatsApp Linked devices."))
				return
			}
			conn.Close()
			delete(c.whatsapp.clients, id)
		}
		// Also remove an inactive session left by a previous core process.
		if err := os.RemoveAll(filepath.Join(CoreDir(c.inst.Root), "whatsapp", id)); err != nil {
			writeJSONBody(w, 500, errBody("remove_failed", "Could not remove the local session"))
			return
		}
		writeJSONBody(w, 200, whatsapp.Status{State: "unlinked"})
		return
	}
	if !c.whatsappPolicy(id).Enabled {
		writeJSONBody(w, 409, errBody("not_configured", "Add WhatsApp and save the agent before linking."))
		return
	}
	if r.Method == "POST" {
		var err error
		conn, err = c.openWhatsApp(id)
		if err != nil {
			writeJSONBody(w, 500, errBody("session_failed", "Could not open the WhatsApp session store"))
			return
		}
		if err = conn.Connect(); err != nil {
			writeJSONBody(w, 502, errBody("connect_failed", "Could not connect. Wait a moment and try again."))
			return
		}
	}
	if conn == nil {
		writeJSONBody(w, 200, whatsapp.Status{State: "unlinked"})
		return
	}
	writeJSONBody(w, 200, conn.Status())
}

// handleWhatsAppGroups lists the groups the agent's linked account is in, for the allowed groups
// picker. Operators only, like the pairing status: the names are the account's.
func (c *Core) handleWhatsAppGroups(w http.ResponseWriter, r *http.Request, id string) {
	w.Header().Set("Cache-Control", "no-store")
	if !c.caller(r).Can(ScopeOperate) {
		writeJSONBody(w, 403, errBody("forbidden", "An operator credential is required"))
		return
	}
	if r.Method != "GET" {
		writeJSONBody(w, 405, errBody("method", "Use GET"))
		return
	}
	if !c.whatsappPolicy(id).Enabled {
		writeJSONBody(w, 409, errBody("not_configured", "Add WhatsApp and save the agent before linking."))
		return
	}
	c.whatsapp.Lock()
	conn := c.whatsapp.clients[id]
	c.whatsapp.Unlock()
	if conn == nil {
		writeJSONBody(w, 409, errBody("not_connected", "Link the agent's WhatsApp account to list its groups."))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	groups, err := conn.Groups(ctx)
	if errors.Is(err, whatsapp.ErrNotConnected) {
		writeJSONBody(w, 409, errBody("not_connected", "Link the agent's WhatsApp account to list its groups."))
		return
	} else if err != nil {
		writeJSONBody(w, 502, errBody("groups_failed", "Could not list the account's groups. Try again."))
		return
	}
	writeJSONBody(w, 200, map[string]any{"groups": groups})
}

// WhatsAppTools are the whatsapp_* MCP tools: an agent posting, unprompted, on its own linked
// account to a chat its channel allows. core is the running core (the tools are built before it).
func WhatsAppTools(core func() *Core) []MCPTool {
	return []MCPTool{{
		Name: "whatsapp_send",
		Description: "Post a WhatsApp message, or an image, on your own linked account without being written to first: " +
			"a result that was not ready when you answered, an alert. Only for an agent with a WhatsApp channel, and only to a " +
			"chat the channel allows: a group of its allowed_groups (id …@g.us) or a person of its allowed_users (+country " +
			"code and number). A WhatsApp message you receive names its chat (`chat …`): pass that to write back there. " +
			"`image` is a PNG or JPEG in the shared space (/shared/group/… or your unit's), up to 16 MB, with `text` as its " +
			"caption. Answers to people who write to you are sent on their own: use this only to start a message.",
		InputSchema: schema([]string{"chat"}, map[string]any{
			"chat":  str("a group id (…@g.us) of your allowed_groups, or a phone number (+…) of your allowed_users"),
			"text":  str("the message, up to 16 KB; with an image, its caption (optional)"),
			"image": str("a PNG or JPEG shared-space path (/shared/…), up to 16 MB"),
		}),
		Run: func(call ToolCall) (any, error) {
			var req struct {
				Chat  string `json:"chat"`
				Text  string `json:"text"`
				Image string `json:"image"`
			}
			if err := decodeArgs(call, &req); err != nil {
				return nil, err
			}
			return core().whatsappSend(call.Agent, req.Chat, req.Text, req.Image)
		},
	}}
}

// maxWhatsAppImage is the largest image whatsapp_send posts (WhatsApp's own limit for a photo).
const maxWhatsAppImage = 16 << 20

func (c *Core) whatsappSend(agent, chat, text, image string) (any, error) {
	p := c.whatsappPolicy(agent)
	if !p.Enabled {
		return nil, toolErr("you have no WhatsApp channel")
	}
	if len(text) > 16<<10 || image == "" && strings.TrimSpace(text) == "" {
		return nil, toolErr("text must be 1 byte to 16 KB (or send an image)")
	}
	jid, err := whatsapp.AllowedChat(p, chat)
	if err != nil {
		return nil, toolErr("%v", err)
	}
	var data []byte
	if image != "" {
		if data, _, err = c.Workdir.ReadBytes(agent, image, maxWhatsAppImage); err != nil {
			return nil, err
		}
	}
	c.whatsapp.Lock()
	conn := c.whatsapp.clients[agent]
	c.whatsapp.Unlock()
	if conn == nil {
		return nil, toolErr("your WhatsApp account is not linked on this core")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var ids []types.MessageID
	if data != nil {
		ids, err = conn.SendImage(ctx, jid, data, text)
	} else {
		ids, err = conn.Send(ctx, jid, text)
	}
	if errors.Is(err, whatsapp.ErrNotConnected) {
		return nil, toolErr("your WhatsApp account is not connected right now; try again later")
	} else if err != nil {
		return nil, toolErr("sent %d part(s), then failed: %v", len(ids), err)
	}
	return map[string]any{"chat": jid.String(), "parts": len(ids)}, nil
}
