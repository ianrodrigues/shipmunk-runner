package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const contracts = "../../contracts-source/contracts/v1"

func TestPinnedContractFixtures(t *testing.T) {
	valid, err := filepath.Glob(filepath.Join(contracts, "fixtures/valid/*.json"))
	if err != nil || len(valid) == 0 {
		t.Fatalf("find valid fixtures: %v", err)
	}
	for _, fixture := range valid {
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		contract := strings.TrimSuffix(filepath.Base(fixture), ".json")
		if err := ValidateFixture(contract, contracts, raw); err != nil {
			t.Errorf("%s: %v", fixture, err)
		}
	}
	invalid, err := filepath.Glob(filepath.Join(contracts, "fixtures/invalid/*.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range invalid {
		raw, err := os.ReadFile(fixture)
		if err != nil {
			t.Fatal(err)
		}
		wrapper, err := Decode(raw, ManifestMaxBytes)
		if err != nil {
			t.Fatal(err)
		}
		data, err := object(wrapper)
		if err != nil {
			t.Fatal(err)
		}
		contract, ok := data["contract"].(string)
		if !ok {
			t.Fatalf("%s has no contract", fixture)
		}
		document, ok := data["document"]
		if !ok {
			t.Fatalf("%s has no document", fixture)
		}
		encoded := []byte(mustJSON(document))
		if err := ValidateFixture(contract, contracts, encoded); err == nil {
			t.Errorf("%s was accepted", fixture)
		}
	}
}

func TestDecodeRejectsAmbiguousAndMalformedInput(t *testing.T) {
	for name, raw := range map[string][]byte{
		"duplicate root":    []byte(`{"result":1,"result":2}`),
		"duplicate nested":  []byte(`{"result":{"outcome":"a","outcome":"b"}}`),
		"trailing document": []byte(`{} {}`),
		"invalid UTF-8":     append([]byte(`{"x":"`), append([]byte{0xff}, []byte(`"}`)...)...),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(raw, 1024); err == nil {
				t.Fatal("accepted invalid input")
			}
		})
	}
	value, err := Decode([]byte(`{"identity":9007199254740991}`), 1024)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := object(value)
	identity, ok := data["identity"].(json.Number)
	if !ok || identity.String() != "9007199254740991" {
		t.Fatalf("lost numeric identity: %#v", data["identity"])
	}
}

