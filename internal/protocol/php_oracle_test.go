package protocol

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPHPOracleClaimFixture retains a temporary, synthetic comparison against
// the PHP runner. The Go contract tests remain self-contained; this oracle is
// deliberately optional once PHP is retired.
func TestPHPOracleClaimFixture(t *testing.T) {
	php := "/opt/homebrew/opt/php@8.5/bin/php"
	if _, err := os.Stat(php); err != nil {
		t.Skip("PHP 8.5 oracle is not installed")
	}
	fixture, err := filepath.Abs(filepath.Join(contracts, "fixtures/valid/manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	script := `require "runner/bootstrap.php"; $data=json_decode(file_get_contents($argv[1]), true, flags: JSON_THROW_ON_ERROR); $claim=Shipmunk\Runner\Claim::fromArray($data); echo $claim->runId."\n".$claim->attemptId."\n".$claim->fence;`
	command := exec.Command("env", "PATH=/opt/homebrew/opt/php@8.5/bin:"+os.Getenv("PATH"), "php", "-r", script, fixture)
	command.Dir = root
	output, err := command.Output()
	if err != nil {
		t.Fatalf("PHP oracle failed: %v", err)
	}
	if string(output) != "01k4w000000000000000000001\n01k4w000000000000000000002\n1" {
		t.Fatalf("unexpected PHP oracle output: %q", output)
	}
	if !strings.Contains(command.String(), "/opt/homebrew/opt/php@8.5/bin") {
		t.Fatal("oracle did not pin PHP 8.5")
	}
}
