package main

import "strings"

// builtinCapabilityPrefixes maps a model id prefix to the capabilities the
// upstream model family accepts. Unknown models deliberately advertise nothing,
// so they sort behind known-capable members while still staying in the fallback
// chain.
var builtinCapabilityPrefixes = []struct {
	prefix string
	caps   []string
}{
	{"claude", []string{capVision, capPDF, capTools}},
	{"gpt-", []string{capVision, capPDF, capTools}},
	{"o1", []string{capVision, capPDF, capTools}},
	{"o3", []string{capVision, capPDF, capTools}},
	{"o4", []string{capVision, capPDF, capTools}},
	{"gemini", []string{capVision, capPDF, capAudio, capVideo, capTools}},
	{"grok-4", []string{capVision, capPDF, capTools}},
	{"grok-3", []string{capVision, capTools}},
	{"grok-2", []string{capVision, capTools}},
	{"kimi", []string{capVision, capTools}},
	{"qwen-vl", []string{capVision, capPDF, capTools}},
	{"glm-4v", []string{capVision, capTools}},
	{"deepseek", []string{capTools}},
	{"mistral", []string{capTools}},
	{"codestral", []string{capTools}},
	{"llama", []string{capTools}},
	{"qwen", []string{capTools}},
}

// capabilitiesFor returns the capability set for a model id. The model may be
// prefixed ("openai/gpt-5", "deepseek/deepseek-v4-pro"); the prefix is ignored
// for lookup. Config overrides replace the built-in answer entirely.
func capabilitiesFor(model string, overrides map[string][]string) map[string]struct{} {
	lookup := strings.ToLower(strings.TrimSpace(model))
	if idx := strings.LastIndex(lookup, "/"); idx >= 0 {
		lookup = lookup[idx+1:]
	}
	if overrides != nil {
		if caps, ok := overrides[lookup]; ok {
			return toCapSet(caps)
		}
	}
	for _, entry := range builtinCapabilityPrefixes {
		if strings.HasPrefix(lookup, entry.prefix) {
			return toCapSet(entry.caps)
		}
	}
	return map[string]struct{}{}
}

func toCapSet(caps []string) map[string]struct{} {
	set := make(map[string]struct{}, len(caps))
	for _, cap := range caps {
		set[cap] = struct{}{}
	}
	return set
}

func supportsAll(have map[string]struct{}, want map[string]struct{}) bool {
	for cap := range want {
		if _, ok := have[cap]; !ok {
			return false
		}
	}
	return true
}