func TestClaimEnforcesCanonicalProtocolFields(t *testing.T) {
	raw := []byte(`{"protocol_version":"1.0","run_id":"01k4w000000000000000000001","attempt_id":"01k4w000000000000000000002","fence":9007199254740991,"deadline":"2099-01-01T00:00:00Z"}`)
	claim, err := ParseClaim(raw, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	if claim.Fence != MaxSafeInteger || !claim.LeaseExpiresAt.Equal(time.Unix(0, 0).UTC().Add(45*time.Second)) {
		t.Fatalf("unexpected claim: %#v", claim)
	}
	for _, replacement := range []string{"\"protocol_version\":\"2.0\"", "\"run_id\":\"01K4W000000000000000000001\"", "\"deadline\":\"2099-01-01T00:00:00+00:00\"", "\"fence\":1.5"} {
		invalid := strings.Replace(string(raw), `"protocol_version":"1.0"`, replacement, 1)
		if strings.Contains(replacement, "run_id") {
			invalid = strings.Replace(string(raw), `"run_id":"01k4w000000000000000000001"`, replacement, 1)
		}
		if strings.Contains(replacement, "deadline") {
			invalid = strings.Replace(string(raw), `"deadline":"2099-01-01T00:00:00Z"`, replacement, 1)
		}
		if strings.Contains(replacement, "fence") {
			invalid = strings.Replace(string(raw), `"fence":9007199254740991`, replacement, 1)
		}
		if _, err := ParseClaim([]byte(invalid), time.Now()); err == nil {
			t.Errorf("accepted %s", replacement)
		}
	}
}

func TestHTTPClientMatchesControlPlaneBoundary(t *testing.T) {
	manifest := `{"protocol_version":"1.0","run_id":"01k4w000000000000000000001","attempt_id":"01k4w000000000000000000002","fence":1,"deadline":"2099-01-01T00:00:00Z"}`
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer runner-token" || request.Header.Get("X-Shipmunk-Protocol") != Version {
			t.Errorf("credentials/version headers missing")
		}
		switch request.URL.Path {
		case "/runner/v1/claims":
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(writer, `{"data":%s}`, manifest)
		case "/runner/v1/attempts/01k4w000000000000000000002/input-artifacts/01k4w000000000000000000003":
			body := []byte("artifact")
			sum := sha256.Sum256(body)
			writer.Header().Set("X-Artifact-SHA256", hex.EncodeToString(sum[:]))
			writer.Header().Set("Content-Length", fmt.Sprint(len(body)))
			_, _ = writer.Write(body)
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "runner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := client.Claim(context.Background(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if claim == nil {
		t.Fatal("idle response became nil")
	}
	bytes, err := client.DownloadArtifact(context.Background(), *claim, "01k4w000000000000000000003", "d3cec991aef473873983c620d75e1cc1d1b3b5a94c99e0c0f07b2d3b4c8d3e5a")
	if err == nil || bytes != nil {
		t.Fatal("incorrect fixture hash was accepted")
	}
	sum := sha256.Sum256([]byte("artifact"))
	bytes, err = client.DownloadArtifact(context.Background(), *claim, "01k4w000000000000000000003", hex.EncodeToString(sum[:]))
	if err != nil || string(bytes) != "artifact" {
		t.Fatalf("artifact: %q, %v", bytes, err)
	}
	if _, err := NewHTTPClient("http://shipmunk.example", "runner-token", nil); err == nil {
		t.Fatal("remote HTTP accepted")
	}
}

func TestHTTPClientFencesAndValidatesOutboundMutations(t *testing.T) {
	event, err := os.ReadFile(filepath.Join(contracts, "fixtures/valid/event-batch.json"))
	if err != nil {
		t.Fatal(err)
	}
	result, err := os.ReadFile(filepath.Join(contracts, "fixtures/valid/result.json"))
	if err != nil {
		t.Fatal(err)
	}
	claim := Claim{RunID: "01k4w000000000000000000001", AttemptID: "01k4w000000000000000000002", Fence: 1}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer runner-token" || request.Header.Get("X-Shipmunk-Protocol") != Version {
			t.Errorf("missing protected headers")
		}
		switch request.URL.Path {
		case "/runner/v1/attempts/01k4w000000000000000000002/events", "/runner/v1/attempts/01k4w000000000000000000002/completion", "/runner/v1/attempts/01k4w000000000000000000002/heartbeat":
			writer.WriteHeader(http.StatusNoContent)
		case "/runner/v1/attempts/01k4w000000000000000000002/artifacts":
			if request.Header.Get("X-Attempt-Fence") != "1" || request.Header.Get("X-Artifact-Kind") != "native_output" || request.Header.Get("X-Protocol-Version") != Version {
				t.Error("missing artifact fence headers")
			}
			fmt.Fprint(writer, `{"data":{"id":"01k4w000000000000000000003"}}`)
		default:
			t.Errorf("unexpected path %s", request.URL.Path)
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	client, err := NewHTTPClient(server.URL, "runner-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SendEvents(context.Background(), claim, event); err != nil {
		t.Fatal(err)
	}
	if err := client.AcknowledgeStopped(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	bytes := []byte("native output")
	sum := sha256.Sum256(bytes)
	if id, err := client.UploadArtifact(context.Background(), claim, "native_output", bytes, hex.EncodeToString(sum[:])); err != nil || id != "01k4w000000000000000000003" {
		t.Fatalf("upload: %q, %v", id, err)
	}
	if err := client.Complete(context.Background(), claim, result); err != nil {
		t.Fatal(err)
	}
}
