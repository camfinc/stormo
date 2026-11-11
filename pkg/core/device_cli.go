package core

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/out"
)

// DeviceUsage is the device section of the CLI usage text.
const DeviceUsage = `  device (linked devices: a watch or a phone signs in through the device gateway, docs/devices.md):
  device list                            linked devices (grants, last seen, fingerprint) and pairings waiting for a code
  device link <code> [--name n] [--grant status,review,chat] [--agents a,b|*] [--yes]
                                         link the device showing <code>: shows what it claims and its key's fingerprint
                                         and asks first (--yes skips; --json needs it). Default grants: status,review
  device deny <code>                     refuse the device showing <code>
  device grants <device> [--grant …] [--agents …]   change what a linked device may do
  device revoke <device>                 unlink a device at once (by id or name)`

// DeviceOptions are `stormo device`'s flags.
type DeviceOptions struct {
	Name, Grant, Agents string
	Yes                 bool
	// In reads the confirmation (stdin); Ask is whether a person can answer it (a terminal).
	In  io.Reader
	Ask bool
}

// DeviceCommand runs `stormo device <sub>`.
func DeviceCommand(inst *instance.Instance, sub string, args []string, o *out.Writer, opt DeviceOptions) error {
	activePort = inst.CorePort()
	if o == nil {
		o = &out.Writer{W: os.Stdout}
	}
	if opt.In == nil {
		opt.In = os.Stdin
	}
	post := func(method, path string, body any, v any) error {
		b, _ := json.Marshal(body)
		return ownerJSON(inst.Root, method, path, bytes.NewReader(b), v)
	}
	switch sub {
	case "", "list":
		var v struct {
			Devices  []Device  `json:"devices"`
			Pairings []Pairing `json:"pairings"`
		}
		if err := ownerJSON(inst.Root, http.MethodGet, "/api/devices", nil, &v); err != nil {
			return err
		}
		o.Result(v, func(w io.Writer) {
			if len(v.Devices) == 0 && len(v.Pairings) == 0 {
				fmt.Fprintln(w, "no linked devices (a device pairs through the device gateway: docs/devices.md)")
				return
			}
			tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tMODEL\tGRANTS\tLAST SEEN\tFINGERPRINT\tSTATE")
			for _, d := range v.Devices {
				state := "linked"
				if d.Revoked != nil {
					state = "revoked (" + d.RevokedReason + ")"
				}
				seen := "never"
				if d.LastSeen != nil {
					seen = time.UnixMilli(*d.LastSeen).Format("2006-01-02 15:04")
					if d.LastIP != "" {
						seen += " " + d.LastIP
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", d.ID, d.Name, d.Model, grantsText(d.Grants), seen, d.Fingerprint, state)
			}
			tw.Flush()
			for _, p := range v.Pairings {
				fmt.Fprintf(w, "waiting: %s %s from %s %s, fingerprint %s, until %s (link it with the code it shows)\n", p.Name, p.Model, p.IP, p.Country,
					p.Fingerprint, time.UnixMilli(p.Expires).Format("15:04"))
			}
		})
		return nil
	case "link", "deny":
		if len(args) == 0 {
			return fmt.Errorf("usage: stormo device %s <code>", sub)
		}
		code := args[0]
		var pv struct {
			Pairing Pairing `json:"pairing"`
		}
		if err := post(http.MethodPost, "/api/devices/link", map[string]any{"userCode": code, "preview": true}, &pv); err != nil {
			return err
		}
		p := pv.Pairing
		o.Step("device: %s, %s %s, app %s", p.Name, p.Model, p.OS, p.AppVersion)
		o.Step("from: %s %s", p.IP, p.Country)
		o.Step("key fingerprint: %s (check the device shows the same)", p.Fingerprint)
		if sub == "deny" {
			var v map[string]any
			if err := post(http.MethodPost, "/api/devices/link", map[string]any{"userCode": code, "deny": true}, &v); err != nil {
				return err
			}
			o.Result(v, func(w io.Writer) { fmt.Fprintf(w, "refused %s\n", p.Name) })
			return nil
		}
		grants, err := parseGrantFlags(opt.Grant, opt.Agents, DeviceGrants{Status: true, Review: true})
		if err != nil {
			return err
		}
		if !opt.Yes {
			if o.JSON || !opt.Ask {
				return errors.New("linking asks first: check the fingerprint, then re-run with --yes")
			}
			fmt.Fprintf(os.Stderr, "Link this device with grants %s? [y/N] ", grantsText(grants))
			line, _ := bufio.NewReader(opt.In).ReadString('\n')
			if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
				return errors.New("not linked")
			}
		}
		var v struct {
			Device Device `json:"device"`
		}
		if err := post(http.MethodPost, "/api/devices/link", map[string]any{"userCode": code, "name": opt.Name, "grants": grants}, &v); err != nil {
			return err
		}
		o.Result(v, func(w io.Writer) {
			fmt.Fprintf(w, "linked %s (%s): %s\n", v.Device.Name, v.Device.ID, grantsText(v.Device.Grants))
		})
		return nil
	case "grants", "revoke":
		if len(args) == 0 {
			return fmt.Errorf("usage: stormo device %s <device>", sub)
		}
		d, err := findDevice(inst, args[0])
		if err != nil {
			return err
		}
		if sub == "revoke" {
			var v map[string]any
			if err := post(http.MethodDelete, "/api/devices/"+d.ID, nil, &v); err != nil {
				return err
			}
			o.Result(v, func(w io.Writer) { fmt.Fprintf(w, "revoked %s (%s)\n", d.Name, d.ID) })
			return nil
		}
		if opt.Grant == "" && opt.Agents == "" && opt.Name == "" {
			return errors.New("usage: stormo device grants <device> [--grant status,review,chat] [--agents a,b|*] [--name n]")
		}
		body := map[string]any{}
		if opt.Grant != "" || opt.Agents != "" {
			g, err := parseGrantFlags(opt.Grant, opt.Agents, d.Grants)
			if err != nil {
				return err
			}
			body["grants"] = g
		}
		if opt.Name != "" {
			body["name"] = opt.Name
		}
		var v struct {
			Device Device `json:"device"`
		}
		if err := post(http.MethodPatch, "/api/devices/"+d.ID, body, &v); err != nil {
			return err
		}
		o.Result(v, func(w io.Writer) {
			fmt.Fprintf(w, "%s (%s): %s\n", v.Device.Name, v.Device.ID, grantsText(v.Device.Grants))
		})
		return nil
	}
	return fmt.Errorf("unknown device command %q\n%s", sub, DeviceUsage)
}

// parseGrantFlags reads --grant (status, review, chat) and --agents over a starting point: --grant
// replaces the set, --agents names who chat reaches (and implies chat).
func parseGrantFlags(grant, agents string, from DeviceGrants) (DeviceGrants, error) {
	g := from
	chat := len(from.Chat) > 0
	if grant != "" {
		g, chat = DeviceGrants{Chat: []string{}}, false
		for _, p := range strings.Split(grant, ",") {
			switch strings.TrimSpace(p) {
			case "status":
				g.Status = true
			case "review":
				g.Review = true
			case "chat":
				chat = true
			case "", "none":
			default:
				return g, fmt.Errorf("--grant takes status, review, chat (or none), not %q", p)
			}
		}
		if chat {
			g.Chat = from.Chat
		}
	}
	if agents != "" {
		g.Chat = []string{}
		for _, a := range strings.Split(agents, ",") {
			if a = strings.TrimSpace(a); a != "" {
				g.Chat = append(g.Chat, a)
			}
		}
		chat = true
	}
	if !chat {
		g.Chat = []string{}
	} else if len(g.Chat) == 0 {
		return g, errors.New("chat needs --agents (names, or * for every agent)")
	}
	return g, nil
}

// findDevice names a linked device by its id or its (unique) name.
func findDevice(inst *instance.Instance, ref string) (Device, error) {
	var v struct {
		Devices []Device `json:"devices"`
	}
	if err := ownerJSON(inst.Root, http.MethodGet, "/api/devices", nil, &v); err != nil {
		return Device{}, err
	}
	var found []Device
	for _, d := range v.Devices {
		if d.Revoked == nil && (d.ID == ref || strings.EqualFold(d.Name, ref)) {
			found = append(found, d)
		}
	}
	switch len(found) {
	case 0:
		return Device{}, fmt.Errorf("no linked device %q (stormo device list)", ref)
	case 1:
		return found[0], nil
	}
	return Device{}, fmt.Errorf("%d linked devices are named %q: use the id (stormo device list)", len(found), ref)
}
