// llm chat — the tool-calling orchestration loop, running inside
// farsight-server itself rather than a Grafana plugin backend (see
// llmtools.go's package doc and the memory design doc's "architecture
// correction" for why). POST /llm/chat lives on the normal
// Tailscale-facing mux, identity resolved via the same identify()/
// tailscale whois every other route uses — the Grafana plugin's chat
// panel calls this directly from the browser (cross-origin, hence CORS
// below), no plugin backend involved at all.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/farsight/farsight/internal/store"
	openai "github.com/sashabaranov/go-openai"
)

// llmMaxToolIterations caps the tool-call loop per chat turn — a model
// stuck calling tools without ever producing a final answer shouldn't be
// able to run (and rack up LLM cost) forever. Raised twice, both times
// after watching a real open-ended analysis question against zeus's
// Ollama exhaust the cap mid-investigation: 5 wasn't enough to scan more
// than one or two candidate metrics; even 10 wasn't enough once —
// zeus's large model spent several calls re-orienting (repeat
// list_devices/get_device_details/search_records) before finally
// landing on the two calls (get_telemetry_series then
// get_telemetry_summary) that actually found the answer, running out of
// budget right after. 15 gives real headroom for that
// re-orientation-then-narrow pattern without being effectively
// unbounded.
const llmMaxToolIterations = 15

const defaultSystemPrompt = `You are the Farsight assistant, integrated into the remote monitoring platform for industrial/medical IoT devices.
You have read-only access to the data of the tenant the current user belongs to — devices, status, historical telemetry, treatments/records — never data outside that tenant (the boundary is enforced by the tools themselves, not by this instruction).
Answer in a technical, direct style. When you report numbers or events, always cite the source (device, time range, tool used) so the user can verify it.
If you don't have enough data to answer confidently, say so explicitly and ask to narrow the question, instead of guessing.
You cannot modify, delete, or reassign anything — only interpret data and, if asked, generate a downloadable report.`

// Settings-table keys for the LLM config an admin edits on /llm-settings
// (see internal/store's generic Get/SetSetting) — no restart needed to
// pick up a change, read live on every /llm/chat call. server.conf's
// three LLM_* keys only seed these once, on the very first boot (see
// main.go's SetSettingIfAbsent calls).
//
// Talks to the OpenAI-compatible chat completions endpoint (Ollama,
// OpenRouter, ...) DIRECTLY via go-openai's own client — no
// grafana-llm-app involved. Earlier design routed through that plugin
// (its settings page has its own Ollama/OpenRouter URL + API key +
// model-mapping fields), but that split the configuration across two
// separate places (Grafana's plugin settings vs this page), and its
// abstraction only exposes two fixed model slots ("base"/"large") —
// massimo needs to pick an arbitrary model string freely (e.g. comparing
// several differently-priced OpenRouter models), not toggle between two
// presets. Provider URL/key/model now all live here, one place, a real
// string field for the model. grafana-llm-app is no longer installed by
// postinst or required for this feature at all.
const (
	settingLLMProviderURL    = "llm_provider_url"
	settingLLMProviderAPIKey = "llm_provider_api_key"
	settingLLMModel          = "llm_model"
	settingLLMSystemPrompt   = "llm_system_prompt"
)

// llmChatDeps bundles what the chat handler needs. Provider URL/API
// key/model/system prompt are deliberately NOT here: they're read fresh
// from the store on every call (see loadLLMConfig) so an admin's edit on
// /llm-settings takes effect immediately, no restart.
type llmChatDeps struct {
	tools llmToolsDeps
	st    *store.Store
}

// llmConfig is what loadLLMConfig resolves for one /llm/chat call.
type llmConfig struct {
	client       *openai.Client
	model        string
	systemPrompt string
}

