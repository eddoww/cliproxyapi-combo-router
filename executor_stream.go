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
	trackWorker(1)
	go func() {
		defer trackWorker(-1)
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

// runComboStream tries members in order. Once any payload reaches the client the
// stream is committed to that member: a later failure is terminal, because
// appending another model's output would splice two answers together.
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
		emitted, status, errMember := streamMember(req.HostCallbackID, pluginStreamID, format, member, body)
		if emitted {
			// Committed: never fall back after partial output.
			return status, errMember
		}
		if errMember == nil {
			errMember = fmt.Errorf("%s: upstream stream produced no output", member)
			status = http.StatusBadGateway
		}
		lastErr = errMember
		lastStatus = status
		if status != 0 && !retryableStatus(status) {
			return status, errMember
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

// streamMember forwards one combo member's stream to the client. It reports
// whether any meaningful payload reached the client and the upstream status.
func streamMember(hostCallbackID, pluginStreamID, format, member string, body []byte) (bool, int, error) {
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
		return false, statusOf(errCall), fmt.Errorf("%s: %w", member, errCall)
	}
	var resp pluginapi.HostModelStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return false, 0, errDecode
	}
	if resp.StatusCode >= 400 {
		_ = closeHostModelStream(resp.StreamID)
		return false, resp.StatusCode, fmt.Errorf("%s: upstream status %d", member, resp.StatusCode)
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return false, http.StatusBadGateway, fmt.Errorf("%s: host returned an empty stream id", member)
	}
	defer func() { _ = closeHostModelStream(resp.StreamID) }()

	emitted := false
	for {
		chunkRaw, errRead := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: resp.StreamID})
		if errRead != nil {
			return emitted, statusOf(errRead), fmt.Errorf("%s: %w", member, errRead)
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if errDecode := json.Unmarshal(chunkRaw, &chunk); errDecode != nil {
			return emitted, 0, errDecode
		}
		if chunk.Error != "" {
			return emitted, http.StatusBadGateway, fmt.Errorf("%s: %s", member, chunk.Error)
		}
		if len(chunk.Payload) > 0 && isMeaningfulPayload(chunk.Payload) {
			if errEmit := emitStreamChunk(pluginStreamID, bytes.Clone(chunk.Payload)); errEmit != nil {
				// The client is gone or the host stream is closed; stop here
				// rather than starting another upstream member.
				return emitted, http.StatusBadGateway, fmt.Errorf("%s: client stream closed: %w", member, errEmit)
			}
			emitted = true
		}
		if chunk.Done {
			break
		}
	}
	return emitted, http.StatusOK, nil
}

// isMeaningfulPayload filters protocol framing that carries no content, such as
// an SSE terminal marker on its own, so a "successful" empty stream still falls
// back to the next member.
func isMeaningfulPayload(payload []byte) bool {
	text := strings.TrimSpace(string(payload))
	if text == "" {
		return false
	}
	meaningful := false
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "data: [DONE]" || line == "[DONE]" {
			continue
		}
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, ":") {
			continue
		}
		if strings.HasPrefix(line, "data: ") {
			var probe map[string]json.RawMessage
			if errUnmarshal := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &probe); errUnmarshal == nil {
				// Control events with no content yet.
				if len(probe) == 1 {
					if _, onlyType := probe["type"]; onlyType {
						continue
					}
				}
			}
		}
		meaningful = true
	}
	return meaningful
}
