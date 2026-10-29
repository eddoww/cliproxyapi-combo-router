package main

import (
	"encoding/json"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// comboRotationState tracks round-robin position per combo name.
var (
	rotationMu    sync.Mutex
	rotationIndex = map[string]int{}
)

func nextRotation(name string, size int) int {
	if size <= 0 {
		return 0
	}
	rotationMu.Lock()
	defer rotationMu.Unlock()
	pos := rotationIndex[name] % size
	rotationIndex[name] = (rotationIndex[name] + 1) % size
	return pos
}

// orderMembers returns the combo members in the order they should be tried.
// Members are never dropped: capability tiers only reorder them, keeping the
// fallback chain intact.
//
// Strategy round-robin rotates the configured order before tiering; fallback
// keeps the configured order.
func orderMembers(combo comboConfig, cfg pluginConfig, required map[string]struct{}, advanceRotation bool) []string {
	base := make([]string, 0, len(combo.Models))
	for _, member := range combo.Models {
		if strings.TrimSpace(member) == combo.Name {
			continue // never route a combo to itself
		}
		base = append(base, member)
	}
	if len(base) == 0 {
		return nil
	}

	strategy := combo.Strategy
	if strategy == "" {
		strategy = cfg.DefaultStrategy
	}
	if strategy == strategyRoundRobin {
		pos := 0
		if advanceRotation {
			pos = nextRotation(combo.Name, len(base))
		}
		if pos != 0 {
			rotated := make([]string, 0, len(base))
			rotated = append(rotated, base[pos:]...)
			rotated = append(rotated, base[:pos]...)
			base = rotated
		}
	}
	if len(required) == 0 || len(base) == 1 {
		return base
	}

	type scored struct {
		model string
		tier  int
	}
	scoredMembers := make([]scored, 0, len(base))
	for _, member := range base {
		scoredMembers = append(scoredMembers, scored{model: member, tier: capabilityTier(member, required, cfg.Capabilities)})
	}
	// Stable insertion sort by tier keeps equal tiers in their configured order.
	for i := 1; i < len(scoredMembers); i++ {
		current := scoredMembers[i]
		j := i - 1
		for j >= 0 && scoredMembers[j].tier > current.tier {
			scoredMembers[j+1] = scoredMembers[j]
			j--
		}
		scoredMembers[j+1] = current
	}
	ordered := make([]string, 0, len(scoredMembers))
	for _, member := range scoredMembers {
		ordered = append(ordered, member.model)
	}
	return ordered
}

// capabilityTier ranks a member: 0 satisfies all required capabilities, 1 is
// missing only soft ones, 2 is missing a hard one.
func capabilityTier(model string, required map[string]struct{}, overrides map[string][]string) int {
	have := capabilitiesFor(model, overrides)
	hardMiss := false
	softMiss := false
	for cap := range required {
		if _, ok := have[cap]; ok {
			continue
		}
		if _, isHard := hardCapabilities[cap]; isHard {
			hardMiss = true
		} else {
			softMiss = true
		}
	}
	switch {
	case hardMiss:
		return 2
	case softMiss:
		return 1
	default:
		return 0
	}
}

// routeCombo is the ModelRouter entry point. It only handles requests whose
// model id matches a configured combo; everything else falls through untouched.
func routeCombo(raw []byte) ([]byte, error) {
	var req rpcModelRouteRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !cfg.Enabled {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "combo_router_disabled"})
	}
	combo, ok := cfg.comboByName(req.RequestedModel)
	if !ok {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false})
	}
	required := detectRequiredCapabilities(req.SourceFormat, req.Body)
	ordered := orderMembers(combo, cfg, required, false)
	if len(ordered) == 0 {
		return okEnvelope(pluginapi.ModelRouteResponse{Handled: false, Reason: "combo_has_no_members"})
	}
	return okEnvelope(pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetSelf,
		Reason:     "combo:" + combo.Name,
	})
}
