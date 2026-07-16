package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func pluginReleaseWorkflow(t *testing.T) string {
	t.Helper()
	return githubReadFile(t, ".github/workflows/sitectl-plugin-goreleaser.yaml")
}

func TestHomebrewRecoveryUsesTaggedSourceAndExactCallerConfig(t *testing.T) {
	workflow := pluginReleaseWorkflow(t)
	source := stepBlock(t, workflow, "      - name: Checkout\n", "      - name: Checkout trusted recovery configuration\n")
	config := stepBlock(t, workflow, "      - name: Checkout trusted recovery configuration\n", "      - name: Stage trusted recovery configuration\n")
	stage := stepBlock(t, workflow, "      - name: Stage trusted recovery configuration\n", "      - name: Checkout sitectl SDK\n")
	release := stepBlock(t, workflow, "      - name: Run GoReleaser\n", "      - name: Verify Homebrew publication\n")

	requireContains(t, source, "ref: ${{ inputs.release-version || github.ref }}")
	for _, required := range []string{
		"if: ${{ inputs.release-mode == 'homebrew-only' }}",
		"ref: ${{ github.sha }}",
		"path: .libops-release-config",
		"persist-credentials: false",
	} {
		requireContains(t, config, required)
	}
	for _, required := range []string{
		"CALLER_SHA: ${{ github.sha }}",
		`git -C "$config_checkout" rev-parse HEAD`,
		`[[ "$actual_sha" != "$CALLER_SHA" ]]`,
		`[[ ! -f "$source_config" || -L "$source_config" ]]`,
		"release.disable",
		"GORELEASER_DISABLE_SCM_RELEASE",
		`cp -- "$source_config" "$recovery_config"`,
		`rm -rf -- "$config_checkout"`,
	} {
		requireContains(t, stage, required)
	}
	requireContains(t, stage, `$0 ~ /^  disable:`)
	if strings.Contains(stage, `$0 ~ /^[[:space:]]+disable:`) {
		t.Error("release.disable recovery guard must require canonical direct indentation")
	}
	requireContains(t, release, "inputs.release-mode == 'homebrew-only' && format('--config {0}/libops-goreleaser-recovery.yaml', runner.temp)")
	requireContains(t, release, "GORELEASER_DISABLE_SCM_RELEASE: ${{ inputs.release-mode == 'homebrew-only' }}")
}

func validationStep(t *testing.T, name string) string {
	t.Helper()
	workflow := pluginReleaseWorkflow(t)
	return stepBlock(t, workflow, "      - name: "+name+"\n", "      - name: Checkout\n")
}

func TestRecoveryModesRequireAuthoritativeDefaultBranch(t *testing.T) {
	releaseValidation := validationStep(t, "Validate release mode")
	packageValidation := validationStep(t, "Validate package publication")
	for name, validation := range map[string]string{
		"homebrew": releaseValidation,
		"packages": packageValidation,
	} {
		t.Run(name, func(t *testing.T) {
			for _, required := range []string{
				"REF_NAME: ${{ github.ref_name }}",
				`default_branch="$(gh api "repos/${GITHUB_REPOSITORY}" --jq .default_branch)"`,
				`[[ "$REF_TYPE" != "branch" || "$REF_NAME" != "$default_branch" ]]`,
			} {
				requireContains(t, validation, required)
			}
		})
	}
	requireContains(t, releaseValidation, "            homebrew-only)\n              require_default_branch\n")
	requireContains(t, packageValidation, "            packages-only)\n              require_default_branch\n")

	fullRelease := stepBlock(t, releaseValidation, "            full)\n", "            homebrew-only)\n")
	fullPackages := stepBlock(t, packageValidation, "            full)\n", "            packages-only)\n")
	if strings.Contains(fullRelease, "require_default_branch") || strings.Contains(fullPackages, "require_default_branch") {
		t.Error("full tag releases must not be subject to the recovery-only default branch gate")
	}

	actionlint := githubReadFile(t, ".github/workflows/actionlint.yaml")
	if count := strings.Count(actionlint, `      - "ci/github/**"`); count != 2 {
		t.Errorf("actionlint path filters contain ci/github/** %d times, want pull_request and push", count)
	}
}

