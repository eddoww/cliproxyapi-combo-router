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
		if outcome.status != 0 && !retryableStatus(outcome.status) {
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
				return streamOutcome{terminal: status != 0 && !retryableStatus(status), status: status, err: fmt.Errorf("%s: %w", member, errRead)}
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
// than protocol framing. Unknown shapes count as content so nothing is dropped
// by accident.
func hasRealContent(payload []byte) bool {
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return false
	}
	sawData := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		sawData = true
		dataText := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if dataText == "" || dataText == "[DONE]" {
			continue
		}
		var event map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal([]byte(dataText), &event); errUnmarshal != nil {
			return true
		}
		if eventHasContent(event) {
			return true
		}
	}
	// Non-SSE payloads (or SSE without data lines) are forwarded as content.
	return !sawData
}

func eventHasContent(event map[string]json.RawMessage) bool {
	// OpenAI chat: choices[].delta with anything beyond role.
	if rawChoices, ok := event["choices"]; ok {
		var choices []map[string]json.RawMessage
		if errUnmarshal := json.Unmarshal(rawChoices, &choices); errUnmarshal == nil {
			for _, choice := range choices {
				if rawDelta, okDelta := choice["delta"]; okDelta {
					var delta map[string]json.RawMessage
					if errUnmarshal := json.Unmarshal(rawDelta, &delta); errUnmarshal == nil {
						for key := range delta {
							if key != "role" {
								return true
							}
						}
					}
				}
				if rawMessage, okMessage := choice["message"]; okMessage && len(rawMessage) > 2 {
					return true
				}
			}
		}
	}
	// OpenAI Responses and Claude deltas.
	if rawDelta, ok := event["delta"]; ok {
		var deltaText string
		if errUnmarshal := json.Unmarshal(rawDelta, &deltaText); errUnmarshal == nil {
			if strings.TrimSpace(deltaText) != "" {
				return true
			}
		} else {
			var deltaObject map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal(rawDelta, &deltaObject); errUnmarshal == nil {
				if objectHasContent(deltaObject) {
					return true
				}
			} else {
				return true
			}
		}
	}
	if objectHasContent(event) {
		return true
	}
	// Responses tool/answer items.
	if rawOutput, ok := event["output"]; ok {
		var output []json.RawMessage
		if errUnmarshal := json.Unmarshal(rawOutput, &output); errUnmarshal == nil && len(output) > 0 {
			return true
		}
	}
	if rawItem, ok := event["item"]; ok && len(rawItem) > 2 {
		return true
	}
	return false
}

// objectHasContent looks for non-empty content-bearing fields one level deep.
func objectHasContent(object map[string]json.RawMessage) bool {
	for _, key := range []string{"text", "arguments", "partial_json", "input_json_delta", "reasoning_content", "content"} {
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
		}
	}
	return false
}
