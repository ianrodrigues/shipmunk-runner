package profile

import (
	"reflect"
	"strings"
	"testing"
)

func TestBindingIdentityUsesOnlyStableFields(t *testing.T) {
	binding := Binding{
		ProfileID:           "profile",
		CredentialReference: "credential:opaque",
		Agent:               AgentCodex,
		AuthMode:            AuthSubscription,
		RuntimeVersion:      CodexVersion,
	}
	identity := BindingIdentity(binding)
	if identity != (Identity{
		ProfileID:           binding.ProfileID,
		CredentialReference: binding.CredentialReference,
		Agent:               binding.Agent,
		AuthMode:            binding.AuthMode,
		RuntimeVersion:      binding.RuntimeVersion,
	}) {
		t.Fatalf("identity = %#v", identity)
	}
	if changed := BindingIdentity(Binding{ProfileID: binding.ProfileID, CredentialReference: "credential:rotated"}); changed == identity {
		t.Fatal("credential-reference rotation did not change identity")
	}
}

func TestPinnedVersionsAndCommands(t *testing.T) {
	versions := map[string]string{AgentCodex: CodexVersion, AgentClaude: ClaudeVersion}
	for agent, version := range versions {
		if actual, ok := PinnedVersion(agent); !ok || actual != version {
			t.Errorf("PinnedVersion(%q) = %q, %v", agent, actual, ok)
		}
		for _, operation := range []string{"version", "login", "probe", "preflight"} {
			if command, ok := Command(agent, operation); !ok || len(command) == 0 || command[0] == "" {
				t.Errorf("Command(%q, %q) = %#v, %v", agent, operation, command, ok)
			}
		}
	}
	if _, ok := PinnedVersion("future-agent"); ok {
		t.Fatal("unknown agent has a pinned version")
	}
	if _, ok := Command(AgentCodex, "shell"); ok {
		t.Fatal("unsupported operation has a command")
	}
	codexPreflight, _ := Command(AgentCodex, "preflight")
	if !contains(codexPreflight, "--ephemeral") || !contains(codexPreflight, "features.shell_tool=false") || !contains(codexPreflight, "web_search=\"disabled\"") {
		t.Fatalf("Codex preflight is missing restrictions: %#v", codexPreflight)
	}
	claudePreflight, _ := Command(AgentClaude, "preflight")
	if !contains(claudePreflight, "--strict-mcp-config") || !contains(claudePreflight, "--no-session-persistence") || !contains(claudePreflight, "--tools") {
		t.Fatalf("Claude preflight is missing restrictions: %#v", claudePreflight)
	}
}

