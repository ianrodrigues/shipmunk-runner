package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestProfileRequestMatchesPHPControlPlaneBoundary(t *testing.T) {
	profileID := "01k4w000000000000000000001"
	operationID := "01k4w000000000000000000002"
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", request.Method)
		}
		if request.Header.Get("Accept") != "application/json" || request.Header.Get("Content-Type") != "application/json" {
			t.Errorf("missing JSON headers: %v", request.Header)
		}
		if request.Header.Get("Authorization") != "Bearer profile-token" || request.Header.Get("X-Shipmunk-Protocol") != Version {
			t.Errorf("missing protected headers: %v", request.Header)
		}
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if payload["protocol_version"] != Version {
			t.Errorf("protocol_version = %#v", payload["protocol_version"])
		}
		if !strings.HasPrefix(request.URL.Path, "/runner/v1/profiles/"+profileID+"/") {
			t.Errorf("unexpected profile path: %s", request.URL.Path)
		}
		if request.URL.Path == "/runner/v1/profiles/"+profileID+"/operations" {
			if payload["operation"] != "login" || payload["operation_id"] != operationID {
				t.Errorf("unexpected begin payload: %#v", payload)
			}
		} else if request.URL.Path != "/runner/v1/profiles/"+profileID+"/operations/"+operationID+"/heartbeat" && request.URL.Path != "/runner/v1/profiles/"+profileID+"/operations/"+operationID+"/completion" {
			t.Errorf("unexpected path: %s", request.URL.Path)
		}
		writer.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(writer, `{"data":{"profile_id":%q,"stop_requested":false}}`, profileID)
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "profile-token", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, request := range []struct {
		suffix  string
		payload map[string]any
	}{
		{suffix: "operations", payload: map[string]any{"operation": "login", "operation_id": operationID}},
		{suffix: "operations/" + operationID + "/heartbeat", payload: map[string]any{}},
		{suffix: "operations/" + operationID + "/completion", payload: map[string]any{"stopped": true, "health": "ready", "reason": nil, "runtime_version": "0.154.0"}},
	} {
		data, err := client.ProfileRequest(context.Background(), profileID, request.suffix, request.payload)
		if err != nil {
			t.Fatalf("profile request %s: %v", request.suffix, err)
		}
		if data["profile_id"] != profileID || data["stop_requested"] != false {
			t.Fatalf("unexpected response data: %#v", data)
		}
	}
	if requests != 3 {
		t.Fatalf("made %d requests, want 3", requests)
	}
}

func TestProfileRequestRejectsUnsafePathsPayloadAndResponses(t *testing.T) {
	const profileID = "01k4w000000000000000000001"
	const operationID = "01k4w000000000000000000002"
	responses := []string{
		`{"data":{"profile_id":"01k4w000000000000000000003"}}`,
		`{"data":{"profile_id":"01k4w000000000000000000001","value":1,"value":2}}`,
	}
	status := http.StatusOK
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if status != http.StatusOK {
			writer.WriteHeader(status)
			return
		}
		_, _ = io.WriteString(writer, responses[0])
		responses = responses[1:]
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "profile-token", nil)
	if err != nil {
		t.Fatal(err)
	}

	for _, input := range []struct {
		profileID string
		suffix    string
		payload   map[string]any
	}{
		{profileID: strings.ToUpper(profileID), suffix: "operations"},
		{profileID: profileID, suffix: "operations/../completion"},
		{profileID: profileID, suffix: "operations/" + strings.ToUpper(operationID) + "/heartbeat"},
		{profileID: profileID, suffix: "operations", payload: map[string]any{"protocol_version": "2.0"}},
		{profileID: profileID, suffix: "operations", payload: map[string]any{"oversized": strings.Repeat("x", ProfileControlPlaneMaxBytes)}},
	} {
		if _, err := client.ProfileRequest(context.Background(), input.profileID, input.suffix, input.payload); err == nil {
			t.Errorf("accepted unsafe request: %#v", input)
		}
	}
	for range 2 {
		if _, err := client.ProfileRequest(context.Background(), profileID, "operations", map[string]any{}); err == nil {
			t.Fatal("accepted invalid response envelope")
		}
	}
	status = http.StatusUpgradeRequired
	if _, err := client.ProfileRequest(context.Background(), profileID, "operations", map[string]any{}); !errors.Is(err, ErrProtocolIncompatible) {
		t.Fatalf("426 response error = %v", err)
	}
}

func TestProfileRequestBoundsResponsesAndDoesNotFollowRedirects(t *testing.T) {
	const profileID = "01k4w000000000000000000001"
	redirectTargetRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/redirect-target" {
			redirectTargetRequests++
			return
		}
		writer.Header().Set("Location", "/redirect-target")
		writer.WriteHeader(http.StatusFound)
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "profile-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ProfileRequest(context.Background(), profileID, "operations", nil); err == nil {
		t.Fatal("accepted a redirect response")
	}
	if redirectTargetRequests != 0 {
		t.Fatal("followed a redirect with profile credentials")
	}

	oversizedServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = io.WriteString(writer, `{"data":{"profile_id":"`+profileID+`","padding":"`+strings.Repeat("x", ProfileControlPlaneMaxBytes)+`"}}`)
	}))
	defer oversizedServer.Close()
	oversizedClient, err := NewHTTPClient(oversizedServer.URL, "profile-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oversizedClient.ProfileRequest(context.Background(), profileID, "operations", nil); err == nil {
		t.Fatal("accepted a response above the profile body limit")
	}
}
