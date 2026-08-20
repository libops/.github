package github

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPlatformReleaseSchemaAndValidatorContract(t *testing.T) {
	root := githubRepositoryRoot(t)
	schemaPath := filepath.Join(root, ".github/compatibility/platform-release.schema.json")
	schemaBytes, err := os.ReadFile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(schemaBytes, &schema); err != nil {
		t.Fatalf("schema is invalid JSON: %v", err)
	}
	if schema["$id"] != "https://libops.io/schemas/platform-release.v1.json" {
		t.Fatalf("unexpected schema id: %v", schema["$id"])
	}

	validator := filepath.Join(root, ".github/actions/validate-platform-compatibility/validate.mjs")
	valid := filepath.Join(root, "ci/github/testdata/platform-release.valid.json")
	command := exec.Command("node", validator, "--schema", schemaPath, valid)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("valid compatibility manifest rejected: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "validated 2026.8") {
		t.Fatalf("unexpected validator output: %s", output)
	}
}

func TestPlatformReleaseValidatorRejectsMutableImage(t *testing.T) {
	root := githubRepositoryRoot(t)
	validPath := filepath.Join(root, "ci/github/testdata/platform-release.valid.json")
	contents, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents), "ghcr.io/libops/archivesspace:4.2.0@sha256:1111111111111111111111111111111111111111111111111111111111111111", "ghcr.io/libops/archivesspace:latest", 1))
	invalidPath := filepath.Join(t.TempDir(), "invalid.json")
	if err := os.WriteFile(invalidPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	validator := filepath.Join(root, ".github/actions/validate-platform-compatibility/validate.mjs")
	schemaPath := filepath.Join(root, ".github/compatibility/platform-release.schema.json")
	command := exec.Command("node", validator, "--schema", schemaPath, invalidPath)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("mutable image unexpectedly accepted: %s", output)
	}
	if !strings.Contains(string(output), "exact tag and sha256 digest") {
		t.Fatalf("unexpected validator failure: %s", output)
	}
}

func TestPlatformReleaseValidatorRejectsIncompleteSBOMCoverage(t *testing.T) {
	root := githubRepositoryRoot(t)
	validPath := filepath.Join(root, "ci/github/testdata/platform-release.valid.json")
	contents, err := os.ReadFile(validPath)
	if err != nil {
		t.Fatal(err)
	}
	contents = []byte(strings.Replace(string(contents),
		`"platforms": ["linux/amd64", "linux/arm64"]`,
		`"platforms": ["linux/amd64"]`, 1))
	invalidPath := filepath.Join(t.TempDir(), "invalid-sbom.json")
	if err := os.WriteFile(invalidPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	validator := filepath.Join(root, ".github/actions/validate-platform-compatibility/validate.mjs")
	schemaPath := filepath.Join(root, ".github/compatibility/platform-release.schema.json")
	command := exec.Command("node", validator, "--schema", schemaPath, invalidPath)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("incomplete SBOM coverage unexpectedly accepted: %s", output)
	}
	if !strings.Contains(string(output), "must contain exactly linux/amd64 and linux/arm64") {
		t.Fatalf("unexpected validator failure: %s", output)
	}
}
