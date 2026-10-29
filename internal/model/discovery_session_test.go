package model

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDiscoverySessionOmitsControllerInputFiles(t *testing.T) {
	session := DiscoverySession{ProgramID: "program", InputFiles: []DiscoveryInputFile{{Path: "inputs/private-context.json", ContentSHA: "controller-only-sha", Data: []byte("controller-only-body")}}}
	data, err := json.Marshal(session)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"InputFiles", "input_files", "private-context", "controller-only-sha", "controller-only-body"} {
		if strings.Contains(string(data), private) {
			t.Fatalf("controller input leaked to control JSON: %s", private)
		}
	}
	var decoded DiscoverySession
	if err = json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.ProgramID != session.ProgramID || len(decoded.InputFiles) != 0 {
		t.Fatal("session serialization changed public identity or exposed input files")
	}
}
