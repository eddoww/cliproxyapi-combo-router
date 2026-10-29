package main

import (
	"encoding/json"
	"testing"
)

func TestDetectOpenAIChatVision(t *testing.T) {
	body := []byte(`{
		"model": "smart",
		"messages": [
			{"role": "user", "content": "older turn"},
			{"role": "assistant", "content": "ok"},
			{"role": "user", "content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}}
			]}
		]
	}`)
	required := detectRequiredCapabilities("openai", body)
	if _, ok := required[capVision]; !ok {
		t.Fatalf("expected vision capability, got %v", required)
	}
}

func TestDetectIgnoresHistoryMedia(t *testing.T) {
	body := []byte(`{
		"model": "smart",
		"messages": [
			{"role": "user", "content": [{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}}]},
			{"role": "assistant", "content": "I saw it"},
			{"role": "user", "content": "and now?"}
		]
	}`)
	required := detectRequiredCapabilities("openai", body)
	if _, ok := required[capVision]; ok {
		t.Fatalf("history media must not pin the combo to vision: %v", required)
	}
}

func TestDetectClaudeToolsAndPDF(t *testing.T) {
	body := []byte(`{
		"model": "smart",
		"tools": [{"name": "search", "input_schema": {"type": "object"}}],
		"messages": [{"role": "user", "content": [
			{"type": "text", "text": "summarise"},
			{"type": "document", "source": {"type": "base64", "media_type": "application/pdf", "data": "AAA"}}
		]}]
	}`)
	required := detectRequiredCapabilities("claude", body)
	if _, ok := required[capTools]; !ok {
		t.Fatalf("expected tools capability, got %v", required)
	}
	if _, ok := required[capPDF]; !ok {
		t.Fatalf("expected pdf capability, got %v", required)
	}
}

func TestDetectGeminiAudio(t *testing.T) {
	body := []byte(`{
		"contents": [{"role": "user", "parts": [
			{"text": "transcribe"},
			{"inlineData": {"mimeType": "audio/wav", "data": "AAA"}}
		]}]
	}`)
	required := detectRequiredCapabilities("gemini", body)
	if _, ok := required[capAudio]; !ok {
		t.Fatalf("expected audio capability, got %v", required)
	}
}

func TestOrderMembersKeepsFallbackAndTiers(t *testing.T) {
	cfg := pluginConfig{DefaultStrategy: strategyFallback, Capabilities: map[string][]string{}}
	combo := comboConfig{Name: "smart", Models: []string{"deepseek-v4-pro", "grok-4.7", "gpt-5"}}
	required := map[string]struct{}{capVision: {}}
	ordered := orderMembers(combo, cfg, required, false)
	if len(ordered) != 3 {
		t.Fatalf("members must never be dropped, got %v", ordered)
	}
	if ordered[0] == "deepseek-v4-pro" {
		t.Fatalf("model without vision must not be first: %v", ordered)
	}
	if ordered[2] != "deepseek-v4-pro" {
		t.Fatalf("model without vision must sort last: %v", ordered)
	}
}

func TestOrderMembersNoRequirementsKeepsConfiguredOrder(t *testing.T) {
	cfg := pluginConfig{DefaultStrategy: strategyFallback}
	combo := comboConfig{Name: "smart", Models: []string{"a", "b", "c"}}
	ordered := orderMembers(combo, cfg, nil, false)
	want := []string{"a", "b", "c"}
	for i := range want {
		if ordered[i] != want[i] {
			t.Fatalf("expected %v, got %v", want, ordered)
		}
	}
}

func TestOrderMembersRoundRobinAdvances(t *testing.T) {
	cfg := pluginConfig{DefaultStrategy: strategyRoundRobin}
	combo := comboConfig{Name: "rr-test", Models: []string{"a", "b", "c"}}
	first := orderMembers(combo, cfg, nil, true)
	second := orderMembers(combo, cfg, nil, true)
	if first[0] == second[0] {
		t.Fatalf("round-robin must rotate, got %v then %v", first, second)
	}
}

func TestOrderMembersNeverRoutesComboToItself(t *testing.T) {
	cfg := pluginConfig{DefaultStrategy: strategyFallback}
	combo := comboConfig{Name: "smart", Models: []string{"smart", "gpt-5"}}
	ordered := orderMembers(combo, cfg, nil, false)
	if len(ordered) != 1 || ordered[0] != "gpt-5" {
		t.Fatalf("expected only gpt-5, got %v", ordered)
	}
}

