package main

import (
	"encoding/json"
	"strings"
	"sync/atomic"

	"gopkg.in/yaml.v3"
)

const pluginVersion = "0.1.4"

const (
	strategyFallback   = "fallback"
	strategyRoundRobin = "round-robin"
)

// comboConfig is one named model group.
type comboConfig struct {
	// Name is the client-visible model id that triggers the combo.
	Name string `yaml:"name"`
	// Models is the ordered member list (provider prefixes such as
	// "deepseek/deepseek-v4-pro" are allowed; the host resolves them).
	Models []string `yaml:"models"`
	// Strategy overrides pluginConfig.DefaultStrategy for this combo.
	Strategy string `yaml:"strategy"`
	// Description is optional, informational only.
	Description string `yaml:"description"`
}

type pluginConfig struct {
	Enabled         bool                `yaml:"enabled"`
	DefaultStrategy string              `yaml:"default_strategy"`
	Combos          []comboConfig       `yaml:"combos"`
	Capabilities    map[string][]string `yaml:"capabilities"`
	// CooldownSeconds is accepted for future failure memory; currently unused.
	CooldownSeconds int `yaml:"cooldown_seconds"`
}

// currentConfig holds the last configuration the host delivered.
var currentConfig atomic.Value

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Enabled:         true,
		DefaultStrategy: strategyFallback,
	}
}

func configure(raw []byte) error {
	var req lifecycleRequest
	if len(raw) > 0 {
		if errUnmarshal := yamlToLifecycle(raw, &req); errUnmarshal != nil {
			return errUnmarshal
		}
	}
	cfg := defaultPluginConfig()
	if len(req.ConfigYAML) > 0 {
		decoded, errDecode := decodeConfig(req.ConfigYAML)
		if errDecode != nil {
			return errDecode
		}
		cfg = decoded
	}
	currentConfig.Store(cfg)
	return nil
}

func yamlToLifecycle(raw []byte, req *lifecycleRequest) error {
	// The host sends JSON on the RPC channel; ConfigYAML is base64 in JSON but
	// decoded to raw bytes by encoding/json for []byte fields.
	return json.Unmarshal(raw, req)
}

func decodeConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	if errUnmarshal := yaml.Unmarshal(raw, &cfg); errUnmarshal != nil {
		return pluginConfig{}, errUnmarshal
	}
	cfg.DefaultStrategy = normalizeStrategy(cfg.DefaultStrategy)
	if cfg.DefaultStrategy == "" {
		cfg.DefaultStrategy = strategyFallback
	}
	for i := range cfg.Combos {
		combo := &cfg.Combos[i]
		combo.Name = strings.TrimSpace(combo.Name)
		combo.Strategy = normalizeStrategy(combo.Strategy)
		if combo.Strategy == "" {
			combo.Strategy = cfg.DefaultStrategy
		}
		members := make([]string, 0, len(combo.Models))
		seen := make(map[string]struct{}, len(combo.Models))
		for _, member := range combo.Models {
			member = strings.TrimSpace(member)
			if member == "" {
				continue
			}
			if _, ok := seen[member]; ok {
				continue
			}
			seen[member] = struct{}{}
			members = append(members, member)
		}
		combo.Models = members
	}
	normalized := make(map[string][]string, len(cfg.Capabilities))
	for model, caps := range cfg.Capabilities {
		model = strings.ToLower(strings.TrimSpace(model))
		if model == "" {
			continue
		}
		normalized[model] = normalizeCapabilityNames(caps)
	}
	cfg.Capabilities = normalized
	return cfg, nil
}

func loadedConfig() pluginConfig {
	raw := currentConfig.Load()
	if cfg, ok := raw.(pluginConfig); ok {
		return cfg
	}
	return defaultPluginConfig()
}

// normalizeStrategy returns "" for an unset strategy so callers can distinguish
// "inherit the default" from an explicit choice.
func normalizeStrategy(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "":
		return ""
	case strategyFallback:
		return strategyFallback
	case strategyRoundRobin, "round_robin", "roundrobin":
		return strategyRoundRobin
	default:
		return strategyFallback
	}
}

func (c pluginConfig) comboByName(name string) (comboConfig, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return comboConfig{}, false
	}
	for _, combo := range c.Combos {
		if combo.Name == name {
			return combo, true
		}
	}
	return comboConfig{}, false
}