type validationResult struct {
	output string
	log    string
	err    error
}

func runReleaseValidation(t *testing.T, stepName, mode, refType, refName, version, defaultBranch string, releaseMissing bool) validationResult {
	t.Helper()
	validation := validationStep(t, stepName)
	tempDir := t.TempDir()
	script := filepath.Join(tempDir, "validate-release.sh")
	if err := os.WriteFile(script, []byte(workflowRunScript(t, validation)), 0700); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	ghLog := filepath.Join(tempDir, "gh.log")
	fakeGH := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "$GH_LOG"
case "${1:-}" in
  api) printf '%s\n' "$DEFAULT_BRANCH" ;;
  release)
    if [[ "$RELEASE_MISSING" == "true" ]]; then exit 1; fi
    printf 'false\n'
    ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte(fakeGH), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", script) // #nosec G204 -- fixed repository script with a test-owned gh fixture.
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"DEFAULT_BRANCH="+defaultBranch,
		"GH_LOG="+ghLog,
		"GH_TOKEN=test-token",
		"GITHUB_REF_NAME="+refName,
		"GITHUB_REPOSITORY=libops/release-contract",
		"REF_NAME="+refName,
		"REF_TYPE="+refType,
		"RELEASE_MISSING="+strconv.FormatBool(releaseMissing),
		"RELEASE_MODE="+mode,
		"RELEASE_VERSION="+version,
	)
	output, runErr := cmd.CombinedOutput()
	log, readErr := os.ReadFile(ghLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return validationResult{output: string(output), log: string(log), err: runErr}
}

func TestRecoveryDefaultBranchGateBehavior(t *testing.T) {
	tests := []struct {
		name    string
		step    string
		mode    string
		version string
	}{
		{name: "homebrew", step: "Validate release mode", mode: "homebrew-only", version: "v0.6.0"},
		{name: "packages", step: "Validate package publication", mode: "packages-only", version: "v0.6.0"},
	}
	for _, test := range tests {
		t.Run(test.name+" default branch", func(t *testing.T) {
			result := runReleaseValidation(t, test.step, test.mode, "branch", "trunk", test.version, "trunk", false)
			if result.err != nil {
				t.Fatalf("default branch recovery failed: %v\n%s", result.err, result.output)
			}
		})
		t.Run(test.name+" feature branch", func(t *testing.T) {
			result := runReleaseValidation(t, test.step, test.mode, "branch", "feature", test.version, "trunk", false)
			if result.err == nil || !strings.Contains(result.output, "must run from the repository default branch (trunk)") {
				t.Fatalf("feature branch recovery was accepted: %v\n%s", result.err, result.output)
			}
		})
		t.Run(test.name+" tag named as default branch", func(t *testing.T) {
			result := runReleaseValidation(t, test.step, test.mode, "tag", "trunk", test.version, "trunk", false)
			if result.err == nil || !strings.Contains(result.output, "must run from the repository default branch (trunk)") {
				t.Fatalf("tag recovery was accepted: %v\n%s", result.err, result.output)
			}
		})
	}
}

func TestFullTagValidationDoesNotQueryDefaultBranch(t *testing.T) {
	tests := []struct {
		name           string
		step           string
		version        string
		releaseMissing bool
	}{
		{name: "goreleaser", step: "Validate release mode", releaseMissing: true},
		{name: "packages", step: "Validate package publication", version: "v0.6.0"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := runReleaseValidation(t, test.step, "full", "tag", "v0.6.0", test.version, "trunk", test.releaseMissing)
			if result.err != nil {
				t.Fatalf("full tag validation changed: %v\n%s", result.err, result.output)
			}
			if strings.Contains(result.log, "api ") {
				t.Fatalf("full tag validation queried recovery branch metadata:\n%s", result.log)
			}
		})
	}
}