// loadLLMConfig reads the current LLM settings from the store — cheap
// (a few indexed point lookups), fine to do on every chat request rather
// than caching, given chat is not a hot path the way telemetry ingestion
// is. client is nil if provider URL or model aren't configured yet
// (feature off) — API key can legitimately be empty (Ollama needs none).
func loadLLMConfig(st *store.Store) (llmConfig, error) {
	providerURL, err := st.GetSettingOr(settingLLMProviderURL, "")
	if err != nil {
		return llmConfig{}, err
	}
	apiKey, err := st.GetSettingOr(settingLLMProviderAPIKey, "")
	if err != nil {
		return llmConfig{}, err
	}
	model, err := st.GetSettingOr(settingLLMModel, "")
	if err != nil {
		return llmConfig{}, err
	}
	systemPrompt, err := st.GetSettingOr(settingLLMSystemPrompt, defaultSystemPrompt)
	if err != nil {
		return llmConfig{}, err
	}
	if strings.TrimSpace(systemPrompt) == "" {
		// Blank is a real, deliberately saved value (an admin clearing the
		// field on /llm-settings to go back to the default), not just "key
		// absent" — GetSettingOr's fallback only covers the latter. An
		// empty Content also breaks the request itself: go-openai's
		// ChatCompletionMessage.MarshalJSON uses omitempty on Content, so
		// an empty string system message serializes as {"role":"system"}
		// with no "content" key at all, which Ollama's OpenAI-compatible
		// endpoint rejects with "invalid message content type: <nil>".
		systemPrompt = defaultSystemPrompt
	}

	var client *openai.Client
	if providerURL != "" && model != "" {
		cfg := openai.DefaultConfig(apiKey) // apiKey may be "" — fine, Ollama ignores the Authorization header entirely.
		cfg.BaseURL = providerURL
		client = openai.NewClientWithConfig(cfg)
	}
	return llmConfig{client: client, model: model, systemPrompt: systemPrompt}, nil
}

type chatRequest struct {
	TenantID       string `json:"tenant_id"`
	ConversationID int64  `json:"conversation_id"`
	Message        string `json:"message"`
}

type chatResponse struct {
	ConversationID int64  `json:"conversation_id"`
	Reply          string `json:"reply"`
}

// corsForPlugin sets CORS headers so the Grafana plugin's chat panel can
// call these endpoints directly from the browser — a real cross-origin
// request, different port than Grafana itself. No cookies/credentials
// involved (identity comes from tailscale whois on the TCP connection,
// not a session), so reflecting the caller's own Origin is safe: this
// isn't granting any browser access it wouldn't already have by reaching
// the Tailscale IP directly with a plain fetch. Returns true if this was
// an OPTIONS preflight the caller should stop and respond to immediately.
func corsForPlugin(w http.ResponseWriter, r *http.Request, methods string) (isPreflight bool) {
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Access-Control-Allow-Methods", methods+", OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// llmMyTenantsHandler serves GET /llm/my-tenants — the tenants the chat
// panel should let the caller pick from (all of them if admin, otherwise
// just the ones they're a member of). Used once when the panel loads, so
// it doesn't have to guess/hardcode a tenant_id for /llm/chat.
func llmMyTenantsHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if corsForPlugin(w, r, "GET") {
			return
		}
		id := identify(r.Context(), r, st)
		if id.Login == "" {
			http.Error(w, "could not resolve identity", http.StatusForbidden)
			return
		}

		var tenantIDs []string
		if id.IsAdmin {
			all, err := st.ListTenants()
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			for _, t := range all {
				tenantIDs = append(tenantIDs, t.TenantID)
			}
		} else {
			var err error
			tenantIDs, err = st.TenantsForLogin(id.Login)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"tenants": tenantIDs, "is_admin": id.IsAdmin})
	}
}

// conversationSummary is one row of GET /llm/conversations.
type conversationSummary struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	UpdatedAt string `json:"updated_at"`
}

// llmConversationsHandler serves GET /llm/conversations?tenant_id=X — the
// caller's own past conversations in that tenant (never another login's,
// see store.ListConversations: private per-login by design), most
// recently active first. Used by the chat panel to show history instead
// of starting blank every time it's opened.
func llmConversationsHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if corsForPlugin(w, r, "GET") {
			return
		}
		id := identify(r.Context(), r, st)
		if id.Login == "" {
			http.Error(w, "could not resolve identity", http.StatusForbidden)
			return
		}
		tenantID := r.URL.Query().Get("tenant_id")
		if tenantID == "" {
			http.Error(w, "missing tenant_id", http.StatusBadRequest)
			return
		}
		if !id.IsAdmin {
			if _, ok, err := st.RoleForLogin(tenantID, id.Login); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if !ok {
				http.Error(w, fmt.Sprintf("%q is not a member of tenant %q", id.Login, tenantID), http.StatusForbidden)
				return
			}
		}

		convs, err := st.ListConversations(tenantID, id.Login)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		summaries := make([]conversationSummary, len(convs))
		for i, c := range convs {
			summaries[i] = conversationSummary{ID: c.ID, Title: c.Title, UpdatedAt: c.UpdatedAt.Format("2006-01-02T15:04:05Z07:00")}
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"conversations": summaries})
	}
}

