package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type hostModelExecuteRequest struct {
	pluginapi.HostModelExecutionRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type hostLogRequest struct {
	Level          string         `json:"level"`
	Message        string         `json:"message"`
	Fields         map[string]any `json:"fields,omitempty"`
	HostCallbackID string         `json:"host_callback_id,omitempty"`
}

// hostLog writes a line through the host logger, tagged with the request id of
// the execution that triggered it.
func hostLog(hostCallbackID, level, message string, fields map[string]any) {
	_, _ = callHost(pluginabi.MethodHostLog, hostLogRequest{
		Level:          level,
		Message:        message,
		Fields:         fields,
		HostCallbackID: hostCallbackID,
	})
}

// retryableStatus reports whether a failed member should hand over to the next
// combo member. Client errors that describe the request itself (400, 413, 422)
// are returned to the caller unchanged.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusRequestTimeout,
		http.StatusConflict,
		http.StatusTooEarly,
		http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	default:
		return status >= 500
	}
}

func requestBody(exec pluginapi.ExecutorRequest) []byte {
	if len(exec.OriginalRequest) > 0 {
		return exec.OriginalRequest
	}
	return exec.Payload
}

func protocolFormat(exec pluginapi.ExecutorRequest) string {
	if format := strings.TrimSpace(exec.SourceFormat); format != "" {
		return format
	}
	if format := strings.TrimSpace(exec.Format); format != "" {
		return format
	}
	return "openai"
}

func hostModelExecute(hostCallbackID, format, model string, body []byte, stream bool) (pluginapi.HostModelExecutionResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostModelExecute, hostModelExecuteRequest{
		HostModelExecutionRequest: pluginapi.HostModelExecutionRequest{
			EntryProtocol: format,
			ExitProtocol:  format,
			Model:         model,
			Stream:        stream,
			Body:          body,
		},
		HostCallbackID: hostCallbackID,
	})
	if errCall != nil {
		return pluginapi.HostModelExecutionResponse{}, errCall
	}
	var resp pluginapi.HostModelExecutionResponse
	if errDecode := json.Unmarshal(raw, &resp); errDecode != nil {
		return pluginapi.HostModelExecutionResponse{}, errDecode
	}
	return resp, nil
}

// planFor returns the ordered member list for the combo carried by an execution
// request. Rotation position is only advanced by the router, so the executor
// uses the same position the route decision used.
func planFor(exec pluginapi.ExecutorRequest) ([]string, error) {
	cfg := loadedConfig()
	combo, ok := cfg.comboByName(exec.Model)
	if !ok {
		return nil, fmt.Errorf("combo %q is not configured", exec.Model)
	}
	format := protocolFormat(exec)
	required := detectRequiredCapabilities(format, requestBody(exec))
	ordered := orderMembers(combo, cfg, required, false)
	if len(ordered) == 0 {
		return nil, fmt.Errorf("combo %q has no usable members", exec.Model)
	}
	return ordered, nil
}

func executeCombo(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	ordered, errPlan := planFor(req.ExecutorRequest)
	if errPlan != nil {
		return errorEnvelope("executor_error", errPlan.Error()), nil
	}
	format := protocolFormat(req.ExecutorRequest)
	body := requestBody(req.ExecutorRequest)

	var lastErr error
	for _, member := range ordered {
		resp, errExecute := hostModelExecute(req.HostCallbackID, format, member, body, false)
		hostLog(req.HostCallbackID, "debug", fmt.Sprintf("combo-router: combo=%s member=%s status=%d body_len=%d err=%s", req.Model, member, resp.StatusCode, len(resp.Body), errorString(errExecute)), nil)
		if errExecute != nil {
			lastErr = fmt.Errorf("%s: %w", member, errExecute)
			continue
		}
		if resp.StatusCode >= 400 {
			lastErr = fmt.Errorf("%s: upstream status %d", member, resp.StatusCode)
			if retryableStatus(resp.StatusCode) {
				continue
			}
			// The request itself is bad; surface the upstream answer as-is.
			return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
		}
		return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q: no member produced a response", req.Model)
	}
	return errorEnvelope("executor_error", lastErr.Error()), nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
