package profile

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/ianrodrigues/shipmunk-runner/internal/protocol"
)

const (
	AgentCodex       = "codex"
	AgentClaude      = "claude_code"
	AuthSubscription = "subscription"

	CodexVersion  = "0.154.0"
	ClaudeVersion = "2.1.269"

	HealthReady       = "ready"
	HealthExpired     = "expired"
	HealthUnsupported = "unsupported"
	HealthError       = "error"
	HealthRateLimited = "rate_limited"

	ReasonNativeLoginRequired = "native_login_required"
	ReasonNativeModeMismatch  = "native_mode_mismatch"
	ReasonRuntimeMismatch     = "runtime_mismatch"
	ReasonNativeProbeFailed   = "native_probe_failed"
	ReasonRateLimited         = "rate_limited"

	maxCodexOutputBytes = 2_097_152
	maxCodexLineBytes   = 65_536
	maxCodexEvents      = 10_000
)

// Binding carries only stable profile fields used to compare server grants.
type Binding struct {
	ProfileID           string
	CredentialReference string
	Agent               string
	AuthMode            string
	RuntimeVersion      string
}

// Identity selects the immutable fields whose change revokes an operation.
type Identity struct {
	ProfileID           string
	CredentialReference string
	Agent               string
	AuthMode            string
	RuntimeVersion      string
}

func BindingIdentity(binding Binding) Identity {
	return Identity{
		ProfileID:           binding.ProfileID,
		CredentialReference: binding.CredentialReference,
		Agent:               binding.Agent,
		AuthMode:            binding.AuthMode,
		RuntimeVersion:      binding.RuntimeVersion,
	}
}

func PinnedVersion(agent string) (string, bool) {
	switch agent {
	case AgentCodex:
		return CodexVersion, true
	case AgentClaude:
		return ClaudeVersion, true
	default:
		return "", false
	}
}

// Command returns the fixed executable invocation for a supported native
// operation. The returned slice is safe to pass directly to exec.Command.
func Command(agent, operation string) ([]string, bool) {
	switch {
	case agent == AgentCodex && operation == "version":
		return []string{"/usr/local/bin/codex", "--version"}, true
	case agent == AgentCodex && operation == "login":
		return []string{"/usr/local/bin/codex", "login", "--device-auth"}, true
	case agent == AgentCodex && operation == "preflight":
		return []string{
			"/usr/local/bin/codex", "exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules",
			"--skip-git-repo-check", "--sandbox", "read-only",
			"-c", `forced_login_method="chatgpt"`, "-c", `cli_auth_credentials_store="file"`,
			"-c", "features.shell_tool=false", "-c", "features.unified_exec=false",
			"-c", `web_search="disabled"`,
			"Reply exactly SHIPMUNK_AUTH_OK. Do not use tools or inspect files.",
		}, true
	case agent == AgentCodex && operation == "probe":
		return []string{"/usr/local/bin/codex", "login", "status"}, true
	case agent == AgentClaude && operation == "version":
		return []string{"/usr/local/bin/claude", "--version"}, true
	case agent == AgentClaude && operation == "login":
		return []string{"/usr/local/bin/claude", "auth", "login", "--claudeai"}, true
	case agent == AgentClaude && operation == "probe":
		return []string{"/usr/local/bin/claude", "auth", "status"}, true
	case agent == AgentClaude && operation == "preflight":
		return []string{
			"/usr/local/bin/claude", "-p", "--output-format", "json", "--safe-mode",
			"--tools", "", "--strict-mcp-config", "--no-session-persistence", "--max-turns", "1",
			"--settings", `{"forceLoginMethod":"claudeai"}`,
			"Reply exactly SHIPMUNK_AUTH_OK. Do not use tools or inspect files.",
		}, true
	default:
		return nil, false
	}
}

type CommandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

type Health struct {
	Health string
	Reason string
}

func VersionMatches(agent, expected string, result CommandResult) bool {
	pinned, supported := PinnedVersion(agent)
	if !supported || expected != pinned || result.ExitCode != 0 {
		return false
	}
	actual := strings.TrimSpace(result.Stdout)
	switch agent {
	case AgentCodex:
		return actual == "codex-cli "+expected
	case AgentClaude:
		return actual == expected+" (Claude Code)"
	default:
		return false
	}
}

func StatusHealth(agent string, result CommandResult) Health {
	if result.ExitCode != 0 {
		return Health{Health: HealthExpired, Reason: ReasonNativeLoginRequired}
	}
	switch agent {
	case AgentCodex:
		if strings.TrimSpace(result.Stdout+result.Stderr) == "Logged in using ChatGPT" {
			return Health{Health: HealthReady}
		}
	case AgentClaude:
		if claudeStatusReady(result.Stdout) {
			return Health{Health: HealthReady}
		}
	}
	return Health{Health: HealthUnsupported, Reason: ReasonNativeModeMismatch}
}

