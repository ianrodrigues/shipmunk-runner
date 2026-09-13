package protocol

import (
	"embed"
	"fmt"
)

//go:embed contracts/v1/*.schema.json
var contractSchemas embed.FS

func schemaBytes(contract string) ([]byte, error) {
	switch contract {
	case "manifest", "run-input", "result", "patch-artifact", "worker-event", "event-batch":
		return contractSchemas.ReadFile("contracts/v1/" + contract + ".schema.json")
	default:
		return nil, fmt.Errorf("unknown protocol contract")
	}
}
