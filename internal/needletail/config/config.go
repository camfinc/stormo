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
	"strconv"
	"time"

	"github.com/camfinc/stormo/internal/needletail/wire"
	"go.yaml.in/yaml/v3"
)

// File is the config's name inside the home.
const File = "needletail.yaml"

// Defaults for the limits a manifest leaves at 0.
const (
	DefaultTurns          = 90
	DefaultCommandTimeout = 180 // seconds
	DefaultScriptTimeout  = 30  // seconds
	DefaultPort           = 8642
	DefaultHome           = "/opt/data"
	DefaultSharedDir      = "/shared"
	// Hot-memory budgets when the config has none (UTF-16 units).
	DefaultMemoryLimit = 2200
	DefaultUserLimit   = 1375
)

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
	if c.Memory.MemoryCharLimit <= 0 {
		c.Memory.MemoryCharLimit = DefaultMemoryLimit
	}
	if c.Memory.UserCharLimit <= 0 {
		c.Memory.UserCharLimit = DefaultUserLimit
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

// Endpoints are the model endpoints in the order they are tried: primary, then fallbacks.
func (c *Config) Endpoints() []wire.Endpoint {
	return append([]wire.Endpoint{c.Model.Primary}, c.Model.Fallbacks...)
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

// SessionsDir holds the session files.
func (c *Config) SessionsDir() string { return filepath.Join(c.Home, "sessions") }
