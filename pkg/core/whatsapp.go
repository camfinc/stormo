package core

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"

	"github.com/camfinc/stormo/pkg/local"
	"github.com/camfinc/stormo/pkg/manifest"
	"github.com/camfinc/stormo/pkg/whatsapp"
)

var whatsappPath = regexp.MustCompile(`^/api/agents/([a-z][a-z0-9-]*)/whatsapp$`)

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
			}
			return whatsapp.Policy{Enabled: true, Users: s.AllowedUsers, All: s.AllowAllUsers != nil && *s.AllowAllUsers}
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
