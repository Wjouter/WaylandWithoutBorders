// internal/config/config.go
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type Config struct {
	Host      string `toml:"host"`
	Key       string `toml:"key"`
	Name      string `toml:"name"`
	Port      int    `toml:"port"`
	Edge      string `toml:"edge"`      // X11 single switch edge: left or right
	Clipboard *bool  `toml:"clipboard"` // nil = unset, treated as enabled

	// Edges selects which screen edges switch to the remote (Wayland only).
	// Unset = all four. ["left","right"] = only those. ["none"] = disable
	// edge switching entirely.
	Edges []string `toml:"edges"`

	// SwitchModifier gates edge switching on a held modifier (Wayland only),
	// like PowerToys' Easy Mouse. "" = switch freely; "shift"/"ctrl"/"alt" =
	// only cross while that key is held.
	SwitchModifier string `toml:"switch_modifier"`

	// Bidirectional enables this machine to also control the remote host.
	// Equivalent to the -bidi flag; the flag OR-s with this so either turns it on.
	Bidirectional bool `toml:"bidirectional"`

	// KeyboardLayout controls inbound Windows->Linux keyboard mapping. "auto"
	// detects the local Linux layout when possible; unsupported layouts fall back
	// to the US-compatible mapping.
	KeyboardLayout string `toml:"keyboard_layout"`

	// AccelMultiplier scales raw evdev deltas before they move the remote cursor
	// (outbound, Linux->Windows). The Windows side adds no acceleration of its
	// own, so this is the only outbound speed knob. <= 0 means unset.
	AccelMultiplier float64 `toml:"accel_multiplier"`

	// InboundMultiplier scales Windows->Linux cursor movement (the inbound,
	// absolute-mirror direction). 1.0 mirrors Windows 1:1; raise it for a faster
	// local cursor when Windows is in control. <= 0 means unset (defaults to 1).
	InboundMultiplier float64 `toml:"inbound_multiplier"`
}

func Load(path string) (*Config, error) {
	var cfg Config
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if cfg.Host == "" {
		return nil, fmt.Errorf("config: host is required")
	}
	if cfg.Key == "" {
		return nil, fmt.Errorf("config: key is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 15100
	}
	if cfg.Name == "" {
		hostname, _ := os.Hostname()
		if hostname == "" {
			hostname = "linux"
		}
		cfg.Name = hostname
	}
	if len(cfg.Name) > 15 {
		cfg.Name = cfg.Name[:15]
	}
	if cfg.AccelMultiplier <= 0 {
		cfg.AccelMultiplier = 2.0
	}
	if cfg.InboundMultiplier <= 0 {
		cfg.InboundMultiplier = 1.0
	}
	if cfg.KeyboardLayout == "" {
		cfg.KeyboardLayout = "auto"
	}
	if cfg.Edge == "" {
		cfg.Edge = "left"
	}
	return &cfg, nil
}

// Save writes the config back to path as TOML, creating parent dirs as needed.
// Used by the GUI to persist edited settings.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create config dir: %w", err)
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create config: %w", err)
	}
	defer f.Close()
	if err := toml.NewEncoder(f).Encode(cfg); err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	return nil
}

// EnabledEdges resolves which edges switch to the remote (Wayland). Unset
// defaults to all four; an explicit "none"/"off" disables switching entirely;
// otherwise the listed valid edges are used.
func (c *Config) EnabledEdges() []string {
	if len(c.Edges) == 0 {
		return []string{"left", "right", "top", "bottom"}
	}
	var out []string
	for _, e := range c.Edges {
		switch e {
		case "left", "right", "top", "bottom":
			out = append(out, e)
		case "none", "off":
			return nil
		}
	}
	return out
}

func (c *Config) MessagePort() int {
	return c.Port + 1
}

// ClipboardEnabled reports whether clipboard sharing should run. An absent
// clipboard key keeps it enabled, preserving the prior default behavior.
func (c *Config) ClipboardEnabled() bool {
	return c.Clipboard == nil || *c.Clipboard
}