// chatMessageOut is one message of GET /llm/conversations/{id}/messages —
// only user/assistant text turns, tool-call/result pairs (see
// store.Message.ToolName) are internal plumbing the chat panel doesn't
// render as bubbles.
type chatMessageOut struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// llmConversationMessagesHandler serves
// GET /llm/conversations/{id}/messages?tenant_id=X — the full text
// history of one conversation, for loading it back into the chat panel.
// Ownership (tenant_id + login) is enforced by store.GetConversation
// itself before ListMessages is ever called — same
// indistinguishable-from-"not found" pattern as everywhere else
// conversation ownership is checked.
func llmConversationMessagesHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if corsForPlugin(w, r, "GET") {
			return
		}
		id := identify(r.Context(), r, st)
		if id.Login == "" {
			http.Error(w, "could not resolve identity", http.StatusForbidden)
			return
		}
		tenantID := r.URL.Query().Get("tenant_id")
		convID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if tenantID == "" || err != nil {
			http.Error(w, "missing/invalid tenant_id or conversation id", http.StatusBadRequest)
			return
		}
		if _, err := st.GetConversation(convID, tenantID, id.Login); err != nil {
			http.Error(w, "conversation not found", http.StatusForbidden)
			return
		}

		stored, err := st.ListMessages(convID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		var out []chatMessageOut
		for _, m := range stored {
			if m.ToolName != "" {
				continue
			}
			out = append(out, chatMessageOut{Role: m.Role, Content: m.Content})
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"conversation_id": convID, "messages": out})
	}
}

