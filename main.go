// codex-usage-guard is a CLIProxyAPI usage plugin that sheds load per Codex
// account based on ChatGPT's rate-limit headers.
//
// CLIProxyAPI itself never parses the upstream usage headers, but it does hand
// the full upstream response header set to registered usage plugins. This
// plugin reads:
//
//	x-codex-primary-used-percent     (rolling 5h window, %)
//	x-codex-secondary-used-percent   (rolling weekly window, %)
//
// When usage on either window crosses a configured threshold the plugin marks
// that account `disabled` in its auth file (the same flag the management UI
// toggles), so the router stops sending it new requests. Because an operator
// `disabled` account never receives traffic again, it would never re-enable on
// its own — so a background goroutine re-enables accounts this plugin disabled
// once their window has had time to roll off. The plugin only ever touches
// accounts it disabled itself (identified by a marker it writes), never ones a
// human or another tool disabled.
//
// The plugin is intentionally self-contained: it speaks the CLIProxyAPI plugin
// C-ABI (version 1) and JSON contract directly, so it does not import the large
// CLIProxyAPI Go module and can be cross-compiled to .so/.dylib/.dll with only
// gopkg.in/yaml.v3 as a dependency.
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
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"gopkg.in/yaml.v3"
)

const (
	pluginID      = "codex-usage-guard"
	pluginVersion = "0.1.1"
	pluginRepo    = "https://github.com/huangdaoxu/codex-usage-guard"
	abiVersion    = 1
	schemaVersion = 1

	// Plugin lifecycle + usage methods (host -> plugin).
	methodPluginRegister    = "plugin.register"
	methodPluginReconfigure = "plugin.reconfigure"
	methodPluginShutdown    = "plugin.shutdown"
	methodUsageHandle       = "usage.handle"

	// Host callbacks (plugin -> host).
	methodHostAuthList = "host.auth.list"
	methodHostAuthGet  = "host.auth.get"
	methodHostAuthSave = "host.auth.save"
	methodHostLog      = "host.log"

	// Codex rate-limit response headers (case-insensitive lookup).
	headerPrimaryPercent   = "x-codex-primary-used-percent"
	headerSecondaryPercent = "x-codex-secondary-used-percent"

	// Markers written into the auth file so the resume loop only ever touches
	// accounts THIS plugin disabled.
	markerDisabledBy = "usage_guard_disabled_by"
	markerResumeAt   = "usage_guard_resume_at" // unix seconds
	markerReason     = "usage_guard_reason"
)

// ---------------------------------------------------------------------------
// Configuration (read from plugins.configs.codex-usage-guard at register time)
// ---------------------------------------------------------------------------

type guardConfig struct {
	// PrimaryPercent is the 5h-window threshold (%). 0 disables the 5h check.
	PrimaryPercent float64 `yaml:"primary_percent"`
	// SecondaryPercent is the weekly-window threshold (%). 0 disables it.
	SecondaryPercent float64 `yaml:"secondary_percent"`
	// PrimaryResumeMinutes is how long to keep an account disabled after a 5h
	// trip before the resume loop re-enables it.
	PrimaryResumeMinutes int `yaml:"primary_resume_minutes"`
	// SecondaryResumeMinutes is the same for a weekly trip.
	SecondaryResumeMinutes int `yaml:"secondary_resume_minutes"`
	// MinActive keeps at least this many accounts enabled on the node; the
	// plugin will not disable an account if doing so drops the active count to
	// or below this floor. 0 means no floor.
	MinActive int `yaml:"min_active"`
	// CooldownSeconds debounces repeated action on the same account.
	CooldownSeconds int `yaml:"cooldown_seconds"`
	// ScanIntervalSeconds is how often the resume loop runs.
	ScanIntervalSeconds int `yaml:"scan_interval_seconds"`
	// DryRun logs what it would do without changing any auth file.
	DryRun bool `yaml:"dry_run"`
}