func writeRecoveryConfig(t *testing.T, checkout, config string) string {
	t.Helper()
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	commands := [][]string{
		{"git", "init", "--quiet"},
		{"git", "config", "user.name", "Release Contract"},
		{"git", "config", "user.email", "release-contract@example.test"},
	}
	for _, command := range commands {
		cmd := exec.Command(command[0], command[1:]...) // #nosec G204 -- fixed test commands.
		cmd.Dir = checkout
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(command, " "), err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(checkout, ".goreleaser.yaml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	for _, command := range [][]string{{"git", "add", ".goreleaser.yaml"}, {"git", "commit", "--quiet", "-m", "fixture"}} {
		cmd := exec.Command(command[0], command[1:]...) // #nosec G204 -- fixed test commands.
		cmd.Dir = checkout
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", strings.Join(command, " "), err, output)
		}
	}
	cmd := exec.Command("git", "rev-parse", "HEAD") // #nosec G204 -- fixed test command.
	cmd.Dir = checkout
	output, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}

func runRecoveryConfigStage(t *testing.T, config string, mutateSHA bool) (string, error) {
	t.Helper()
	workflow := pluginReleaseWorkflow(t)
	stage := stepBlock(t, workflow, "      - name: Stage trusted recovery configuration\n", "      - name: Checkout sitectl SDK\n")
	script := filepath.Join(t.TempDir(), "stage-recovery-config.sh")
	if err := os.WriteFile(script, []byte(workflowRunScript(t, stage)), 0700); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	runnerTemp := t.TempDir()
	checkout := filepath.Join(workspace, ".libops-release-config")
	callerSHA := writeRecoveryConfig(t, checkout, config)
	if mutateSHA {
		callerSHA = strings.Repeat("0", 40)
	}
	cmd := exec.Command("bash", script) // #nosec G204 -- fixed repository script over test-owned fixtures.
	cmd.Dir = workspace
	cmd.Env = append(os.Environ(), "CALLER_SHA="+callerSHA, "RUNNER_TEMP="+runnerTemp)
	output, err := cmd.CombinedOutput()
	recovered, readErr := os.ReadFile(filepath.Join(runnerTemp, "libops-goreleaser-recovery.yaml"))
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return string(output) + string(recovered), err
}

func TestRecoveryConfigStageAcceptsOnlyCompatibleExactCallerConfig(t *testing.T) {
	const compatible = "version: 2\nrelease:\n  disable: \"{{ .Env.GORELEASER_DISABLE_SCM_RELEASE }}\"\nbuilds: []\n"
	output, err := runRecoveryConfigStage(t, compatible, false)
	if err != nil {
		t.Fatalf("compatible exact config failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, compatible) {
		t.Fatalf("staged recovery config differs from caller config:\n%s", output)
	}

	const oldConfig = "version: 2\nbuilds: []\n"
	if output, err = runRecoveryConfigStage(t, oldConfig, false); err == nil || !strings.Contains(output, "must bind release.disable") {
		t.Fatalf("config without the recovery release guard was accepted: %v\n%s", err, output)
	}

	const nestedConfig = "version: 2\nrelease:\n  github:\n    disable: \"{{ .Env.GORELEASER_DISABLE_SCM_RELEASE }}\"\nbuilds: []\n"
	if output, err = runRecoveryConfigStage(t, nestedConfig, false); err == nil || !strings.Contains(output, "must bind release.disable") {
		t.Fatalf("nested release disable field was accepted: %v\n%s", err, output)
	}

	if output, err = runRecoveryConfigStage(t, compatible, true); err == nil || !strings.Contains(output, "does not match caller SHA") {
		t.Fatalf("config from a different commit was accepted: %v\n%s", err, output)
	}
}
