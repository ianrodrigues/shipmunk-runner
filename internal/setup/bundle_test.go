package setup

import (
	"strings"
	"testing"
	"time"
)

func TestParseRejectsExpiredAndAmbiguousBundles(t *testing.T) {
	token := "1|" + strings.Repeat("a", 40)
	raw := []byte(`{"version":1,"runtime_version":"0.154.0","base_url":"https://shipmunk.example","runner_id":"01k4w000000000000000000001","profile_id":"01k4w000000000000000000002","expires_at":"2099-01-01T00:00:00Z","profile_token":"` + token + `","execution_token":"` + token + `"}`)
	bundle, err := Parse(raw, time.Unix(0, 0))
	if err != nil || bundle.RunnerID != "01k4w000000000000000000001" {
		t.Fatalf("valid bundle: %#v, %v", bundle, err)
	}
	for _, invalid := range [][]byte{
		[]byte(strings.Replace(string(raw), `"version":1`, `"version":1,"version":2`, 1)),
		[]byte(strings.Replace(string(raw), `2099-01-01T00:00:00Z`, `1970-01-01T00:00:00Z`, 1)),
		[]byte(strings.Replace(string(raw), `https://shipmunk.example`, `http://shipmunk.example`, 1)),
	} {
		if _, err := Parse(invalid, time.Unix(0, 0)); err == nil {
			t.Fatal("accepted invalid setup bundle")
		}
	}
}