func defaultConfig() guardConfig {
	return guardConfig{
		PrimaryPercent:         90,
		SecondaryPercent:       90,
		PrimaryResumeMinutes:   300,   // ~5h window
		SecondaryResumeMinutes: 10080, // 7d window
		MinActive:              0,
		CooldownSeconds:        120,
		ScanIntervalSeconds:    60,
		DryRun:                 false,
	}
}

func (c *guardConfig) sanitize() {
	if c.PrimaryResumeMinutes <= 0 {
		c.PrimaryResumeMinutes = 300
	}
	if c.SecondaryResumeMinutes <= 0 {
		c.SecondaryResumeMinutes = 10080
	}
	if c.CooldownSeconds < 0 {
		c.CooldownSeconds = 0
	}
	if c.ScanIntervalSeconds <= 0 {
		c.ScanIntervalSeconds = 60
	}
	if c.MinActive < 0 {
		c.MinActive = 0
	}
}

var (
	cfgMu      sync.RWMutex
	cfg        = defaultConfig()
	lastAction = map[string]time.Time{}
	actionMu   sync.Mutex
	resumeOnce sync.Once
)

func currentConfig() guardConfig {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg
}

// ---------------------------------------------------------------------------
// JSON contracts (mirrors of the host structs we exchange)
// ---------------------------------------------------------------------------

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type lifecycleRequest struct {
	ConfigYAML    []byte `json:"config_yaml"`
	SchemaVersion uint32 `json:"schema_version"`
}

// usageRecord mirrors pluginapi.UsageRecord. The host struct carries no JSON
// tags, so keys are the exact Go field names.
type usageRecord struct {
	Provider        string              `json:"Provider"`
	Model           string              `json:"Model"`
	AuthID          string              `json:"AuthID"`
	AuthIndex       string              `json:"AuthIndex"`
	AuthType        string              `json:"AuthType"`
	Failed          bool                `json:"Failed"`
	ResponseHeaders map[string][]string `json:"ResponseHeaders"`
}

type hostAuthGetRequest struct {
	AuthIndex string `json:"auth_index"`
}

type hostAuthGetResponse struct {
	AuthIndex string          `json:"auth_index"`
	Name      string          `json:"name"`
	Path      string          `json:"path"`
	JSON      json.RawMessage `json:"json"`
}

type hostAuthSaveRequest struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

type hostAuthListResponse struct {
	Files []hostAuthFileEntry `json:"files"`
}

