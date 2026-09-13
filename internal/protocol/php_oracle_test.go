package protocol

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestPHPOracleClaimFixture retains a temporary, synthetic comparison against
// the PHP runner. The Go contract tests remain self-contained; this oracle is
// deliberately optional once PHP is retired.
func TestPHPOracleClaimFixture(t *testing.T) {
	php, err := exec.LookPath("php")
	if err != nil {
		t.Skip("PHP 8.5 oracle is not installed")
	}
	if err := exec.Command(php, "-r", `exit(PHP_VERSION_ID >= 80500 ? 0 : 1);`).Run(); err != nil {
		t.Skip("PHP 8.5 oracle is not installed")
	}
	fixture, err := filepath.Abs(filepath.Join(contracts, "fixtures/valid/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	claim, err := ParseClaim(raw, time.Unix(0, 0))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := `require "runner/bootstrap.php"; $data=json_decode(file_get_contents($argv[1]), true, flags: JSON_THROW_ON_ERROR); $claim=Shipmunk\Runner\Claim::fromArray($data); echo $claim->runId."\n".$claim->attemptId."\n".$claim->fence;`
	command := exec.Command(php, "-r", script, fixture)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("PHP oracle failed: %v", err)
	}
	expected := fmt.Sprintf("%s\n%s\n%d", claim.RunID, claim.AttemptID, claim.Fence)
	if string(output) != expected {
		t.Fatalf("unexpected PHP oracle output: %q", output)
	}
}
