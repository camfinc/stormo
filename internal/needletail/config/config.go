// Package config loads the runtime's settings: $AGENT_HOME/needletail.yaml (wire.Config, rendered
// by the adapter) and the container env the adapter sets (docs/needletail.md §4.1).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"time"

	"github.com/camfinc/stormo/internal/needletail/fsx"
	"github.com/camfinc/stormo/internal/needletail/wire"
	"github.com/camfinc/stormo/pkg/instance"
	"github.com/camfinc/stormo/pkg/learning"
	"go.yaml.in/yaml/v3"
)

// File is the config's name inside the home.
const File = "needletail.yaml"

// Defaults for the limits a manifest leaves at 0.
const (
	DefaultTurns          = 90
	DefaultCommandTimeout = 180 // seconds
	DefaultScriptTimeout  = 30  // seconds
	DefaultLuauTimeout    = 30  // seconds: a Luau tool the model calls
	DefaultPort           = 8642
	DefaultHome           = "/opt/data"
	DefaultSharedDir      = "/shared"
	// Hot-memory budgets when the config has none (UTF-16 units).
	DefaultMemoryLimit = 2200
	DefaultUserLimit   = 1375
)

// Scrubber is the learning scrubber the dream runs with this instance's rules (needletail.yaml
// scrub:, the engine's own rules alone when absent): what it flags must not be exported for the dream.
func (c *Config) Scrubber() *learning.Scrubber {
	inst := &instance.Instance{}
	if c.Scrub != nil {
		inst.ScrubTokenPrefixes = c.Scrub.TokenPrefixes
		for _, r := range c.Scrub.Rules {
			inst.ScrubRules = append(inst.ScrubRules, instance.ScrubRule{Kind: r.Kind, Pattern: r.Pattern, Require: r.Require})
		}
	}
	return learning.NewScrubber(inst)
}

// Config is needletail.yaml plus what the environment says about this container.
type Config struct {
	wire.Config
	// Home is the agent's home ($AGENT_HOME).
	Home string
	// SharedDir is the shared-space root ($SWARM_SHARED_DIR); Unit the agent's unit ($SWARM_UNIT).
	SharedDir string
	Unit      string
	// Port is the API's port ($NEEDLETAIL_API_PORT).
	Port int
	// Location is the agent's wall clock: Timezone, else $TZ (a zone Go cannot load is UTC, as
	// for time.Local), else UTC.
	Location *time.Location
}

