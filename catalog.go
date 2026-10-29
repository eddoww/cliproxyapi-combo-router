package main

import "strings"

// builtinCapabilityPrefixes maps a model-family prefix to the capabilities the
// upstream accepts. Entries are deliberately conservative: a false positive puts
// an incapable member ahead of a capable one, which is worse than the reverse.
// Unknown models advertise nothing and stay in the chain behind capable members.
var builtinCapabilityPrefixes = []struct {
	prefix string
	caps   []string
}{
	{"claude", []string{capVision, capPDF, capTools}},
	{"gpt-5", []string{capVision, capPDF, capTools}},
	{"gpt-4", []string{capVision, capPDF, capTools}},
	{"gpt-oss", []string{capTools}},
	{"gpt-3", []string{capTools}},
	{"o1", []string{capVision, capPDF, capTools}},
	{"o3", []string{capVision, capPDF, capTools}},
	{"o4", []string{capVision, capPDF, capTools}},
	{"gemini", []string{capVision, capPDF, capAudio, capVideo, capTools}},
	{"grok-4", []string{capVision, capTools}},
	{"grok-3", []string{capVision, capTools}},
	{"grok-2", []string{capTools}},
	{"kimi", []string{capVision, capTools}},
	{"qwen-vl", []string{capVision, capPDF, capTools}},
	{"glm-4v", []string{capVision, capTools}},
	{"deepseek", []string{capTools}},
	{"mistral", []string{capTools}},
	{"codestral", []string{capTools}},
	{"llama", []string{capTools}},
	{"qwen", []string{capTools}},
}

// capabilitiesFor returns the capability set for a model id. Overrides are
// matched against the full qualified id first ("openrouter/gpt-5"), then against
// the unqualified model name ("gpt-5"), so both spellings work.
func capabilitiesFor(model string, overrides map[string][]string) map[string]struct{} {
	full := strings.ToLower(strings.TrimSpace(model))
	unqualified := full
	if idx := strings.LastIndex(full, "/"); idx >= 0 {
		unqualified = full[idx+1:]
	}
	if overrides != nil {
		if caps, ok := overrides[full]; ok {
			return toCapSet(caps)
		}
		if caps, ok := overrides[unqualified]; ok {
			return toCapSet(caps)
		}
	}
	for _, entry := range builtinCapabilityPrefixes {
		if strings.HasPrefix(unqualified, entry.prefix) {
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