func TestVersionMatchesRequiresPinnedExactOutput(t *testing.T) {
	cases := []struct {
		name     string
		agent    string
		expected string
		result   CommandResult
		want     bool
	}{
		{"codex exact", AgentCodex, CodexVersion, CommandResult{ExitCode: 0, Stdout: "codex-cli 0.154.0\n"}, true},
		{"claude exact", AgentClaude, ClaudeVersion, CommandResult{ExitCode: 0, Stdout: "2.1.269 (Claude Code)\n"}, true},
		{"untrusted expected", AgentCodex, "0.155.0", CommandResult{ExitCode: 0, Stdout: "codex-cli 0.155.0"}, false},
		{"wrong output", AgentCodex, CodexVersion, CommandResult{ExitCode: 0, Stdout: "codex-cli 0.154.1"}, false},
		{"failed process", AgentCodex, CodexVersion, CommandResult{ExitCode: 1, Stdout: "codex-cli 0.154.0"}, false},
		{"unknown agent", "unknown", "0", CommandResult{ExitCode: 0}, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := VersionMatches(test.agent, test.expected, test.result); got != test.want {
				t.Fatalf("VersionMatches() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestStatusHealthRequiresExplicitSubscriptionMode(t *testing.T) {
	for _, subscription := range []string{"pro", "max", "team", "enterprise"} {
		status := `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"` + subscription + `"}`
		if got := StatusHealth(AgentClaude, CommandResult{Stdout: status}); got.Health != HealthReady || got.Reason != "" {
			t.Errorf("Claude subscription %q health = %#v", subscription, got)
		}
	}
	for name, status := range map[string]string{
		"console":       `{"loggedIn":true,"authMethod":"console","apiProvider":"firstParty","subscriptionType":"pro"}`,
		"api provider":  `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"bedrock","subscriptionType":"pro"}`,
		"unknown plan":  `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"api"}`,
		"not logged in": `{"loggedIn":false,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"pro"}`,
		"wrong type":    `{"loggedIn":"true","authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"pro"}`,
		"trailing JSON": `{"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"pro"} {}`,
		"duplicate key": `{"loggedIn":false,"loggedIn":true,"authMethod":"claude.ai","apiProvider":"firstParty","subscriptionType":"pro"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if got := StatusHealth(AgentClaude, CommandResult{Stdout: status}); got.Health != HealthUnsupported || got.Reason != ReasonNativeModeMismatch {
				t.Fatalf("health = %#v", got)
			}
		})
	}
	if got := StatusHealth(AgentCodex, CommandResult{Stdout: "Logged in using ChatGPT", Stderr: "\n"}); got.Health != HealthReady {
		t.Fatalf("Codex ChatGPT status health = %#v", got)
	}
	if got := StatusHealth(AgentCodex, CommandResult{Stdout: "Logged in using an API key"}); got.Health != HealthUnsupported {
		t.Fatalf("Codex API-key status health = %#v", got)
	}
	if got := StatusHealth(AgentCodex, CommandResult{ExitCode: 1, Stdout: "Logged in using ChatGPT"}); got != (Health{Health: HealthExpired, Reason: ReasonNativeLoginRequired}) {
		t.Fatalf("failed probe health = %#v", got)
	}
}

func TestAuthenticatedHealthClaudeRequiresStructuredExactResult(t *testing.T) {
	valid := `{"type":"result","subtype":"success","is_error":false,"result":"SHIPMUNK_AUTH_OK"}`
	if got := AuthenticatedHealth(AgentClaude, CommandResult{Stdout: valid}); got.Health != HealthReady {
		t.Fatalf("valid Claude result health = %#v", got)
	}
	for name, output := range map[string]string{
		"empty":             "",
		"wrong type":        `{}`,
		"wrong terminal":    `{"type":"result","subtype":"error","is_error":false,"result":"SHIPMUNK_AUTH_OK"}`,
		"error":             `{"type":"result","subtype":"success","is_error":true,"result":"SHIPMUNK_AUTH_OK"}`,
		"wrong response":    `{"type":"result","subtype":"success","is_error":false,"result":"not exact"}`,
		"malformed":         `{"type":"result"`,
		"trailing JSON":     valid + `{}`,
		"duplicate result":  `{"type":"result","subtype":"success","is_error":false,"result":"bad","result":"SHIPMUNK_AUTH_OK"}`,
		"non-string result": `{"type":"result","subtype":"success","is_error":false,"result":42}`,
	} {
		t.Run(name, func(t *testing.T) {
			got := AuthenticatedHealth(AgentClaude, CommandResult{Stdout: output})
			if got != (Health{Health: HealthError, Reason: ReasonNativeProbeFailed}) {
				t.Fatalf("health = %#v", got)
			}
		})
	}
	if got := AuthenticatedHealth(AgentClaude, CommandResult{ExitCode: 1, Stdout: valid}); got.Health != HealthError {
		t.Fatalf("failed process health = %#v", got)
	}
}

func TestAuthenticatedHealthCodexValidatesEntireJSONLStream(t *testing.T) {
	valid := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"SHIPMUNK_AUTH_OK"}}`,
		`{"type":"turn.completed"}`,
		"",
	}, "\n")
	if got := AuthenticatedHealth(AgentCodex, CommandResult{Stdout: valid}); got.Health != HealthReady {
		t.Fatalf("valid Codex result health = %#v", got)
	}
	for name, output := range map[string]string{
		"missing newline": strings.TrimSuffix(valid, "\n"),
		"malformed line":  strings.Replace(valid, `{"type":"turn.started"}`, "not-json", 1),
		"duplicate key":   strings.Replace(valid, `"thread_id":"thread"`, `"thread_id":"thread","thread_id":"other"`, 1),
		"unknown event":   strings.Replace(valid, `{"type":"turn.started"}`, `{"type":"future.failure"}`, 1),
		"tool item": strings.Replace(valid,
			`{"id":"message","type":"agent_message","text":"SHIPMUNK_AUTH_OK"}`,
			`{"id":"message","type":"command_execution"}`, 1),
		"unfinished item": strings.Replace(valid,
			`{"type":"item.completed","item":{"id":"message","type":"agent_message","text":"SHIPMUNK_AUTH_OK"}}`,
			`{"type":"item.started","item":{"id":"message","type":"agent_message"}}`, 1),
		"trailing event": valid + `{"type":"turn.completed"}` + "\n",
		"bad terminal":   strings.Replace(valid, `{"type":"turn.completed"}`, `{"type":"turn.failed"}`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			got := AuthenticatedHealth(AgentCodex, CommandResult{Stdout: output})
			if got != (Health{Health: HealthError, Reason: ReasonNativeProbeFailed}) {
				t.Fatalf("health = %#v", got)
			}
		})
	}
}

func TestCodexPreflightRateLimitIsRecognizedOnlyFromCompleteFailure(t *testing.T) {
	for _, failure := range []string{
		`{"type":"turn.failed","error":{"code":"rate_limit_exceeded","message":"PRIVATE"}}`,
		`{"type":"error","code":"rate_limit_exceeded","message":"PRIVATE"}`,
		`{"type":"turn.failed","error":{"message":"{\"error\":{\"code\":\"rate_limit_exceeded\"}}"}}`,
	} {
		got := AuthenticatedHealth(AgentCodex, CommandResult{ExitCode: 1, Stdout: failure + "\n"})
		if got != (Health{Health: HealthRateLimited, Reason: ReasonRateLimited}) {
			t.Errorf("rate limit health = %#v for %s", got, failure)
		}
	}
	withActiveItem := strings.Join([]string{
		`{"type":"thread.started","thread_id":"thread"}`,
		`{"type":"turn.started"}`,
		`{"type":"item.started","item":{"id":"reasoning","type":"reasoning"}}`,
		`{"type":"item.completed","item":{"id":"error","type":"error","code":"rate_limit_exceeded","message":"PRIVATE"}}`,
		`{"type":"turn.failed","error":{"message":"private"}}`,
		"",
	}, "\n")
	if got := AuthenticatedHealth(AgentCodex, CommandResult{ExitCode: 1, Stdout: withActiveItem}); got.Health != HealthRateLimited {
		t.Fatalf("item rate limit with active item = %#v", got)
	}
	for name, output := range map[string]string{
		"missing newline":     `{"type":"error","code":"rate_limit_exceeded"}`,
		"trailing malformed":  `{"type":"error","code":"rate_limit_exceeded"}` + "\nnot-json\n",
		"success after error": `{"type":"error","code":"rate_limit_exceeded"}` + "\n" + `{"type":"turn.completed"}` + "\n",
		"unknown code":        `{"type":"turn.failed","error":{"code":"future_limit","message":"PRIVATE"}}` + "\n",
		"orphan item update":  `{"type":"item.updated","item":{"id":"orphan","type":"error","code":"rate_limit_exceeded"}}` + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			if got := AuthenticatedHealth(AgentCodex, CommandResult{ExitCode: 1, Stdout: output}); got.Health != HealthError {
				t.Fatalf("health = %#v", got)
			}
		})
	}
}

func TestCodexPreflightBoundsAndSanitizesFailures(t *testing.T) {
	if got := AuthenticatedHealth(AgentCodex, CommandResult{Stdout: strings.Repeat("x", maxCodexOutputBytes+1)}); got.Health != HealthError {
		t.Fatalf("oversized output health = %#v", got)
	}
	if got := AuthenticatedHealth(AgentCodex, CommandResult{Stdout: `{"type":"turn.failed","error":{"message":"PRIVATE_PROVIDER_TEXT"}}` + "\n", Stderr: "PRIVATE_STDERR"}); got != (Health{Health: HealthError, Reason: ReasonNativeProbeFailed}) {
		t.Fatalf("generic failure was not sanitized: %#v", got)
	}
	for _, text := range []string{"authentication expired", "not logged in", "unauthorized"} {
		if got := AuthenticatedHealth(AgentCodex, CommandResult{ExitCode: 1, Stdout: `{"type":"turn.failed","error":{"message":"` + text + `"}}` + "\n"}); got.Health != HealthError || got.Reason != ReasonNativeProbeFailed {
			t.Errorf("auth failure %q = %#v", text, got)
		}
	}
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func TestCommandArgumentSlicesAreIndependent(t *testing.T) {
	first, _ := Command(AgentCodex, "login")
	second, _ := Command(AgentCodex, "login")
	first[0] = "changed"
	if reflect.DeepEqual(first, second) {
		t.Fatal("Command returned a shared mutable argument slice")
	}
}