// Load reads home's needletail.yaml and the env (getenv is os.Getenv outside tests). Unknown keys
// are refused: the file is a versioned contract.
func Load(home string, getenv func(string) string) (*Config, error) {
	if home == "" {
		home = getenv("AGENT_HOME")
	}
	if home == "" {
		home = DefaultHome
	}
	body, err := os.ReadFile(filepath.Join(home, File))
	if err != nil {
		return nil, err
	}
	c := &Config{Home: home}
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&c.Config); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%s: %w", File, err)
	}
	if c.Version != wire.ConfigVersion {
		return nil, fmt.Errorf("%s: version %d (this runtime reads %d)", File, c.Version, wire.ConfigVersion)
	}
	for _, ep := range append([]wire.Endpoint{c.Model.Primary}, c.Model.Fallbacks...) {
		if ep.KeyEnv == "NEEDLETAIL_LEARNING_KEY" {
			return nil, fmt.Errorf("owner learning key is reserved")
		}
	}
	if c.Core != nil && c.Core.KeyEnv == "NEEDLETAIL_LEARNING_KEY" {
		return nil, fmt.Errorf("owner learning key is reserved")
	}
	for _, secret := range c.Luau.HTTPSecrets {
		if secret.Env == "NEEDLETAIL_LEARNING_KEY" {
			return nil, fmt.Errorf("owner learning key is reserved")
		}
	}
	if c.Scrub != nil {
		for i, r := range c.Scrub.Rules {
			for _, p := range append([]string{r.Pattern}, r.Require...) {
				if _, err := regexp.Compile(p); err != nil || r.Pattern == "" || r.Kind == "" {
					return nil, fmt.Errorf("%s: scrub.rules[%d]: invalid rule", File, i)
				}
			}
		}
	}
	if f := c.Files; f != nil && (f.RetentionDays < 0 || f.RetentionDays > 365) {
		return nil, fmt.Errorf("%s: files.retention_days %d must be 1 to 365", File, f.RetentionDays)
	}
	if s := c.STT; s != nil && s.Provider != "" && s.Provider != "elevenlabs" {
		return nil, fmt.Errorf("%s: stt.provider %q is not one this runtime has (elevenlabs)", File, s.Provider)
	}
	if c.Model.Primary.BaseURL == "" || c.Model.Primary.Model == "" {
		return nil, fmt.Errorf("%s: model.primary needs base_url and model", File)
	}
	if c.Limits.Turns <= 0 {
		c.Limits.Turns = DefaultTurns
	}
	if c.Limits.CommandTimeout <= 0 {
		c.Limits.CommandTimeout = DefaultCommandTimeout
	}
	if c.Limits.ScriptTimeout <= 0 {
		c.Limits.ScriptTimeout = DefaultScriptTimeout
	}
	if c.Limits.LuauTimeout <= 0 {
		c.Limits.LuauTimeout = DefaultLuauTimeout
	}
	if c.Memory.MemoryCharLimit <= 0 {
		c.Memory.MemoryCharLimit = DefaultMemoryLimit
	}
	if c.Memory.UserCharLimit <= 0 {
		c.Memory.UserCharLimit = DefaultUserLimit
	}
	c.Location = time.UTC
	if c.Timezone != "" {
		loc, err := time.LoadLocation(c.Timezone)
		if err == nil && c.Timezone == "Local" {
			err = errors.New("not an IANA zone")
		}
		if err != nil {
			return nil, fmt.Errorf("%s: timezone %q: %w", File, c.Timezone, err)
		}
		c.Location = loc
	} else if tz := getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			c.Location = loc
		}
	}
	c.SharedDir = getenv("SWARM_SHARED_DIR")
	if c.SharedDir == "" {
		c.SharedDir = DefaultSharedDir
	}
	c.Unit = getenv("SWARM_UNIT")
	c.Port = DefaultPort
	if p := getenv("NEEDLETAIL_API_PORT"); p != "" {
		n, err := strconv.Atoi(p)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("NEEDLETAIL_API_PORT %q is not a port", p)
		}
		c.Port = n
	}
	return c, nil
}

// DefaultFileRetention is how long uploads are kept when files.retention_days is unset.
const DefaultFileRetention = 14 * 24 * time.Hour

// FileRetention is how long people's uploads are kept (files.retention_days).
func (c *Config) FileRetention() time.Duration {
	if c.Files != nil && c.Files.RetentionDays > 0 {
		return time.Duration(c.Files.RetentionDays) * 24 * time.Hour
	}
	return DefaultFileRetention
}

// VisionMode is files.vision: auto, on or off (llm.Client.Vision).
func (c *Config) VisionMode() string {
	if c.Files != nil && c.Files.Vision != "" {
		return string(c.Files.Vision)
	}
	return "auto"
}

// STTEnabled is whether voice notes are transcribed (stt.enabled; the key must be there too).
func (c *Config) STTEnabled() bool { return c.STT != nil && c.STT.Enabled }

// Endpoints are the model endpoints in the order they are tried: primary, then fallbacks.
func (c *Config) Endpoints() []wire.Endpoint {
	return append([]wire.Endpoint{c.Model.Primary}, c.Model.Fallbacks...)
}

// BackgroundLifetime is how long a background process may run (0: the runtime's default).
func (c *Config) BackgroundLifetime() time.Duration {
	return time.Duration(c.Limits.BackgroundLifetime) * time.Second
}

// CommandTimeout is one terminal command's default bound.
func (c *Config) CommandTimeout() time.Duration {
	return time.Duration(c.Limits.CommandTimeout) * time.Second
}

// SharedRoots are the shared-space directories this agent may reach: group and its own unit.
func (c *Config) SharedRoots() []string {
	roots := []string{filepath.Join(c.SharedDir, "group")}
	if c.Unit != "" {
		roots = append(roots, filepath.Join(c.SharedDir, c.Unit))
	}
	return roots
}

// StateDir is the runtime's own state (idempotency keys, later monitor outputs and cursors).
func (c *Config) StateDir() string { return filepath.Join(c.Home, "state", "needletail") }

// TmpDir is the home's unclaimed temp dir (fsx.TmpDir).
func (c *Config) TmpDir() string { return filepath.Join(c.Home, fsx.TmpDir) }

// SessionsDir holds the session files.
func (c *Config) SessionsDir() string { return filepath.Join(c.Home, "sessions") }