type hostAuthFileEntry struct {
	ID        string `json:"id,omitempty"`
	AuthIndex string `json:"auth_index,omitempty"`
	Name      string `json:"name"`
	Provider  string `json:"provider,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	Email     string `json:"email,omitempty"`
}

type hostLogRequest struct {
	Level   string         `json:"level,omitempty"`
	Message string         `json:"message,omitempty"`
	Fields  map[string]any `json:"fields,omitempty"`
}

// ---------------------------------------------------------------------------
// C ABI exports
// ---------------------------------------------------------------------------

func main() {}

//export cliproxy_plugin_init
func cliproxy_plugin_init(host *C.cliproxy_host_api, plugin *C.cliproxy_plugin_api) C.int {
	if plugin == nil {
		return 1
	}
	C.store_host_api(host)
	plugin.abi_version = C.uint32_t(abiVersion)
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
	raw, err := handleMethod(C.GoString(method), requestBytes)
	if err != nil {
		writeResponse(response, errorEnvelope("plugin_error", err.Error()))
		return 1
	}
	writeResponse(response, raw)
	return 0
}

//export cliproxyPluginFree
func cliproxyPluginFree(ptr unsafe.Pointer, length C.size_t) {
	if ptr != nil {
		C.free(ptr)
	}
	_ = length
}

//export cliproxyPluginShutdown
func cliproxyPluginShutdown() {}

// ---------------------------------------------------------------------------
// Method dispatch
// ---------------------------------------------------------------------------

func handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case methodPluginRegister, methodPluginReconfigure:
		applyLifecycleConfig(request)
		ensureResumeLoop()
		return okEnvelopeJSON(registrationJSON())
	case methodPluginShutdown:
		return okEnvelopeJSON("{}")
	case methodUsageHandle:
		handleUsage(request)
		return okEnvelopeJSON("{}")
	default:
		return errorEnvelope("unknown_method", "unknown method: "+method), nil
	}
}

func registrationJSON() string {
	return `{"schema_version":` + strconv.Itoa(schemaVersion) +
		`,"metadata":{"Name":"` + pluginID +
		`","Version":"` + pluginVersion +
		`","Author":"galaxy-router","GitHubRepository":"` + pluginRepo +
		`","Logo":"","ConfigFields":[]}` +
		`,"capabilities":{"usage_plugin":true}}`
}

func applyLifecycleConfig(request []byte) {
	newCfg := defaultConfig()
	var req lifecycleRequest
	if len(request) > 0 {
		if err := json.Unmarshal(request, &req); err == nil && len(req.ConfigYAML) > 0 {
			_ = yaml.Unmarshal(req.ConfigYAML, &newCfg)
		}
	}
	newCfg.sanitize()
	cfgMu.Lock()
	cfg = newCfg
	cfgMu.Unlock()
	hostLog("info", "codex-usage-guard configured", map[string]any{
		"primary_percent":   newCfg.PrimaryPercent,
		"secondary_percent": newCfg.SecondaryPercent,
		"min_active":        newCfg.MinActive,
		"dry_run":           newCfg.DryRun,
	})
}

// ---------------------------------------------------------------------------
// usage.handle — the hot path
// ---------------------------------------------------------------------------

func handleUsage(request []byte) {
	var rec usageRecord
	if err := json.Unmarshal(request, &rec); err != nil {
		return
	}
	if len(rec.ResponseHeaders) == 0 {
		return
	}
	c := currentConfig()

	primary := parsePercent(headerGet(rec.ResponseHeaders, headerPrimaryPercent))
	secondary := parsePercent(headerGet(rec.ResponseHeaders, headerSecondaryPercent))

	// Pick the window that tripped; if both trip prefer the weekly one because
	// it needs the longer recovery time.
	tripped := false
	reason := ""
	resumeMinutes := 0
	usedPct := 0.0
	if c.PrimaryPercent > 0 && primary >= c.PrimaryPercent {
		tripped = true
		reason = "5h"
		resumeMinutes = c.PrimaryResumeMinutes
		usedPct = primary
	}
	if c.SecondaryPercent > 0 && secondary >= c.SecondaryPercent {
		tripped = true
		reason = "weekly"
		resumeMinutes = c.SecondaryResumeMinutes
		usedPct = secondary
	}
	if !tripped {
		return
	}

	key := accountKey(rec.AuthID, rec.AuthIndex)
	if onCooldown(key, c.CooldownSeconds) {
		return
	}

	disableAccount(rec, c, reason, usedPct, resumeMinutes)
}

func disableAccount(rec usageRecord, c guardConfig, reason string, usedPct float64, resumeMinutes int) {
	key := accountKey(rec.AuthID, rec.AuthIndex)

	entry, name, raw, err := resolveAuth(rec.AuthIndex, rec.AuthID)
	if err != nil {
		hostLog("error", "codex-usage-guard: cannot resolve auth to disable", map[string]any{
			"auth_id": rec.AuthID, "error": err.Error(),
		})
		return
	}
	// Already disabled (by anyone) — nothing to do, but debounce.
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		hostLog("error", "codex-usage-guard: auth json parse failed", map[string]any{"name": name, "error": err.Error()})
		return
	}
	if b, _ := doc["disabled"].(bool); b {
		markAction(key)
		return
	}

	if c.MinActive > 0 {
		active, err := activeAccountCount()
		if err == nil && active-1 < c.MinActive {
			hostLog("warn", "codex-usage-guard: skipping disable to keep min_active", map[string]any{
				"name": name, "active": active, "min_active": c.MinActive, "reason": reason, "used_pct": usedPct,
			})
			markAction(key)
			return
		}
	}

	resumeAt := time.Now().Add(time.Duration(resumeMinutes) * time.Minute).Unix()

	if c.DryRun {
		hostLog("warn", "codex-usage-guard: DRY RUN would disable account", map[string]any{
			"name": name, "email": entry.Email, "reason": reason, "used_pct": usedPct,
			"resume_at": resumeAt,
		})
		markAction(key)
		return
	}

	doc["disabled"] = true
	doc[markerDisabledBy] = pluginID
	doc[markerResumeAt] = resumeAt
	doc[markerReason] = fmt.Sprintf("%s window at %.1f%%", reason, usedPct)

	patched, err := json.Marshal(doc)
	if err != nil {
		hostLog("error", "codex-usage-guard: marshal patched auth failed", map[string]any{"name": name, "error": err.Error()})
		return
	}
	if err := saveAuth(name, patched); err != nil {
		hostLog("error", "codex-usage-guard: save disabled auth failed", map[string]any{"name": name, "error": err.Error()})
		return
	}
	markAction(key)
	hostLog("warn", "codex-usage-guard: disabled account over threshold", map[string]any{
		"name": name, "email": entry.Email, "reason": reason, "used_pct": usedPct,
		"resume_at": time.Unix(resumeAt, 0).Format(time.RFC3339),
	})
}

// ---------------------------------------------------------------------------
// Resume loop — re-enable accounts THIS plugin disabled once their window rolls
// ---------------------------------------------------------------------------

func ensureResumeLoop() {
	resumeOnce.Do(func() {
		go resumeLoop()
	})
}

func resumeLoop() {
	defer func() { _ = recover() }()
	for {
		interval := currentConfig().ScanIntervalSeconds
		if interval <= 0 {
			interval = 60
		}
		time.Sleep(time.Duration(interval) * time.Second)
		func() {
			defer func() { _ = recover() }()
			resumeReadyAccounts()
		}()
	}
}

func resumeReadyAccounts() {
	files, err := listAuth()
	if err != nil {
		return
	}
	now := time.Now().Unix()
	for _, f := range files {
		if !f.Disabled {
			continue
		}
		idx := f.AuthIndex
		if idx == "" {
			idx = f.ID
		}
		_, name, raw, err := resolveAuth(idx, f.ID)
		if err != nil {
			continue
		}
		var doc map[string]any
		if err := json.Unmarshal(raw, &doc); err != nil {
			continue
		}
		if by, _ := doc[markerDisabledBy].(string); by != pluginID {
			continue // not ours — never touch it
		}
		resumeAt := toInt64(doc[markerResumeAt])
		if resumeAt == 0 || now < resumeAt {
			continue
		}
		doc["disabled"] = false
		delete(doc, markerDisabledBy)
		delete(doc, markerResumeAt)
		delete(doc, markerReason)
		patched, err := json.Marshal(doc)
		if err != nil {
			continue
		}
		if err := saveAuth(name, patched); err != nil {
			hostLog("error", "codex-usage-guard: re-enable save failed", map[string]any{"name": name, "error": err.Error()})
			continue
		}
		hostLog("info", "codex-usage-guard: re-enabled account after window reset", map[string]any{
			"name": name, "email": f.Email,
		})
	}
}

// ---------------------------------------------------------------------------
// Host callback helpers
// ---------------------------------------------------------------------------

// resolveAuth returns the auth entry, file name, and raw JSON for an account,
// preferring the auth index and falling back to a list lookup by id.
func resolveAuth(authIndex, authID string) (hostAuthFileEntry, string, json.RawMessage, error) {
	if authIndex != "" {
		if resp, err := getAuth(authIndex); err == nil && resp.Name != "" {
			return hostAuthFileEntry{AuthIndex: resp.AuthIndex, Name: resp.Name}, resp.Name, resp.JSON, nil
		}
	}
	files, err := listAuth()
	if err != nil {
		return hostAuthFileEntry{}, "", nil, err
	}
	for _, f := range files {
		if (authID != "" && f.ID == authID) || (authIndex != "" && f.AuthIndex == authIndex) {
			idx := f.AuthIndex
			if idx == "" {
				idx = f.ID
			}
			resp, err := getAuth(idx)
			if err != nil {
				return hostAuthFileEntry{}, "", nil, err
			}
			name := resp.Name
			if name == "" {
				name = f.Name
			}
			return f, name, resp.JSON, nil
		}
	}
	return hostAuthFileEntry{}, "", nil, fmt.Errorf("auth not found (id=%q index=%q)", authID, authIndex)
}

func getAuth(authIndex string) (hostAuthGetResponse, error) {
	raw, err := callHost(methodHostAuthGet, hostAuthGetRequest{AuthIndex: authIndex})
	if err != nil {
		return hostAuthGetResponse{}, err
	}
	var resp hostAuthGetResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return hostAuthGetResponse{}, err
	}
	return resp, nil
}

