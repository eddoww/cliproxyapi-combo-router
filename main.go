// Package main implements the CLIProxyAPI combo-router plugin.
//
// The plugin adds two things the upstream proxy does not have:
//
//   - Combos: a client-visible model name that expands to an ordered list of
//     real models, with cross-model fallback inside a single request.
//   - Capability-aware ordering: when the current user turn carries images,
//     audio, video or PDFs, combo members that advertise those capabilities are
//     tried first. Members are never dropped, so fallback stays intact.
//
// It is a standard dynamic library plugin (C ABI, JSON-RPC) built with
// -buildmode=c-shared, so it does not depend on the Go toolchain version used
// to build the proxy.
package main

/*
#include <stdint.h>
#include <stdlib.h>

typedef struct {
	void* ptr;
	size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(void*, const char*, const uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_host_free_fn)(void*, size_t);

typedef struct {
	uint32_t abi_version;
	void* host_ctx;
	cliproxy_host_call_fn call;
	cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(char*, uint8_t*, size_t, cliproxy_buffer*);
typedef void (*cliproxy_plugin_free_fn)(void*, size_t);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
	uint32_t abi_version;
	cliproxy_plugin_call_fn call;
	cliproxy_plugin_free_fn free_buffer;
	cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

extern int cliproxyPluginCall(char*, uint8_t*, size_t, cliproxy_buffer*);
extern void cliproxyPluginFree(void*, size_t);
extern void cliproxyPluginShutdown(void);

static const cliproxy_host_api* stored_host;

static void store_host_api(const cliproxy_host_api* host) {
	stored_host = host;
}

static int call_host_api(const char* method, const uint8_t* request, size_t request_len, cliproxy_buffer* response) {
	if (stored_host == NULL || stored_host->call == NULL) {
		return 1;
	}
	return stored_host->call(stored_host->host_ctx, method, request, request_len, response);
}

static void free_host_buffer(void* ptr, size_t len) {
	if (stored_host != NULL && stored_host->free_buffer != NULL && ptr != NULL) {
		stored_host->free_buffer(ptr, len);
	}
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"
	"unsafe"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// pluginIdentifier must match the plugin id the host derives from the file name
// (combo-router.so -> combo-router). Model routers return target=self, which the
// host resolves back to this id.
const pluginIdentifier = "combo-router"

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code              string `json:"code"`
	Message           string `json:"message"`
	HTTPStatus        int    `json:"http_status,omitempty"`
	RetryAfterSeconds int    `json:"retry_after_seconds,omitempty"`
}

type lifecycleRequest struct {
	ConfigYAML []byte `json:"config_yaml"`
}

type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type rpcModelRouteRequest struct {
	pluginapi.ModelRouteRequest
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

type registration struct {
	SchemaVersion uint32                 `json:"schema_version"`
	Metadata      pluginapi.Metadata     `json:"metadata"`
	Capabilities  registrationCapability `json:"capabilities"`
}

type registrationCapability struct {
	ModelRouter           bool     `json:"model_router"`
	Executor              bool     `json:"executor"`
	ExecutorModelScope    string   `json:"executor_model_scope"`
	ExecutorInputFormats  []string `json:"executor_input_formats"`
	ExecutorOutputFormats []string `json:"executor_output_formats"`
}

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(pluginabi.ABIVersion)
	plugin.call = C.cliproxy_plugin_call_fn(C.cliproxyPluginCall)
	plugin.free_buffer = C.cliproxy_plugin_free_fn(C.cliproxyPluginFree)
	plugin.shutdown = C.cliproxy_plugin_shutdown_fn(C.cliproxyPluginShutdown)
	return 0
}

//export cliproxyPluginCall
func cliproxyPluginCall(method *C.char, request *C.uint8_t, requestLen C.size_t, response *C.cliproxy_buffer) C.int {
	if response != nil {
		response.ptr = nil
		response.len = 0
	}
	if method == nil {
		writeResponse(response, errorEnvelope("invalid_method", "method is required"))
		return 1
	}
	var requestBytes []byte
	if request != nil && requestLen > 0 {
		requestBytes = C.GoBytes(unsafe.Pointer(request), C.int(requestLen))
	}
	raw, errHandle := safeHandleMethod(C.GoString(method), requestBytes)
	if errHandle != nil {
		writeResponse(response, errorEnvelope("plugin_error", errHandle.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, _ C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// safeHandleMethod keeps a panic inside one plugin call from unwinding into the
// host process that loaded this shared library.
func safeHandleMethod(method string, request []byte) (out []byte, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			out = errorEnvelope("plugin_panic", fmt.Sprintf("combo-router panic in %s: %v", method, recovered))
			err = nil
		}
	}()
	return handleMethod(method, request)
}

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		if errConfigure := configure(request); errConfigure != nil {
			return nil, errConfigure
		}
		return okEnvelope(pluginRegistration())
	case pluginabi.MethodPluginQuiesce:
		waitForWorkers(5 * time.Second)
		return okEnvelope(map[string]any{})
	case pluginabi.MethodPluginShutdown:
		waitForWorkers(2 * time.Second)
		return okEnvelope(map[string]any{})
	case pluginabi.MethodModelRoute:
		return routeCombo(request)
	case pluginabi.MethodExecutorIdentifier:
		return okEnvelope(map[string]string{"identifier": pluginIdentifier})
	case pluginabi.MethodExecutorExecute:
		return executeCombo(request)
	case pluginabi.MethodExecutorExecuteStream:
		return executeComboStream(request)
	case pluginabi.MethodExecutorCountTokens:
		// The combo executor does not own token counting; a zero estimate keeps
		// Claude count_tokens calls working instead of failing the request.
		return okEnvelope(pluginapi.ExecutorResponse{Payload: []byte(`{"input_tokens":0}`)})
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginIdentifier,
			Version:          pluginVersion,
			Author:           "eddoww",
			GitHubRepository: "https://github.com/eddoww/cliproxyapi-combo-router",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "enabled", Type: pluginapi.ConfigFieldTypeBoolean, Description: "Enable combo routing."},
				{Name: "default_strategy", Type: pluginapi.ConfigFieldTypeEnum, EnumValues: []string{strategyFallback, strategyRoundRobin}, Description: "Default member ordering strategy for combos without their own strategy."},
				{Name: "combos", Type: pluginapi.ConfigFieldTypeArray, Description: "Named combos: {name, models: [...], strategy?}."},
				{Name: "capabilities", Type: pluginapi.ConfigFieldTypeObject, Description: "Per-model capability overrides: {model: [vision, audio, video, pdf, tools]}."},
			},
		},
		Capabilities: registrationCapability{
			ModelRouter:           true,
			Executor:              true,
			ExecutorModelScope:    string(pluginapi.ExecutorModelScopeStatic),
			ExecutorInputFormats:  executorFormats(),
			ExecutorOutputFormats: executorFormats(),
		},
	}
}

func executorFormats() []string {
	return []string{"openai", "openai-response", "claude", "gemini", "codex"}
}

func okEnvelope(result any) ([]byte, error) {
	raw, errMarshal := json.Marshal(result)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}

func writeResponse(response *C.cliproxy_buffer, raw []byte) {
	if response == nil || len(raw) == 0 {
		return
	}
	ptr := C.CBytes(raw)
	if ptr == nil {
		return
	}
	response.ptr = ptr
	response.len = C.size_t(len(raw))
}

// hostError carries the upstream HTTP status the host attached to a failed
// callback, so routing policy can distinguish a 400 from a 503.
type hostError struct {
	code       string
	message    string
	status     int
	retryAfter int
}

func (e *hostError) Error() string {
	if e == nil {
		return ""
	}
	if e.code != "" {
		return e.code + ": " + e.message
	}
	return e.message
}

// statusOf extracts an upstream status from an error chain, or 0 when unknown.
func statusOf(err error) int {
	var typed *hostError
	if errors.As(err, &typed) && typed != nil {
		return typed.status
	}
	return 0
}

var (
	workersMu sync.Mutex
	workers   sync.WaitGroup
)

func trackWorker(delta int) {
	workersMu.Lock()
	defer workersMu.Unlock()
	if delta > 0 {
		workers.Add(delta)
	} else {
		workers.Done()
	}
}

func waitForWorkers(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		workers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
	}
}

func callHost(method string, payload any) (json.RawMessage, error) {
	var request []byte
	if payload != nil {
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, fmt.Errorf("marshal host callback %s: %w", method, errMarshal)
		}
		request = raw
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))
	var response C.cliproxy_buffer
	var reqPtr *C.uint8_t
	if len(request) > 0 {
		reqPtr = (*C.uint8_t)(C.CBytes(request))
		defer C.free(unsafe.Pointer(reqPtr))
	}
	if C.call_host_api(cMethod, reqPtr, C.size_t(len(request)), &response) != 0 {
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	if response.ptr == nil || response.len == 0 {
		return nil, fmt.Errorf("host callback %s returned no response", method)
	}
	raw := C.GoBytes(response.ptr, C.int(response.len))
	C.free_host_buffer(response.ptr, response.len)

	var env envelope
	if errUnmarshal := json.Unmarshal(raw, &env); errUnmarshal != nil {
		return nil, fmt.Errorf("decode host envelope %s: %w", method, errUnmarshal)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, &hostError{
				code:       env.Error.Code,
				message:    env.Error.Message,
				status:     env.Error.HTTPStatus,
				retryAfter: env.Error.RetryAfterSeconds,
			}
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}
