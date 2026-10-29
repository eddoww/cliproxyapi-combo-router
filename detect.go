package main

import (
	"encoding/json"
	"strings"
)

// Capability names understood by the combo router.
const (
	capVision = "vision"
	capAudio  = "audio"
	capVideo  = "video"
	capPDF    = "pdf"
	capTools  = "tools"
)

// hardCapabilities must be satisfied for a member to be tried early. They
// describe input the upstream literally cannot see or decode.
var hardCapabilities = map[string]struct{}{
	capVision: {},
	capAudio:  {},
	capVideo:  {},
	capPDF:    {},
}

func normalizeCapabilityNames(caps []string) []string {
	out := make([]string, 0, len(caps))
	seen := make(map[string]struct{}, len(caps))
	for _, cap := range caps {
		name := strings.ToLower(strings.TrimSpace(cap))
		switch name {
		case "image", "images", "vision":
			name = capVision
		case "audio", "audioinput", "audio_input":
			name = capAudio
		case "video", "videoinput", "video_input":
			name = capVideo
		case "pdf", "document":
			name = capPDF
		case "tools", "tool", "functions":
			name = capTools
		default:
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

// detectRequiredCapabilities inspects the current user turn of a request body
// and returns the capabilities it needs. Only the trailing user turn is
// inspected so that images from older history do not pin a whole conversation
// to a vision model.
func detectRequiredCapabilities(sourceFormat string, body []byte) map[string]struct{} {
	required := make(map[string]struct{})
	if len(body) == 0 {
		return required
	}
	var payload map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &payload); errUnmarshal != nil || payload == nil {
		return required
	}

	if rawTools, ok := payload["tools"]; ok {
		var tools []json.RawMessage
		if errTools := json.Unmarshal(rawTools, &tools); errTools == nil && len(tools) > 0 {
			required[capTools] = struct{}{}
		}
	}
	if rawFunctions, ok := payload["functions"]; ok {
		var functions []json.RawMessage
		if errFunctions := json.Unmarshal(rawFunctions, &functions); errFunctions == nil && len(functions) > 0 {
			required[capTools] = struct{}{}
		}
	}

	switch normalizeFormat(sourceFormat) {
	case "claude":
		if raw, ok := payload["messages"]; ok {
			var messages []json.RawMessage
			if errMessages := json.Unmarshal(raw, &messages); errMessages == nil {
				for _, rawMessage := range trailingTurn(messages, "assistant", "model") {
					scanMessage(rawMessage, required)
				}
			}
		}
	case "gemini":
		if raw, ok := payload["contents"]; ok {
			var contents []json.RawMessage
			if errContents := json.Unmarshal(raw, &contents); errContents == nil {
				for _, rawContent := range trailingTurn(contents, "model", "assistant") {
					scanMessage(rawContent, required)
				}
			}
		}
	default:
		// OpenAI chat/completions and OpenAI Responses.
		if raw, ok := payload["messages"]; ok {
			var messages []json.RawMessage
			if errMessages := json.Unmarshal(raw, &messages); errMessages == nil {
				for _, rawMessage := range trailingTurn(messages, "assistant", "model") {
					scanMessage(rawMessage, required)
				}
			}
		}
		if raw, ok := payload["input"]; ok {
			var items []json.RawMessage
			if errItems := json.Unmarshal(raw, &items); errItems == nil {
				for _, rawItem := range trailingTurn(items, "assistant", "model") {
					scanMessage(rawItem, required)
				}
			} else {
				// input may also be a plain string prompt: nothing to detect.
				_ = raw
			}
		}
	}
	return required
}

// trailingTurn returns the items after the last assistant/model message. A
// current user turn can span several items (text + image), so all of them are
// returned.
func trailingTurn(items []json.RawMessage, assistantRoles ...string) []json.RawMessage {
	assistant := make(map[string]struct{}, len(assistantRoles))
	for _, role := range assistantRoles {
		assistant[role] = struct{}{}
	}
	start := 0
	for i := len(items) - 1; i >= 0; i-- {
		var probe struct {
			Role string `json:"role"`
		}
		if errUnmarshal := json.Unmarshal(items[i], &probe); errUnmarshal != nil {
			continue
		}
		if _, isAssistant := assistant[strings.ToLower(strings.TrimSpace(probe.Role))]; isAssistant {
			start = i + 1
			break
		}
	}
	return items[start:]
}

func scanMessage(raw json.RawMessage, required map[string]struct{}) {
	var message map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(raw, &message); errUnmarshal != nil || message == nil {
		return
	}

	// Ollama/Hermes message-level image arrays.
	if rawImages, ok := message["images"]; ok {
		var images []json.RawMessage
		if errImages := json.Unmarshal(rawImages, &images); errImages == nil && len(images) > 0 {
			required[capVision] = struct{}{}
		}
	}

	// Vercel AI SDK / Hermes attachments.
	for _, key := range []string{"experimental_attachments", "attachments"} {
		if rawAttachments, ok := message[key]; ok {
			var attachments []map[string]json.RawMessage
			if errAttachments := json.Unmarshal(rawAttachments, &attachments); errAttachments == nil {
				for _, attachment := range attachments {
					if mime := jsonString(attachment["contentType"], attachment["mediaType"]); mime != "" {
						addByMime(mime, required)
						continue
					}
					if len(attachment["url"]) > 0 || len(attachment["data"]) > 0 {
						required[capVision] = struct{}{}
					}
				}
			}
		}
	}

	if rawContent, ok := message["content"]; ok {
		scanContent(rawContent, required)
	}
	if rawParts, ok := message["parts"]; ok { // Gemini content
		scanContent(rawParts, required)
	}
}

func scanContent(raw json.RawMessage, required map[string]struct{}) {
	if len(raw) == 0 {
		return
	}
	// Plain string content.
	var asString string
	if errString := json.Unmarshal(raw, &asString); errString == nil {
		return
	}
	var blocks []json.RawMessage
	if errBlocks := json.Unmarshal(raw, &blocks); errBlocks != nil {
		return
	}
	for _, rawBlock := range blocks {
		var block map[string]json.RawMessage
		if errBlock := json.Unmarshal(rawBlock, &block); errBlock != nil || block == nil {
			continue
		}
		addBlockCapability(block, required)
	}
}

func addBlockCapability(block map[string]json.RawMessage, required map[string]struct{}) {
	blockType := strings.ToLower(strings.TrimSpace(jsonString(block["type"])))
	switch blockType {
	case "image_url", "image", "input_image":
		required[capVision] = struct{}{}
	case "input_audio", "audio", "audio_url":
		required[capAudio] = struct{}{}
	case "input_video", "video", "video_url":
		required[capVideo] = struct{}{}
	case "file", "input_file", "document":
		if mime := blockMime(block); mime != "" {
			addByMime(mime, required)
		} else {
			required[capPDF] = struct{}{}
		}
	}

	// Modality hints carried by nested objects (Gemini inlineData, Claude
	// source.media_type, OpenAI file objects).
	if mime := blockMime(block); mime != "" {
		addByMime(mime, required)
	}
}

func blockMime(block map[string]json.RawMessage) string {
	for _, key := range []string{"source", "inlineData", "inline_data", "fileData", "file_data", "file"} {
		raw, ok := block[key]
		if !ok {
			continue
		}
		var nested map[string]json.RawMessage
		if errNested := json.Unmarshal(raw, &nested); errNested != nil || nested == nil {
			continue
		}
		if mime := jsonString(nested["media_type"], nested["mimeType"], nested["mime_type"]); mime != "" {
			return mime
		}
		if data := jsonString(nested["file_data"], nested["data"]); strings.HasPrefix(data, "data:") {
			if idx := strings.Index(data, ";"); idx > 5 {
				return data[5:idx]
			}
		}
	}
	return ""
}

func addByMime(mime string, required map[string]struct{}) {
	mime = strings.ToLower(strings.TrimSpace(mime))
	switch {
	case strings.HasPrefix(mime, "image/"):
		required[capVision] = struct{}{}
	case strings.HasPrefix(mime, "audio/"):
		required[capAudio] = struct{}{}
	case strings.HasPrefix(mime, "video/"):
		required[capVideo] = struct{}{}
	case mime == "application/pdf":
		required[capPDF] = struct{}{}
	}
}

func jsonString(raws ...json.RawMessage) string {
	for _, raw := range raws {
		if len(raw) == 0 {
			continue
		}
		var value string
		if errUnmarshal := json.Unmarshal(raw, &value); errUnmarshal == nil {
			return value
		}
	}
	return ""
}

func normalizeFormat(sourceFormat string) string {
	switch strings.ToLower(strings.TrimSpace(sourceFormat)) {
	case "claude", "anthropic":
		return "claude"
	case "gemini", "google":
		return "gemini"
	case "codex", "openai-response", "openai-responses", "responses":
		return "openai-response"
	default:
		return "openai"
	}
}
