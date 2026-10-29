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
// combo member. Request errors that describe the request itself (400, 413, 422)
// are terminal and are returned to the caller with their upstream status.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout,
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusTooManyRequests:
		return true
	default:
		return status >= 500
	}
}

// retryableError decides whether a failed member should hand over. A missing or
// unsupported member model is a configuration problem in the combo, not a bad
// client request, so it falls through even though the host reports it as 400.
func retryableError(err error, status int) bool {
	if status != 0 && retryableStatus(status) {
		return true
	}
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"model_not_found",
		"unknown provider for model",
		"model not found",
		"unsupported model",
		"no available auth",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

// errorEnvelopeStatus preserves the upstream HTTP status for the host so the
// client sees the real status instead of a blanket 500.
func errorEnvelopeStatus(code, message string, status int) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{
		Code:       code,
		Message:    message,
		HTTPStatus: status,
	}})
	return raw
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
// request. Round-robin advances exactly once here, so the plan for this
// execution is fixed before any member is tried.
func planFor(exec pluginapi.ExecutorRequest) ([]string, error) {
	cfg := loadedConfig()
	combo, ok := cfg.comboByName(exec.Model)
	if !ok {
		return nil, fmt.Errorf("combo %q is not configured", exec.Model)
	}
	format := protocolFormat(exec)
	required := detectRequiredCapabilities(format, requestBody(exec))
	ordered := orderMembers(combo, cfg, required, true)
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
	lastStatus := 0
	for _, member := range ordered {
		resp, errExecute := hostModelExecute(req.HostCallbackID, format, member, body, false)
		hostLog(req.HostCallbackID, "debug", fmt.Sprintf("combo-router: combo=%s member=%s status=%d body_len=%d err=%s", req.Model, member, resp.StatusCode, len(resp.Body), errorString(errExecute)), nil)
		if errExecute != nil {
			lastErr = fmt.Errorf("%s: %w", member, errExecute)
			lastStatus = statusOf(errExecute)
			if !retryableError(errExecute, lastStatus) {
				return errorEnvelopeStatus("executor_error", lastErr.Error(), lastStatus), nil
			}
			continue
		}
		if resp.StatusCode >= 400 {
			lastStatus = resp.StatusCode
			lastErr = fmt.Errorf("%s: upstream status %d", member, resp.StatusCode)
			if retryableStatus(resp.StatusCode) {
				continue
			}
			message := strings.TrimSpace(string(resp.Body))
			if message == "" {
				message = lastErr.Error()
			}
			return errorEnvelopeStatus("executor_error", message, resp.StatusCode), nil
		}
		if len(bytes.TrimSpace(resp.Body)) == 0 {
			// A 2xx with no payload is not a usable answer; try the next member.
			lastStatus = http.StatusBadGateway
			lastErr = fmt.Errorf("%s: empty response body", member)
			continue
		}
		return okEnvelope(pluginapi.ExecutorResponse{Payload: resp.Body, Headers: resp.Headers})
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("combo %q: no member produced a response", req.Model)
	}
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	return errorEnvelopeStatus("executor_error", lastErr.Error(), lastStatus), nil
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
