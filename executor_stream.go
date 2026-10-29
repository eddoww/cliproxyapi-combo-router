package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// preCommitBufferLimit bounds how long a member may emit framing-only events
// before the stream is committed to it anyway.
const preCommitBufferLimit = 256 << 10

type rpcStreamEmitRequest struct {
	StreamID    string           `json:"stream_id"`
	Payload     []byte           `json:"payload,omitempty"`
	Error       string           `json:"error,omitempty"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID    string           `json:"stream_id"`
	Error       string           `json:"error,omitempty"`
	ErrorDetail *pluginabi.Error `json:"error_detail,omitempty"`
}

func emitStreamChunk(streamID string, payload []byte) error {
	if strings.TrimSpace(streamID) == "" {
		return fmt.Errorf("plugin stream id is required")
	}
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, rpcStreamEmitRequest{
		StreamID: streamID,
		Payload:  payload,
	})
	return errCall
}

func closePluginStream(streamID, errMsg string) {
	closePluginStreamStatus(streamID, errMsg, 0)
}

// closePluginStreamStatus reports a structured error so the client keeps the
// upstream status instead of a generic stream failure.
func closePluginStreamStatus(streamID, errMsg string, status int) {
	if strings.TrimSpace(streamID) == "" {
		return
	}
	request := rpcStreamCloseRequest{StreamID: streamID, Error: strings.TrimSpace(errMsg)}
	if status > 0 {
		request.ErrorDetail = &pluginabi.Error{
			Code:       "executor_error",
			Message:    strings.TrimSpace(errMsg),
			HTTPStatus: status,
		}
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, request)
}

func closeHostModelStream(hostStreamID string) error {
	if strings.TrimSpace(hostStreamID) == "" {
		return nil
	}
	_, errCall := callHost(pluginabi.MethodHostModelStreamClose, pluginapi.HostModelStreamCloseRequest{StreamID: hostStreamID})
	return errCall
}

func executeComboStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	streamID := strings.TrimSpace(req.StreamID)
	if streamID == "" {
		return errorEnvelope("executor_error", "stream_id is required for executor.execute_stream"), nil
	}
	if !startWorker() {
		return errorEnvelopeStatus("executor_error", "combo-router is shutting down", http.StatusServiceUnavailable), nil
	}
	go func() {
		defer finishWorker()
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("combo stream orchestration panic: %v", recovered))
			}
		}()
		status, errRun := runComboStream(req, streamID)
		if errRun != nil {
			closePluginStreamStatus(streamID, errRun.Error(), status)
			return
		}
		closePluginStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// streamOutcome describes what happened to one member's stream.
type streamOutcome struct {
	// committed means payload has reached the client; no fallback is allowed.
	committed bool
	// terminal means the failure must not be retried (client gone, bad request).
	terminal bool
	status   int
	err      error
}

// runComboStream tries members in order. Each member's framing is buffered until
// it produces real content, so a member that fails before output leaves no trace
// for the client. After output, the stream is committed and protocol events are
// forwarded verbatim.
func runComboStream(req rpcExecutorRequest, pluginStreamID string) (int, error) {
	ordered, errPlan := planFor(req.ExecutorRequest)
	if errPlan != nil {
		return http.StatusInternalServerError, errPlan
	}
	format := protocolFormat(req.ExecutorRequest)
	body := requestBody(req.ExecutorRequest)

	var lastErr error
	lastStatus := 0
	for _, member := range ordered {
		outcome := streamMember(req.HostCallbackID, pluginStreamID, format, member, body)
		if outcome.committed || outcome.terminal {
			return outcome.status, outcome.err
		}
		if outcome.err == nil {
			outcome.err = fmt.Errorf("%s: upstream stream produced no content", member)
			outcome.status = http.StatusBadGateway
		}
		lastErr = outcome.err
		lastStatus = outcome.status
		if !retryableError(outcome.err, outcome.status) {
			return outcome.status, outcome.err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q: no member produced a stream", req.Model)
	}
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	return lastStatus, lastErr
}

// streamMember forwards one combo member's stream. Framing-only chunks are held
// back until the first real content arrives or the buffer limit is reached.
func streamMember(hostCallbackID, pluginStreamID, format, member string, body []byte) streamOutcome {
	raw, errCall := callHost(pluginabi.MethodHostModelExecuteStream, hostModelExecuteRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: format,
			ExitProtocol:  format,
			Model:         member,
			Stream:        true,
			Body:          body,
		},
		HostCallbackID: hostCallbackID,
	})
	if errCall != nil {
		status := statusOf(errCall)
		return streamOutcome{
			terminal: !retryableError(errCall, status),
			status:   status,
			err:      fmt.Errorf("%s: %w", member, errCall),
		}
	}
	var resp pluginapi.HostModelStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return streamOutcome{terminal: true, status: http.StatusInternalServerError, err: errDecode}
	}
	if resp.StatusCode >= 400 {
		_ = closeHostModelStream(resp.StreamID)
		errStatus := fmt.Errorf("%s: upstream status %d", member, resp.StatusCode)
		return streamOutcome{
			terminal: !retryableError(errStatus, resp.StatusCode),
			status:   resp.StatusCode,
			err:      errStatus,
		}
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return streamOutcome{status: http.StatusBadGateway, err: fmt.Errorf("%s: host returned an empty stream id", member)}
	}
	defer func() { _ = closeHostModelStream(resp.StreamID) }()

	var pending [][]byte
	pendingBytes := 0
	committed := false

	commit := func() error {
		for _, chunk := range pending {
			if errEmit := emitStreamChunk(pluginStreamID, chunk); errEmit != nil {
				return errEmit
			}
		}
		pending = nil
		committed = true
		return nil
	}

	for {
		chunkRaw, errRead := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: resp.StreamID})
		if errRead != nil {
			status := statusOf(errRead)
			if !committed {
				errReadWrapped := fmt.Errorf("%s: %w", member, errRead)
				return streamOutcome{terminal: !retryableError(errReadWrapped, status), status: status, err: errReadWrapped}
			}
			return streamOutcome{committed: true, terminal: true, status: status, err: fmt.Errorf("%s: %w", member, errRead)}
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if errDecode := json.Unmarshal(chunkRaw, &chunk); errDecode != nil {
			return streamOutcome{committed: committed, terminal: true, status: http.StatusInternalServerError, err: errDecode}
		}
		if chunk.Error != "" {
			if !committed {
				return streamOutcome{status: http.StatusBadGateway, err: fmt.Errorf("%s: %s", member, chunk.Error)}
			}
			return streamOutcome{committed: true, terminal: true, status: http.StatusBadGateway, err: fmt.Errorf("%s: %s", member, chunk.Error)}
		}
		if len(chunk.Payload) > 0 {
			if committed {
				if errEmit := emitStreamChunk(pluginStreamID, bytes.Clone(chunk.Payload)); errEmit != nil {
					return streamOutcome{committed: true, terminal: true, status: http.StatusBadGateway, err: fmt.Errorf("%s: client stream closed: %w", member, errEmit)}
				}
			} else {
				pending = append(pending, bytes.Clone(chunk.Payload))
				pendingBytes += len(chunk.Payload)
				if hasRealContent(chunk.Payload) || pendingBytes >= preCommitBufferLimit {
					if errCommit := commit(); errCommit != nil {
						// The client is gone; never start another member.
						return streamOutcome{committed: true, terminal: true, status: http.StatusBadGateway, err: fmt.Errorf("%s: client stream closed: %w", member, errCommit)}
					}
				}
			}
		}
		if chunk.Done {
			break
		}
	}
	if !committed {
		// Only framing arrived: fall back without exposing it to the client.
		return streamOutcome{status: http.StatusBadGateway, err: fmt.Errorf("%s: stream ended without content", member)}
	}
	return streamOutcome{committed: true, status: http.StatusOK}
}

// hasRealContent reports whether an SSE chunk carries actual model output rather
// than protocol framing. Only a strict allowlist of empty shapes counts as
// framing; anything unrecognised is treated as content so nothing is dropped.
func hasRealContent(payload []byte) bool {
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return false
	}
	sawSSE := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case strings.HasPrefix(line, "data:"):
			sawSSE = true
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "" || data == "[DONE]" {
				continue
			}
			var event map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal([]byte(data), &event); errUnmarshal != nil {
				return true
			}
			if !isFramingEvent(event) {
				return true
			}
		case strings.HasPrefix(line, "event:"), strings.HasPrefix(line, "id:"), strings.HasPrefix(line, ":"):
			sawSSE = true
		default:
			// Plain (non-SSE) body: forward it.
			return true
		}
	}
	return !sawSSE
}

// isFramingEvent returns true only for events that carry no model output.
func isFramingEvent(event map[string]json.RawMessage) bool {
	if rawChoices, ok := event["choices"]; ok {
		var choices []map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(rawChoices, &choices); errUnmarshal == nil {
			for _, choice := range choices {
				if rawDelta, okDelta := choice["delta"]; okDelta {
					var delta map[string]json.RawMessage
					if errUnmarshal := json.Unmarshal(rawDelta, &delta); errUnmarshal == nil && deltaHasContent(delta) {
						return false
					}
				}
				if rawMessage, okMessage := choice["message"]; okMessage {
					var message map[string]json.RawMessage
					if errUnmarshal := json.Unmarshal(rawMessage, &message); errUnmarshal == nil && objectHasContent(message) {
						return false
					}
				}
			}
		}
		return true
	}

	switch strings.ToLower(jsonString(event["type"])) {
	case "ping", "message_start", "message_stop", "message_delta", "content_block_stop",
		"response.created", "response.in_progress", "response.queued", "response.output_item.done":
		return true
	case "content_block_start":
		if raw, ok := event["content_block"]; ok {
			var block map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal(raw, &block); errUnmarshal == nil {
				if strings.ToLower(jsonString(block["type"])) == "tool_use" {
					return false
				}
				if objectHasContent(block) {
					return false
				}
			}
		}
		return true
	case "content_block_delta", "response.output_text.delta", "response.function_call_arguments.delta",
		"response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		if raw, ok := event["delta"]; ok {
			var asText string
			if errUnmarshal := json.Unmarshal(raw, &asText); errUnmarshal == nil {
				return strings.TrimSpace(asText) == ""
			}
			var delta map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal(raw, &delta); errUnmarshal == nil {
				return !objectHasContent(delta)
			}
		}
		return true
	case "response.output_item.added":
		if raw, ok := event["item"]; ok {
			var item map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal(raw, &item); errUnmarshal == nil {
				if objectHasContent(item) {
					return false
				}
			}
		}
		return true
	case "":
		// Untyped shape (Gemini chunks and friends): non-empty means content.
		return len(event) == 0
	default:
		return false
	}
}

// deltaHasContent ignores the role marker and empty containers.
func deltaHasContent(delta map[string]json.RawMessage) bool {
	for key, raw := range delta {
		if key == "role" {
			continue
		}
		var asText string
		if errUnmarshal := json.Unmarshal(raw, &asText); errUnmarshal == nil {
			if strings.TrimSpace(asText) != "" {
				return true
			}
			continue
		}
		trimmed := strings.TrimSpace(string(raw))
		if trimmed == "" || trimmed == "null" || trimmed == "[]" || trimmed == "{}" {
			continue
		}
		return true
	}
	return false
}

// objectHasContent looks for non-empty content-bearing fields.
func objectHasContent(object map[string]json.RawMessage) bool {
	for _, key := range []string{
		"text", "thinking", "arguments", "partial_json", "input_json_delta",
		"reasoning_content", "content", "input",
	} {
		raw, ok := object[key]
		if !ok {
			continue
		}
		var asText string
		if errUnmarshal := json.Unmarshal(raw, &asText); errUnmarshal == nil {
			if strings.TrimSpace(asText) != "" {
				return true
			}
			continue
		}
		var asList []json.RawMessage
		if errUnmarshal := json.Unmarshal(raw, &asList); errUnmarshal == nil {
			if len(asList) > 0 {
				return true
			}
			continue
		}
		var asObject map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(raw, &asObject); errUnmarshal == nil {
			if len(asObject) > 0 {
				return true
			}
		}
	}
	return false
}