func listAuth() ([]hostAuthFileEntry, error) {
	raw, err := callHost(methodHostAuthList, map[string]any{})
	if err != nil {
		return nil, err
	}
	var resp hostAuthListResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, err
	}
	return resp.Files, nil
}

func saveAuth(name string, doc json.RawMessage) error {
	if !strings.HasSuffix(strings.ToLower(name), ".json") {
		name += ".json"
	}
	_, err := callHost(methodHostAuthSave, hostAuthSaveRequest{Name: name, JSON: doc})
	return err
}

func activeAccountCount() (int, error) {
	files, err := listAuth()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, f := range files {
		if !f.Disabled {
			n++
		}
	}
	return n, nil
}

func hostLog(level, message string, fields map[string]any) {
	_, _ = callHost(methodHostLog, hostLogRequest{Level: level, Message: message, Fields: fields})
}

// callHost performs a plugin -> host RPC: it marshals payload as the raw request
// body and decodes the host's {ok,result,error} envelope.
func callHost(method string, payload any) (json.RawMessage, error) {
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal %s payload: %w", method, err)
	}
	cMethod := C.CString(method)
	defer C.free(unsafe.Pointer(cMethod))

	var response C.cliproxy_buffer
	var requestPtr *C.uint8_t
	if len(rawPayload) > 0 {
		cPayload := C.CBytes(rawPayload)
		defer C.free(cPayload)
		requestPtr = (*C.uint8_t)(cPayload)
	}
	code := C.call_host_api(cMethod, requestPtr, C.size_t(len(rawPayload)), &response)

	var rawResponse []byte
	if response.ptr != nil && response.len > 0 {
		rawResponse = C.GoBytes(response.ptr, C.int(response.len))
	}
	if response.ptr != nil {
		C.free_host_buffer(response.ptr, response.len)
	}
	if len(rawResponse) == 0 {
		return nil, fmt.Errorf("host callback %s returned no response (code=%d)", method, int(code))
	}
	var env envelope
	if err := json.Unmarshal(rawResponse, &env); err != nil {
		return nil, fmt.Errorf("decode %s envelope: %w", method, err)
	}
	if !env.OK {
		if env.Error != nil {
			return nil, fmt.Errorf("%s: %s", env.Error.Code, env.Error.Message)
		}
		return nil, fmt.Errorf("host callback %s failed", method)
	}
	return append(json.RawMessage(nil), env.Result...), nil
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

func accountKey(authID, authIndex string) string {
	if authID != "" {
		return authID
	}
	return authIndex
}

func onCooldown(key string, cooldownSeconds int) bool {
	if cooldownSeconds <= 0 || key == "" {
		return false
	}
	actionMu.Lock()
	defer actionMu.Unlock()
	last, ok := lastAction[key]
	if !ok {
		return false
	}
	return time.Since(last) < time.Duration(cooldownSeconds)*time.Second
}

func markAction(key string) {
	if key == "" {
		return
	}
	actionMu.Lock()
	lastAction[key] = time.Now()
	actionMu.Unlock()
}

func headerGet(h map[string][]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func parsePercent(s string) float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	if s == "" {
		return -1
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return f
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, _ := n.Int64()
		return i
	case string:
		i, _ := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		return i
	default:
		return 0
	}
}

func okEnvelopeJSON(result string) ([]byte, error) {
	return json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
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