func TestRetryableStatus(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 504, 401, 403} {
		if !retryableStatus(status) {
			t.Fatalf("status %d should be retryable", status)
		}
	}
	for _, status := range []int{400, 413, 422} {
		if retryableStatus(status) {
			t.Fatalf("status %d should not be retryable", status)
		}
	}
}

func TestDetectAssistantPrefillInspectsUserTurn(t *testing.T) {
	body := []byte(`{
		"model": "smart",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,AAA"}}
			]},
			{"role": "assistant", "content": "It is"}
		]
	}`)
	required := detectRequiredCapabilities("openai", body)
	if _, ok := required[capVision]; !ok {
		t.Fatalf("assistant prefill must not hide the user image: %v", required)
	}
}

func TestDetectNestedToolResultFile(t *testing.T) {
	body := []byte(`{
		"model": "smart",
		"messages": [{"role": "user", "content": [
			{"type": "tool_result", "tool_use_id": "t1", "content": [
				{"type": "text", "text": "read it"},
				{"type": "image", "source": {"media_type": "image/png", "data": "AAA"}}
			]}
		]}]
	}`)
	required := detectRequiredCapabilities("claude", body)
	if _, ok := required[capVision]; !ok {
		t.Fatalf("tool result images must be detected: %v", required)
	}
}

func TestQualifiedCapabilityOverride(t *testing.T) {
	overrides := map[string][]string{"openrouter/gpt-5": {capTools}}
	have := capabilitiesFor("openrouter/gpt-5", overrides)
	if _, ok := have[capVision]; ok {
		t.Fatalf("qualified override must replace built-ins: %v", have)
	}
	if _, ok := have[capTools]; !ok {
		t.Fatalf("qualified override must be applied: %v", have)
	}
}

func TestGPTOSSIsNotVisionCapable(t *testing.T) {
	have := capabilitiesFor("gpt-oss-120b", nil)
	if _, ok := have[capVision]; ok {
		t.Fatalf("gpt-oss must not claim vision: %v", have)
	}
}

func TestDefaultStrategyInherited(t *testing.T) {
	cfg, errDecode := decodeConfig([]byte("default_strategy: round-robin\ncombos:\n  - name: c\n    models: [a, b]\n"))
	if errDecode != nil {
		t.Fatalf("decode: %v", errDecode)
	}
	if cfg.Combos[0].Strategy != strategyRoundRobin {
		t.Fatalf("combo must inherit default_strategy, got %q", cfg.Combos[0].Strategy)
	}
}

func TestRoundRobinAdvancesOncePerExecution(t *testing.T) {
	cfg := pluginConfig{DefaultStrategy: strategyRoundRobin}
	combo := comboConfig{Name: "once", Models: []string{"a", "b", "c"}}
	first := orderMembers(combo, cfg, nil, true)
	second := orderMembers(combo, cfg, nil, true)
	if first[0] != "a" || second[0] != "b" {
		t.Fatalf("expected a then b, got %v then %v", first, second)
	}
}

func TestHasRealContent(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"done marker", "data: [DONE]\n", false},
		{"claude message_stop", "event: message_stop\ndata: {\"type\":\"message_stop\"}\n", false},
		{"claude message_start", "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"content\":[]}}\n", false},
		{"responses created", "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"output\":[]}}\n", false},
		{"chat role only", "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n", false},
		{"chat content", "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n", true},
		{"chat empty tool delta", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[]}}]}\n", false},
		{"chat tool call", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\"}]}}]}\n", true},
		{"chat empty content", "data: {\"choices\":[{\"delta\":{\"content\":\"\"}}]}\n", false},
		{"gemini candidates", "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hi\"}]}}]}\n", true},
		{"claude thinking delta", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"pondering\"}}\n", true},
		{"claude tool_use start", "data: {\"type\":\"content_block_start\",\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"x\",\"input\":{}}}\n", true},
		{"event line only", "event: message_start\n", false},
		{"responses empty item", "data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\",\"content\":[]}}\n", false},
		{"claude text delta", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n", true},
		{"claude empty delta", "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"\"}}\n", false},
		{"non-sse payload", "raw-bytes", true},
	}
	for _, testCase := range cases {
		if got := hasRealContent([]byte(testCase.payload)); got != testCase.want {
			t.Fatalf("%s: hasRealContent=%v want %v", testCase.name, got, testCase.want)
		}
	}
}

func TestErrorEnvelopeCarriesStatus(t *testing.T) {
	raw := errorEnvelopeStatus("executor_error", "bad request", 400)
	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		t.Fatalf("decode: %v", errUnmarshal)
	}
	if env.OK || env.Error == nil || env.Error.HTTPStatus != 400 {
		t.Fatalf("expected http_status 400, got %s", raw)
	}
}
