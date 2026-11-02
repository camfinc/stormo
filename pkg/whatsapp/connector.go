// Package whatsapp provides a core-hosted linked-device connector. Credentials never leave the core.
package whatsapp

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	_ "modernc.org/sqlite"
)

type Status struct {
	State string `json:"state"`
	QR    string `json:"qr,omitempty"`
	Error string `json:"error,omitempty"`
}
type Policy struct {
	Users   []string
	All     bool
	Enabled bool
}
type Reply func(context.Context, string, string) (string, error)
type Connector struct {
	send   func(context.Context, types.JID, string) error
	ctx    context.Context
	mu     sync.Mutex
	client *whatsmeow.Client
	store  *sqlstore.Container
	db     *sql.DB
	status Status
	cancel context.CancelFunc
	policy func() Policy
	reply  Reply
	queue  chan *events.Message
	done   chan struct{}
}

func Open(path string, policy func() Policy, reply Reply) (*Connector, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	f.Close()
	if err = os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := sqlstore.NewWithDB(db, "sqlite3", nil)
	ctx, cancel := context.WithCancel(context.Background())
	fail := func(err error) (*Connector, error) { cancel(); db.Close(); return nil, err }
	if err = store.Upgrade(ctx); err != nil {
		return fail(err)
	}
	if _, err = db.Exec(`CREATE TABLE IF NOT EXISTS stormo_received (id TEXT PRIMARY KEY, at INTEGER NOT NULL)`); err != nil {
		return fail(err)
	}
	device, err := store.GetFirstDevice(ctx)
	if err != nil {
		return fail(err)
	}
	c := &Connector{ctx: ctx, client: whatsmeow.NewClient(device, nil), store: store, db: db, cancel: cancel, policy: policy, reply: reply, queue: make(chan *events.Message, 32), done: make(chan struct{}), status: Status{State: "disconnected"}}
	c.send = func(ctx context.Context, chat types.JID, text string) error {
		_, err := c.client.SendMessage(ctx, chat, &waE2E.Message{Conversation: proto.String(text)})
		return err
	}
	c.client.AddEventHandler(func(event any) {
		switch e := event.(type) {
		case *events.Connected:
			c.set(Status{State: "connected"})
		case *events.Disconnected:
			c.set(Status{State: "disconnected"})
		case *events.LoggedOut:
			c.set(Status{State: "unlinked"})
		case *events.Message:
			if _, ok := Accepted(e, policy()); ok {
				select {
				case c.queue <- e:
				default:
					c.set(Status{State: "busy", Error: "Message queue full. Please retry later."})
				}
			}
		}
	})
	go func() {
		defer close(c.done)
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-c.queue:
				c.handle(ctx, e)
			}
		}
	}()
	return c, nil
}
func (c *Connector) set(s Status)   { c.mu.Lock(); c.status = s; c.mu.Unlock() }
func (c *Connector) Status() Status { c.mu.Lock(); defer c.mu.Unlock(); return c.status }
func (c *Connector) Paired() bool   { return c.client.Store.ID != nil }
func (c *Connector) Connect() error {
	if c.client.IsConnected() {
		if c.Status().State == "connected" || c.Status().State == "pairing" {
			return nil
		}
		c.client.Disconnect()
	}
	c.set(Status{State: "connecting"})
	if !c.Paired() {
		qr, err := c.client.GetQRChannel(c.ctx)
		if err != nil {
			return err
		}
		go func() {
			for e := range qr {
				if e.Event == "code" {
					c.set(Status{State: "pairing", QR: e.Code})
				} else if e.Event != "success" {
					c.set(Status{State: e.Event})
				}
			}
		}()
	}
	if err := c.client.Connect(); err != nil {
		c.set(Status{State: "disconnected", Error: "Could not connect to WhatsApp. Try again."})
		return err
	}
	return nil
}
func (c *Connector) Close() { c.cancel(); c.client.Disconnect(); <-c.done; c.store.Close() }
func (c *Connector) Logout(ctx context.Context) error {
	if c.Paired() {
		if err := c.client.Logout(ctx); err != nil {
			return err
		}
	}
	c.set(Status{State: "unlinked"})
	return nil
}

// Accepted is deliberately limited to fresh direct text messages from permitted phone numbers.
func Accepted(e *events.Message, p Policy) (string, bool) {
	if !p.Enabled || e.Info.IsFromMe || e.Info.IsGroup || e.Info.Chat.IsBroadcastList() || e.Info.Chat.Server == types.NewsletterServer {
		return "", false
	}
	sender := e.Info.Sender
	if sender.Server != types.DefaultUserServer {
		sender = e.Info.SenderAlt
	}
	if sender.Server != types.DefaultUserServer {
		return "", false
	}
	allowed := p.All
	for _, u := range p.Users {
		if strings.TrimPrefix(u, "+") == sender.User {
			allowed = true
		}
	}
	text := e.Message.GetConversation()
	if text == "" {
		text = e.Message.GetExtendedTextMessage().GetText()
	}
	return text, allowed && strings.TrimSpace(text) != "" && len(text) <= 32000
}
func (c *Connector) handle(ctx context.Context, e *events.Message) {
	text, ok := Accepted(e, c.policy())
	if !ok {
		return
	}
	// Claim before invoking tools: reconnect delivery must not execute a task twice.
	key := e.Info.Chat.String() + ":" + e.Info.ID
	result, err := c.db.ExecContext(ctx, `INSERT OR IGNORE INTO stormo_received(id,at) VALUES (?,?)`, key, time.Now().Unix())
	if err != nil {
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return
	}
	hash := sha256.Sum256([]byte(e.Info.Chat.ToNonAD().String()))
	turn, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	reply, err := c.reply(turn, fmt.Sprintf("whatsapp-%x", hash[:16]), text)
	if err != nil {
		reply = "The agent could not complete this request. Please check its status in Stormo before retrying."
	}
	if !c.policy().Enabled {
		return
	}
	for len(reply) > 0 {
		r := []rune(reply)
		count := len(r)
		if count > 3500 {
			count = 3500
		}
		part := string(r[:count])
		reply = string(r[count:])
		if err = c.send(turn, e.Info.Chat, part); err != nil {
			c.set(Status{State: "delivery_error", Error: "A reply could not be delivered. Check the agent conversation before retrying."})
			return
		}
	}
}
