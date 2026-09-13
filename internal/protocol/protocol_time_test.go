package protocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestProtocolTimestampsUseStrictRFC3339UTCGrammar(t *testing.T) {
	for name, testCase := range map[string]struct {
		timestamp string
		valid     bool
	}{
		"whole second":       {timestamp: "2026-09-13T08:00:00Z", valid: true},
		"dot fraction":       {timestamp: "2026-09-13T08:00:00.1Z", valid: true},
		"comma fraction":     {timestamp: "2026-09-13T08:00:00,1Z"},
		"single digit hour":  {timestamp: "2026-09-13T8:00:00Z"},
		"numeric UTC offset": {timestamp: "2026-09-13T08:00:00+00:00"},
	} {
		t.Run(name, func(t *testing.T) {
			event := strings.Replace(validWorkerEventTemplate, "TIMESTAMP", testCase.timestamp, 1)
			eventErr := Validate("worker-event", []byte(event))

			manifest, err := Decode(readContractFixture(t, "manifest"), ManifestMaxBytes)
			if err != nil {
				t.Fatal(err)
			}
			manifestObject, err := object(manifest)
			if err != nil {
				t.Fatal(err)
			}
			manifestObject["deadline"] = testCase.timestamp
			claimJSON, err := json.Marshal(manifestObject)
			if err != nil {
				t.Fatal(err)
			}
			_, claimErr := ParseClaim(claimJSON, time.Now())

			if testCase.valid {
				if eventErr != nil {
					t.Errorf("worker event rejected valid timestamp: %v", eventErr)
				}
				if claimErr != nil {
					t.Errorf("claim rejected valid timestamp: %v", claimErr)
				}
				return
			}
			if eventErr == nil {
				t.Error("worker event accepted a timestamp outside strict RFC3339 UTC syntax")
			}
			if claimErr == nil {
				t.Error("claim accepted a timestamp outside strict RFC3339 UTC syntax")
			}
		})
	}
}

const validWorkerEventTemplate = `{"protocol_version":"1.0","attempt_id":"01k4w000000000000000000002","fence":1,"sequence":1,"type":"progress","timestamp":"TIMESTAMP","payload":{"message":"Working."}}`