// llmDeleteConversationHandler serves
// POST /llm/conversations/{id}/delete?tenant_id=X — deletes one of the
// caller's own conversations. Not admin-gated: ownership (tenant_id +
// login) is enforced by store.DeleteConversation itself, same
// indistinguishable-from-"not found" pattern as everywhere else — an
// admin has no special permission to delete someone else's conversation,
// this was always private per-login (see docs/LLM-INTEGRATION.md "Chat
// history").
func llmDeleteConversationHandler(st *store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if corsForPlugin(w, r, "POST") {
			return
		}
		id := identify(r.Context(), r, st)
		if id.Login == "" {
			http.Error(w, "could not resolve identity", http.StatusForbidden)
			return
		}
		tenantID := r.URL.Query().Get("tenant_id")
		convID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if tenantID == "" || err != nil {
			http.Error(w, "missing/invalid tenant_id or conversation id", http.StatusBadRequest)
			return
		}
		if err := st.DeleteConversation(convID, tenantID, id.Login); err != nil {
			http.Error(w, "conversation not found", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// llmChatHandler serves /llm/chat — CORS-enabled (see corsForPlugin), one
// full chat turn per call: persist the user's message, run the
// tool-calling loop until the model gives a final answer, persist and
// return it.
func llmChatHandler(deps llmChatDeps) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if corsForPlugin(w, r, "POST") {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		cfg, err := loadLLMConfig(deps.st)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if cfg.client == nil {
			http.Error(w, "LLM chat not configured yet (see /llm-settings)", http.StatusServiceUnavailable)
			return
		}

		id := identify(r.Context(), r, deps.st)
		if id.Login == "" {
			http.Error(w, "could not resolve identity", http.StatusForbidden)
			return
		}

		var req chatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.TenantID == "" || req.Message == "" {
			http.Error(w, "missing tenant_id/message", http.StatusBadRequest)
			return
		}

		if !id.IsAdmin {
			if _, ok, err := deps.st.RoleForLogin(req.TenantID, id.Login); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			} else if !ok {
				http.Error(w, fmt.Sprintf("%q is not a member of tenant %q", id.Login, req.TenantID), http.StatusForbidden)
				return
			}
		}

		convID := req.ConversationID
		if convID == 0 {
			title := req.Message
			if len(title) > 60 {
				title = title[:60]
			}
			newID, err := deps.st.CreateConversation(req.TenantID, id.Login, title)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			convID = newID
		} else if _, err := deps.st.GetConversation(convID, req.TenantID, id.Login); err != nil {
			// Same as everywhere else conversation ownership is checked:
			// wrong owner and "doesn't exist" are indistinguishable on
			// purpose.
			http.Error(w, "conversation not found", http.StatusForbidden)
			return
		}

		if err := deps.st.AppendMessage(convID, openai.ChatMessageRoleUser, req.Message, "", "", ""); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		reply, err := runChatTurn(r.Context(), deps, cfg, req.TenantID, id.Login, convID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResponse{ConversationID: convID, Reply: reply})
	}
}

// runChatTurn replays convID's stored history into the shape the model
// expects, then loops: ask the model, execute any tool_calls it emits
// in-process (see llmtools.go's callTool), feed the results back, repeat
// until it produces a final text answer (or llmMaxToolIterations is hit).
func runChatTurn(ctx context.Context, deps llmChatDeps, cfg llmConfig, tenantID, login string, convID int64) (string, error) {
	stored, err := deps.st.ListMessages(convID)
	if err != nil {
		return "", err
	}

	messages := []openai.ChatCompletionMessage{{Role: openai.ChatMessageRoleSystem, Content: cfg.systemPrompt}}
	for _, m := range stored {
		if m.ToolName != "" {
			// A tool call + its result, replayed as the assistant/tool
			// message pair the model originally produced/received. The
			// synthetic call ID only needs to be internally consistent
			// between these two messages, not stable across turns.
			callID := fmt.Sprintf("call_%d", m.ID)
			messages = append(messages,
				openai.ChatCompletionMessage{
					Role: openai.ChatMessageRoleAssistant,
					ToolCalls: []openai.ToolCall{{
						ID: callID, Type: openai.ToolTypeFunction,
						Function: openai.FunctionCall{Name: m.ToolName, Arguments: m.ToolArgs},
					}},
				},
				openai.ChatCompletionMessage{Role: openai.ChatMessageRoleTool, ToolCallID: callID, Content: m.ToolResult},
			)
		} else {
			messages = append(messages, openai.ChatCompletionMessage{Role: m.Role, Content: m.Content})
		}
	}

	for i := 0; i < llmMaxToolIterations; i++ {
		resp, err := cfg.client.CreateChatCompletion(ctx, openai.ChatCompletionRequest{
			Model:    cfg.model,
			Messages: messages,
			Tools:    toolSchemas,
		})
		if err != nil {
			return "", fmt.Errorf("llm: %w", err)
		}
		if len(resp.Choices) == 0 {
			// Observed for real (OpenRouter/Haiku, twice in a row during
			// testing, both times resolved by a plain retry) with no way to
			// tell after the fact whether it's genuine provider-side
			// flakiness or a real pattern worth investigating further —
			// this is the only place that distinction could ever be made.
			log.Printf("llm: empty choices from provider (model=%q, iteration=%d)", cfg.model, i)
			return "", fmt.Errorf("llm: empty response")
		}
		msg := resp.Choices[0].Message

		if len(msg.ToolCalls) == 0 {
			if strings.TrimSpace(msg.Content) == "" {
				// Observed live against zeus's Ollama (large/thinking
				// model): after a real, correct tool call sequence, the
				// model sometimes ends its turn with no tool_calls AND
				// empty content — the model itself seems to occasionally
				// fail to produce the final answer text, not something
				// wrong with our data or request. Nudging once with an
				// explicit request for the final answer recovers it in
				// practice; if it happens again the loop just consumes
				// another iteration rather than looping forever.
				messages = append(messages, msg, openai.ChatCompletionMessage{
					Role:    openai.ChatMessageRoleUser,
					Content: "Your previous reply was empty. Now provide the final answer in natural language, based on the data already gathered.",
				})
				continue
			}
			if err := deps.st.AppendMessage(convID, openai.ChatMessageRoleAssistant, msg.Content, "", "", ""); err != nil {
				return "", err
			}
			return msg.Content, nil
		}

		messages = append(messages, msg)
		for _, tc := range msg.ToolCalls {
			result, toolErr := callTool(ctx, deps.tools, tenantID, login, tc.Function.Name, json.RawMessage(tc.Function.Arguments))
			var resultJSON []byte
			if toolErr != nil {
				resultJSON, _ = json.Marshal(map[string]string{"error": toolErr.Error()})
			} else {
				resultJSON, _ = json.Marshal(result)
			}
			if err := deps.st.AppendMessage(convID, "", "", tc.Function.Name, tc.Function.Arguments, string(resultJSON)); err != nil {
				return "", err
			}
			messages = append(messages, openai.ChatCompletionMessage{
				Role: openai.ChatMessageRoleTool, ToolCallID: tc.ID, Content: string(resultJSON),
			})
		}
	}
	return "", fmt.Errorf("llm: too many tool-call iterations without a final answer")
}

// toolSchemas is the JSON-schema description of every tool in
// toolDispatch (llmtools.go) — exactly what the model reads to decide
// whether/how to call one, as important as the system prompt itself (see
// memory: living LLM integration design doc, "Tool response format
// discipline").
var toolSchemas = []openai.Tool{
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "list_devices",
			Description: "List the caller's tenant's devices, with online/offline status.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"online_only": map[string]any{"type": "boolean", "description": "If true, only show online devices."},
				},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "get_device_details",
			Description: "A device's details: attributes, available telemetry metrics (each with a description, if configured, of what it measures and what a normal value looks like — use these to decide which metric to check, don't rely on the name alone), recent records/treatments.",
			Parameters: map[string]any{
				"type":       "object",
				"properties": map[string]any{"device_id": map[string]any{"type": "string"}},
				"required":   []string{"device_id"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "get_telemetry_summary",
			Description: "Aggregated statistics (min/max/mean/last value) for one metric of one device over a range — never raw points.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"device_id": map[string]any{"type": "string"},
					"metric":    map[string]any{"type": "string", "description": "One of the names in available_metrics from get_device_details."},
					"from":      map[string]any{"type": "string", "description": "RFC3339, optional (default last 24h)."},
					"to":        map[string]any{"type": "string", "description": "RFC3339, optional (default now)."},
					"record_id": map[string]any{"type": "string", "description": "Optional, filters to a single treatment/record."},
				},
				"required": []string{"device_id", "metric"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "get_telemetry_series",
			Description: "Real (non-aggregated) points for one metric of one device, up to a cap — use only when you need the detail, not for summary questions.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"device_id":  map[string]any{"type": "string"},
					"metric":     map[string]any{"type": "string"},
					"from":       map[string]any{"type": "string", "description": "RFC3339, optional (default last 24h)."},
					"to":         map[string]any{"type": "string", "description": "RFC3339, optional (default now)."},
					"record_id":  map[string]any{"type": "string"},
					"max_points": map[string]any{"type": "integer", "description": "Default 200, max 1000."},
				},
				"required": []string{"device_id", "metric"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "search_records",
			Description: "Search records/treatments by filters (device_id, record_id, ts, or any field inside the record's data).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"filters": map[string]any{"type": "object", "description": "Field/value pairs, all AND-ed together."},
					"limit":   map[string]any{"type": "integer", "description": "Default 20, max 100."},
				},
				"required": []string{"filters"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "generate_report",
			Description: "Generate a markdown report the user can download — use only on an explicit request, not for every answer.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"title":            map[string]any{"type": "string"},
					"content_markdown": map[string]any{"type": "string"},
				},
				"required": []string{"content_markdown"},
			},
		},
	},
	{
		Type: openai.ToolTypeFunction,
		Function: &openai.FunctionDefinition{
			Name:        "get_file_content",
			Description: "Fetch the bytes (base64) of a file associated with a device/record — use it to analyze an image found in a record via search_records (the 'filename' field).",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"device_id": map[string]any{"type": "string"},
					"filename":  map[string]any{"type": "string"},
				},
				"required": []string{"device_id", "filename"},
			},
		},
	},
}
