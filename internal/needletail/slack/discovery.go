package slack

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/camfinc/stormo/internal/needletail/sched"
)

// ChannelMetadata deliberately excludes topics, purposes, messages and member lists.
// Discovery is not engagement and never adds a destination to the messaging policy.
type ChannelMetadata struct {
	ID       string `json:"id"`
	Name     string `json:"name,omitempty"`
	Private  bool   `json:"is_private"`
	Archived bool   `json:"is_archived"`
	Member   *bool  `json:"is_member,omitempty"`
	// IM and MPIM: a DM or a group DM. A group DM's name lists its members (mpdm-ana--bob-1), so
	// neither's name is ever returned.
	IM   bool `json:"is_im,omitempty"`
	MPIM bool `json:"is_mpim,omitempty"`
}
type channelPage struct {
	Channels []ChannelMetadata `json:"channels"`
	Metadata struct {
		Next string `json:"next_cursor"`
	} `json:"response_metadata"`
}

func (w *Web) Channels(ctx context.Context, kind, cursor string) (channelPage, error) {
	var page channelPage
	err := w.call(ctx, "conversations.list", w.BotToken, url.Values{"types": {kind}, "cursor": {cursor}, "limit": {"100"}, "exclude_archived": {"true"}}, &page)
	return page, err
}
func (w *Web) ChannelInfo(ctx context.Context, id string) (ChannelMetadata, error) {
	var out struct {
		Channel ChannelMetadata `json:"channel"`
	}
	err := w.call(ctx, "conversations.info", w.BotToken, url.Values{"channel": {id}, "include_num_members": {"false"}}, &out)
	if err == nil && out.Channel.ID != id {
		err = errors.New("Slack returned mismatched channel metadata")
	}
	return out.Channel, err
}

type discoveredChannel struct {
	ChannelMetadata
	Target        string `json:"target"`
	PolicyAllowed bool   `json:"policy_allowed"`
}

func (g *Gateway) discovered(c ChannelMetadata) discoveredChannel {
	if c.IM || c.MPIM || strings.HasPrefix(c.Name, "mpdm-") {
		c.Name = "" // a DM's or group DM's name names people
	}
	return discoveredChannel{ChannelMetadata: c, Target: "slack:" + c.ID, PolicyAllowed: g.CheckTarget(sched.Target{Platform: "slack", Channel: c.ID}) == nil}
}

const discoveryNote = "Visibility and membership do not guarantee posting permission. Discovery does not authorize sending or join channels; only a successful send receipt confirms delivery."

func discoveryError(err error) error {
	var api *APIError
	if errors.As(err, &api) {
		switch api.Code {
		case "missing_scope":
			return fmt.Errorf("Slack channel discovery needs channels:read for public channels, groups:read for private channels, or im:read for DMs; ask the owner to update and reinstall the app: %w", err)
		case "channel_not_found":
			return fmt.Errorf("Slack cannot access this channel; it may be unavailable or hidden from this bot. This does not prove it does not exist: %w", err)
		}
	}
	return err
}

// discover performs one bounded provider page, with an explicit continuation cursor.
// A search filters that page only; even an empty match page may have more results.
func (g *Gateway) discover(ctx context.Context, query, cursor, kind string) (any, error) {
	if kind == "" {
		kind = "public_channel"
	}
	if kind != "public_channel" && kind != "private_channel" {
		return nil, errors.New("channel_type must be public_channel or private_channel")
	}
	if len(query) > 100 || len(cursor) > 2048 {
		return nil, errors.New("query or cursor is too long")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	page, err := g.o.Web.Channels(ctx, kind, cursor)
	if err != nil {
		return nil, discoveryError(err)
	}
	if len(page.Channels) > 100 || len(page.Metadata.Next) > 2048 {
		return nil, errors.New("Slack returned an oversized channel page")
	}
	q := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(query), "#"))
	found := []discoveredChannel{}
	for _, c := range page.Channels {
		if !channelIDRe.MatchString(c.ID) || !strings.HasPrefix(c.ID, "C") && !strings.HasPrefix(c.ID, "G") {
			return nil, errors.New("Slack returned an invalid channel ID")
		}
		if c.IM || c.MPIM || strings.HasPrefix(c.Name, "mpdm-") || c.Archived || !strings.Contains(strings.ToLower(c.Name), q) {
			continue
		}
		found = append(found, g.discovered(c))
	}
	return map[string]any{"channels": found, "next_cursor": page.Metadata.Next, "has_more": page.Metadata.Next != "", "query": query, "channel_type": kind, "note": discoveryNote + " The query filters this page only; continue with next_cursor even when this page has no matches."}, nil
}
func (g *Gateway) inspectChannel(ctx context.Context, target string) (any, error) {
	id := strings.TrimSpace(target)
	if id == "slack" {
		id = g.o.HomeChannel
	} else {
		id = strings.TrimPrefix(id, "slack:")
	}
	if !channelIDRe.MatchString(id) {
		return nil, errors.New("channel_info requires a channel ID or slack:<channel ID>; use channels with query to find its name first")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	c, err := g.o.Web.ChannelInfo(ctx, id)
	if err != nil {
		return nil, discoveryError(err)
	}
	return map[string]any{"channel": g.discovered(c), "accessible": true, "delivery_verified": false, "note": discoveryNote}, nil
}
