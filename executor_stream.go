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
	StreamID string `json:"stream_id"`
	Payload  []byte `json:"payload,omitempty"`
	Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
	StreamID string `json:"stream_id"`
	Error    string `json:"error,omitempty"`
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
	if strings.TrimSpace(streamID) == "" {
		return
	}
	_, _ = callHost(pluginabi.MethodHostStreamClose, rpcStreamCloseRequest{
		StreamID: streamID,
		Error:    strings.TrimSpace(errMsg),
	})
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
	go func() {
		defer func() {
			if recovered := recover(); recovered != nil {
				closePluginStream(streamID, fmt.Sprintf("combo stream orchestration panic: %v", recovered))
			}
		}()
		if errRun := runComboStream(req, streamID); errRun != nil {
			closePluginStream(streamID, errRun.Error())
			return
		}
		closePluginStream(streamID, "")
	}()
	return okEnvelope(map[string]any{
		"headers": http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

func runComboStream(req rpcExecutorRequest, pluginStreamID string) error {
	ordered, errPlan := planFor(req.ExecutorRequest)
	if errPlan != nil {
		return errPlan
	}
	format := protocolFormat(req.ExecutorRequest)
	body := requestBody(req.ExecutorRequest)

	var lastErr error
	for _, member := range ordered {
		emitted, errMember := streamMember(req.HostCallbackID, pluginStreamID, format, member, body)
		if errMember == nil && emitted {
			return nil
		}
		if errMember == nil {
			errMember = fmt.Errorf("%s: upstream stream produced no output", member)
		}
		lastErr = errMember
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q: no member produced a stream", req.Model)
	}
	return lastErr
}

// streamMember forwards one combo member's stream to the client. It reports
// whether any payload reached the client; when nothing did, the caller may fall
// back to the next member.
func streamMember(hostCallbackID, pluginStreamID, format, member string, body []byte) (bool, error) {
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
		return false, fmt.Errorf("%s: %w", member, errCall)
	}
	var resp pluginapi.HostModelStreamResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return false, errDecode
	}
	if resp.StatusCode >= 400 {
		_ = closeHostModelStream(resp.StreamID)
		return false, fmt.Errorf("%s: upstream status %d", member, resp.StatusCode)
	}
	if strings.TrimSpace(resp.StreamID) == "" {
		return false, fmt.Errorf("%s: host returned an empty stream id", member)
	}
	defer func() { _ = closeHostModelStream(resp.StreamID) }()

	emitted := false
	for {
		chunkRaw, errRead := callHost(pluginabi.MethodHostModelStreamRead, pluginapi.HostModelStreamReadRequest{StreamID: resp.StreamID})
		if errRead != nil {
			return emitted, fmt.Errorf("%s: %w", member, errRead)
		}
		var chunk pluginapi.HostModelStreamReadResponse
		if errDecode := json.Unmarshal(chunkRaw, &chunk); errDecode != nil {
			return emitted, errDecode
		}
		if chunk.Error != "" {
			if !emitted {
				return false, fmt.Errorf("%s: %s", member, chunk.Error)
			}
			return emitted, fmt.Errorf("%s: %s", member, chunk.Error)
		}
		if len(chunk.Payload) > 0 {
			if errEmit := emitStreamChunk(pluginStreamID, bytes.Clone(chunk.Payload)); errEmit != nil {
				return emitted, errEmit
			}
			emitted = true
		}
		if chunk.Done {
			break
		}
	}
	return emitted, nil
}