func claudeStatusReady(output string) bool {
	value, err := protocol.Decode([]byte(output), 16_384)
	if err != nil {
		return false
	}
	status, ok := value.(map[string]any)
	if !ok {
		return false
	}
	loggedIn, loggedInOK := status["loggedIn"].(bool)
	authMethod, authOK := status["authMethod"].(string)
	provider, providerOK := status["apiProvider"].(string)
	subscription, subscriptionOK := status["subscriptionType"].(string)
	return loggedInOK && loggedIn && authOK && authMethod == "claude.ai" &&
		providerOK && provider == "firstParty" && subscriptionOK &&
		(subscription == "pro" || subscription == "max" || subscription == "team" || subscription == "enterprise")
}

func AuthenticatedHealth(agent string, result CommandResult) Health {
	failed := Health{Health: HealthError, Reason: ReasonNativeProbeFailed}
	switch agent {
	case AgentClaude:
		if result.ExitCode != 0 {
			return failed
		}
		value, err := protocol.Decode([]byte(result.Stdout), 16_384)
		if err != nil {
			return failed
		}
		data, ok := value.(map[string]any)
		if !ok {
			return failed
		}
		typeValue, typeOK := data["type"].(string)
		subtype, subtypeOK := data["subtype"].(string)
		isError, errorOK := data["is_error"].(bool)
		message, messageOK := data["result"].(string)
		if typeOK && typeValue == "result" && subtypeOK && subtype == "success" &&
			errorOK && !isError && messageOK && strings.TrimSpace(message) == "SHIPMUNK_AUTH_OK" {
			return Health{Health: HealthReady}
		}
		return failed
	case AgentCodex:
		reason, ok := codexPreflightFailure(result.ExitCode, result.Stdout, result.Stderr)
		if !ok {
			return failed
		}
		if reason == "rate_limited" {
			return Health{Health: HealthRateLimited, Reason: ReasonRateLimited}
		}
		if reason == "" {
			return Health{Health: HealthReady}
		}
		return failed
	default:
		return failed
	}
}

type codexEvent struct {
	Type     string          `json:"type"`
	ThreadID any             `json:"thread_id"`
	Error    json.RawMessage `json:"error"`
	Item     *codexItem      `json:"item"`
}

type codexItem struct {
	ID      any `json:"id"`
	Type    any `json:"type"`
	Text    any `json:"text"`
	Code    any `json:"code"`
	Message any `json:"message"`
}

type codexItemState struct {
	kind     string
	complete bool
}

// codexPreflightFailure returns ("", true) only for a complete authenticated
// success. A known rate limit is returned separately; all malformed streams
// and other failures map to the sanitized generic probe error.
func codexPreflightFailure(exitCode int, stdout, stderr string) (string, bool) {
	if len(stdout)+len(stderr) > maxCodexOutputBytes || (stdout != "" && !strings.HasSuffix(stdout, "\n")) {
		return "", false
	}
	if stdout == "" {
		if exitCode != 0 {
			return "process_error", true
		}
		return "", false
	}
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	if len(lines) > maxCodexEvents {
		return "", false
	}
	for _, line := range lines {
		if len(line) > maxCodexLineBytes {
			return "", false
		}
	}

	state := "initial"
	terminal := false
	completed := false
	fatalError := false
	message := ""
	items := make(map[string]codexItemState)
	failure := ""
	for _, line := range lines {
		if terminal {
			return "", false
		}
		value, err := protocol.Decode([]byte(line), maxCodexLineBytes)
		if err != nil {
			return "", false
		}
		object, ok := value.(map[string]any)
		if !ok {
			return "", false
		}
		event := decodeCodexEvent(object)
		if event == nil || !validPreflightEvent(event) {
			return "", false
		}
		if strings.HasPrefix(event.Type, "item.") {
			if !recordCodexItem(event, items) {
				return "", false
			}
		}
		itemFailure := strings.HasPrefix(event.Type, "item.") && event.Item != nil && event.Item.Type == "error"
		isFailure := event.Type == "error" || event.Type == "turn.failed" || itemFailure
		if failure != "" && !isFailure {
			return "", false
		}
		if !isFailure {
			switch {
			case event.Type == "thread.started" && state == "initial":
				if !nonemptyText(event.ThreadID, 128) {
					return "", false
				}
				state = "thread"
			case event.Type == "turn.started" && state == "thread":
				state = "turn"
			case state == "turn" && (strings.HasPrefix(event.Type, "item.") || event.Type == "turn.completed"):
			default:
				return "", false
			}
		}
		if event.Type == "item.completed" && event.Item.Type == "agent_message" {
			if text, ok := event.Item.Text.(string); ok {
				message = text
			} else {
				message = ""
			}
		}
		if event.Type == "turn.completed" {
			for _, item := range items {
				if !item.complete {
					return "", false
				}
			}
			completed = true
		}
		terminal = event.Type == "turn.completed" || event.Type == "turn.failed"
		fatalError = fatalError || event.Type == "error"
		if event.Type == "error" || event.Type == "turn.failed" {
			if failure == "" {
				failure = codexFailureReason(event.Error)
			}
		}
		if itemFailure && failure == "" {
			failure = codexFailureItemReason(event.Item)
		}
	}
	if failure != "" && !terminal && !fatalError {
		return "", false
	}
	if failure != "" {
		return failure, true
	}
	if exitCode != 0 || !completed || message != "SHIPMUNK_AUTH_OK" {
		return "process_error", true
	}
	return "", true
}

