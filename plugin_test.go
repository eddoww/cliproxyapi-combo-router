package main

import "testing"

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