func decodeCodexEvent(object map[string]any) *codexEvent {
	typeValue, ok := object["type"].(string)
	if !ok || typeValue == "" {
		return nil
	}
	event := &codexEvent{Type: typeValue, ThreadID: object["thread_id"]}
	if typeValue == "error" {
		encoded, err := json.Marshal(object)
		if err != nil {
			return nil
		}
		event.Error = encoded
	} else if raw, exists := object["error"]; exists {
		encoded, err := json.Marshal(raw)
		if err != nil {
			return nil
		}
		event.Error = encoded
	}
	if raw, exists := object["item"]; exists {
		itemObject, ok := raw.(map[string]any)
		if !ok {
			return nil
		}
		event.Item = &codexItem{ID: itemObject["id"], Type: itemObject["type"], Text: itemObject["text"], Code: itemObject["code"], Message: itemObject["message"]}
	}
	return event
}

func validPreflightEvent(event *codexEvent) bool {
	switch event.Type {
	case "thread.started", "turn.started", "turn.completed", "turn.failed", "error":
		return true
	case "item.started", "item.updated", "item.completed":
		if event.Item == nil {
			return false
		}
		kind, ok := event.Item.Type.(string)
		return ok && (kind == "agent_message" || kind == "reasoning" || kind == "error")
	default:
		return false
	}
}

func recordCodexItem(event *codexEvent, items map[string]codexItemState) bool {
	item := event.Item
	if item == nil || !nonemptyText(item.ID, 128) {
		return false
	}
	id := item.ID.(string)
	kind, ok := item.Type.(string)
	if !ok || kind == "" || utf8.RuneCountInString(kind) > 128 {
		return false
	}
	previous, exists := items[id]
	if exists && (previous.complete || previous.kind != kind || event.Type == "item.started") {
		return false
	}
	if event.Type == "item.updated" && !exists {
		return false
	}
	items[id] = codexItemState{kind: kind, complete: event.Type == "item.completed"}
	return true
}

func nonemptyText(value any, maxCharacters int) bool {
	text, ok := value.(string)
	return ok && text != "" && utf8.RuneCountInString(text) <= maxCharacters
}

func codexFailureReason(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "process_error"
	}
	value, err := protocol.Decode(raw, maxCodexLineBytes)
	if err != nil {
		return "process_error"
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "process_error"
	}
	if reason := codexFailureCode(object["code"]); reason != "" {
		return reason
	}
	message, _ := object["message"].(string)
	message = strings.TrimSpace(message)
	if strings.HasPrefix(message, "{") {
		body, err := protocol.Decode([]byte(message), maxCodexLineBytes)
		if err == nil {
			if outer, ok := body.(map[string]any); ok {
				if nested, ok := outer["error"].(map[string]any); ok {
					if reason := codexFailureCode(nested["code"]); reason != "" {
						return reason
					}
				}
			}
		}
		return "process_error"
	}
	return codexFailureMessage(message)
}

func codexFailureItemReason(item *codexItem) string {
	if item == nil {
		return "process_error"
	}
	if reason := codexFailureCode(item.Code); reason != "" {
		return reason
	}
	message, _ := item.Message.(string)
	return codexFailureMessage(message)
}

func codexFailureMessage(message string) string {
	message = strings.TrimSpace(message)
	switch strings.ToLower(message) {
	case "authentication expired", "not logged in", "unauthorized":
		return "auth_expired"
	case "rate limit exceeded", "usage limit reached":
		return "rate_limited"
	case "approval required":
		return "approval_required"
	default:
		return "process_error"
	}
}

func codexFailureCode(value any) string {
	code, ok := value.(string)
	if !ok {
		return ""
	}
	switch code {
	case "token_expired", "auth_expired", "unauthorized", "refresh_token_expired":
		return "auth_expired"
	case "rate_limit_exceeded", "usage_limit_reached":
		return "rate_limited"
	case "approval_required", "approval_request":
		return "approval_required"
	case "model_not_found":
		return "model_unavailable"
	case "invalid_json_schema":
		return "invalid_output_schema"
	default:
		return ""
	}
}
