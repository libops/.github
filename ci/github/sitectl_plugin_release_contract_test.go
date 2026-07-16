package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func pluginReleaseWorkflow(t *testing.T) string {
	t.Helper()
	return githubReadFile(t, ".github/workflows/sitectl-plugin-goreleaser.yaml")
}

func releaseJob(t *testing.T) string {
	t.Helper()
	return stepBlock(t, pluginReleaseWorkflow(t), "  release:\n", "  homebrew:\n")
}

func validationJob(t *testing.T) string {
	t.Helper()
	return stepBlock(t, pluginReleaseWorkflow(t), "  validate:\n", "  release:\n")
}

func homebrewJob(t *testing.T) string {
	t.Helper()
	return stepBlock(t, pluginReleaseWorkflow(t), "  homebrew:\n", "  publish-linux-packages:\n")
}

func packageJob(t *testing.T) string {
	t.Helper()
	workflow := pluginReleaseWorkflow(t)
	start := strings.Index(workflow, "  publish-linux-packages:\n")
	if start == -1 {
		t.Fatal("workflow has no publish-linux-packages job")
	}
	return workflow[start:]
}

func TestReleaseAndRecoveryShareAuthoritativePostReleaseReconciliation(t *testing.T) {
	release := releaseJob(t)
	homebrew := homebrewJob(t)

	for _, required := range []string{
		"needs: validate",
		"if: ${{ inputs.release-mode == 'full' }}",
		"args: ${{ inputs.goreleaser-args }} --skip=homebrew",
		"GITHUB_TOKEN: ${{ github.token }}",
	} {
		requireContains(t, release, required)
	}
	if strings.Contains(release, "HOMEBREW_REPO") || strings.Contains(release, "HOMEBREW_TOKEN") {
		t.Error("release builder must not receive the Homebrew repository credential")
	}

	for _, required := range []string{
		"needs: [validate, release]",
		"needs.validate.result == 'success'",
		"inputs.release-mode != 'packages-only'",
		"inputs.release-mode == 'homebrew-only' && needs.release.result == 'skipped'",
		"repository: ${{ job.workflow_repository }}",
		"ref: ${{ job.workflow_sha }}",
		"persist-credentials: false",
		`go-version: "1.26.5"`,
		"cache: false",
		"go run .libops-workflow/ci/github/homebrew-reconcile/main.go",
		"GH_TOKEN: ${{ github.token }}",
		"HOMEBREW_TOKEN: ${{ secrets.HOMEBREW_REPO }}",
	} {
		requireContains(t, homebrew, required)
	}
	for _, forbidden := range []string{
		"goreleaser/goreleaser-action",
		".goreleaser.yaml",
		"Checkout release source",
		"inputs.release-version || github.ref }}",
		"git clone --depth=1 --branch",
	} {
		if strings.Contains(homebrew, forbidden) {
			t.Errorf("Homebrew recovery must not contain caller build behavior %q", forbidden)
		}
	}
}

func TestRecoveryResolvesExactReusableWorkflowSource(t *testing.T) {
	homebrew := homebrewJob(t)
	checkout := stepBlock(
		t,
		homebrew,
		"      - name: Checkout exact shared reconciliation source\n",
		"      - name: Verify shared reconciliation source\n",
	)
	verify := stepBlock(
		t,
		homebrew,
		"      - name: Verify shared reconciliation source\n",
		"      - name: Set up Go\n",
	)
	for _, required := range []string{
		"repository: ${{ job.workflow_repository }}",
		"ref: ${{ job.workflow_sha }}",
		"persist-credentials: false",
	} {
		requireContains(t, checkout, required)
	}
	for _, required := range []string{
		"EXPECTED_SHA: ${{ job.workflow_sha }}",
		`git -C .libops-workflow rev-parse HEAD`,
		`[[ "$actual_sha" != "$EXPECTED_SHA" ]]`,
	} {
		requireContains(t, verify, required)
	}
	if strings.Contains(homebrew, "${{ github.sha }}") || strings.Contains(homebrew, "${{ github.workflow_sha }}") {
		t.Error("reconciliation source must use the called workflow commit, not the caller commit")
	}
}

func TestReleaseJobsUseLeastPrivilege(t *testing.T) {
	workflow := pluginReleaseWorkflow(t)
	requireContains(t, workflow, "\npermissions: {}\n")

	releasePermissions := stepBlock(t, releaseJob(t), "    permissions:\n", "    steps:\n")
	requireContains(t, releasePermissions, "      contents: write")
	if strings.Contains(releasePermissions, "id-token") || strings.Contains(releasePermissions, "pull-requests") {
		t.Errorf("release job has unrelated permissions:\n%s", releasePermissions)
	}

	homebrewPermissions := stepBlock(t, homebrewJob(t), "    permissions:\n", "    steps:\n")
	requireContains(t, homebrewPermissions, "      contents: read")
	if strings.Contains(homebrewPermissions, "id-token") || strings.Contains(homebrewPermissions, "contents: write") {
		t.Errorf("Homebrew job has unrelated automatic-token permissions:\n%s", homebrewPermissions)
	}

	packagePermissions := stepBlock(t, packageJob(t), "    permissions:\n", "    steps:\n")
	requireContains(t, packagePermissions, "      contents: read")
	requireContains(t, packagePermissions, "      id-token: write")
	if strings.Contains(packagePermissions, "contents: write") || strings.Contains(packagePermissions, "pull-requests") {
		t.Errorf("package job has unrelated permissions:\n%s", packagePermissions)
	}
}

func TestPackagesWaitForFullHomebrewReconciliation(t *testing.T) {
	packages := packageJob(t)
	for _, required := range []string{
		"needs: [validate, release, homebrew]",
		"needs.validate.result == 'success'",
		"inputs.release-mode == 'full'",
		"needs.release.result == 'success'",
		"needs.homebrew.result == 'success'",
		"inputs.release-mode == 'packages-only'",
		"needs.release.result == 'skipped'",
		"needs.homebrew.result == 'skipped'",
	} {
		requireContains(t, packages, required)
	}
}

func TestPostAuthenticationPackageShellUsesValidatedEnvironment(t *testing.T) {
	packages := packageJob(t)
	start := strings.Index(packages, "      - name: Publish Linux package repository\n")
	if start == -1 {
		t.Fatal("package job has no publication step")
	}
	publish := packages[start:]
	for _, required := range []string{
		"PACKAGE_NAME: ${{ inputs.package-name }}",
		"RELEASE_VERSION: ${{ inputs.release-version || github.ref_name }}",
		`PACKAGE_NAME="${PACKAGE_NAME}"`,
		`RELEASE_VERSION="${RELEASE_VERSION}"`,
	} {
		requireContains(t, publish, required)
	}
	run := workflowRunScript(t, publish)
	if strings.Contains(run, "${{ inputs.package-name }}") || strings.Contains(run, "${{ inputs.release-version") {
		t.Error("post-authentication package shell interpolates caller input directly")
	}
}

func TestReleaseRequestValidationIsCredentialFreeAndExecutable(t *testing.T) {
	validation := validationJob(t)
	permissions := stepBlock(t, validation, "    permissions: {}\n", "    steps:\n")
	requireContains(t, permissions, "permissions: {}")
	for _, forbidden := range []string{"github.token", "secrets.", "GH_TOKEN", "id-token", "contents:"} {
		if strings.Contains(validation, forbidden) {
			t.Errorf("validation job contains credential-bearing behavior %q", forbidden)
		}
	}
	step := stepBlock(
		t,
		pluginReleaseWorkflow(t),
		"      - name: Validate release request\n",
		"  release:\n",
	)
	script := filepath.Join(t.TempDir(), "validate-request.sh")
	if err := os.WriteFile(script, []byte(workflowRunScript(t, step)), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(mode, packageName, repository, publishPackages string) (string, error) {
		command := exec.Command("bash", script) // #nosec G204 -- fixed workflow script.
		command.Env = append(os.Environ(),
			"GITHUB_REPOSITORY="+repository,
			"PACKAGE_NAME="+packageName,
			"PUBLISH_PACKAGE_REPO="+publishPackages,
			"RELEASE_MODE="+mode,
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	for _, mode := range []string{"full", "homebrew-only", "packages-only"} {
		if output, err := run(mode, "sitectl-ojs", "libops/sitectl-ojs", "true"); err != nil {
			t.Fatalf("valid %s request failed: %v\n%s", mode, err, output)
		}
	}
	for name, test := range map[string][4]string{
		"unknown mode":        {"other", "sitectl-ojs", "libops/sitectl-ojs", "true"},
		"invalid package":     {"full", "Sitectl-OJS", "libops/Sitectl-OJS", "true"},
		"repeated hyphen":     {"full", "sitectl-ojs--test", "libops/sitectl-ojs--test", "true"},
		"trailing hyphen":     {"full", "sitectl-ojs-", "libops/sitectl-ojs-", "true"},
		"repository mismatch": {"full", "sitectl-ojs", "libops/sitectl-drupal", "true"},
		"external owner":      {"full", "sitectl-ojs", "someone/sitectl-ojs", "true"},
		"disabled no-op":      {"packages-only", "sitectl-ojs", "libops/sitectl-ojs", "false"},
	} {
		t.Run(name, func(t *testing.T) {
			if output, err := run(test[0], test[1], test[2], test[3]); err == nil {
				t.Fatalf("invalid request passed:\n%s", output)
			}
		})
	}
}

func TestRecoveryCredentialAndConcurrencyContract(t *testing.T) {
	workflow := pluginReleaseWorkflow(t)
	secrets := stepBlock(t, workflow, "    secrets:\n", "\npermissions: {}\n")
	for _, required := range []string{
		"      HOMEBREW_REPO:",
		"required: true",
		"contents and pull-request access to libops/homebrew",
	} {
		requireContains(t, secrets, required)
	}
	requireContains(
		t,
		workflow,
		"group: sitectl-plugin-release-${{ github.repository }}-${{ inputs.release-version || github.ref_name }}",
	)
}

func TestFullReleaseValidationIsExecutable(t *testing.T) {
	step := stepBlock(
		t,
		releaseJob(t),
		"      - name: Validate full release\n",
		"      - name: Checkout release source\n",
	)
	script := filepath.Join(t.TempDir(), "validate-full.sh")
	if err := os.WriteFile(script, []byte(workflowRunScript(t, step)), 0700); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	fakeGH := `#!/usr/bin/env bash
set -euo pipefail
if [[ "${RELEASE_EXISTS:-false}" == "true" ]]; then exit 0; fi
exit 1
`
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte(fakeGH), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(refType, refName, version, exists string) (string, error) {
		command := exec.Command("bash", script) // #nosec G204 -- fixed workflow script.
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GH_TOKEN=test",
			"GITHUB_REF_NAME="+refName,
			"GITHUB_REPOSITORY=libops/sitectl-ojs",
			"REF_TYPE="+refType,
			"RELEASE_EXISTS="+exists,
			"RELEASE_VERSION="+version,
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if output, err := run("tag", "v1.0.0", "", "false"); err != nil {
		t.Fatalf("valid full release failed: %v\n%s", err, output)
	}
	for name, test := range map[string][4]string{
		"branch":        {"branch", "v1.0.0", "", "false"},
		"leading zero":  {"tag", "v01.0.0", "", "false"},
		"version input": {"tag", "v1.0.0", "v1.0.0", "false"},
		"existing":      {"tag", "v1.0.0", "", "true"},
	} {
		t.Run(name, func(t *testing.T) {
			output, err := run(test[0], test[1], test[2], test[3])
			if err == nil {
				t.Fatalf("invalid full release passed:\n%s", output)
			}
		})
	}
}

func TestPackageRecoveryRequiresDefaultBranch(t *testing.T) {
	step := stepBlock(
		t,
		packageJob(t),
		"      - name: Validate package publication\n",
		"      - name: Authenticate to Google Cloud\n",
	)
	script := filepath.Join(t.TempDir(), "validate-packages.sh")
	if err := os.WriteFile(script, []byte(workflowRunScript(t, step)), 0700); err != nil {
		t.Fatal(err)
	}
	fakeBin := filepath.Join(t.TempDir(), "bin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	fakeGH := `#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
  api) printf '%s\n' main ;;
  release) printf '%s\n' false ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte(fakeGH), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(refType, refName string) (string, error) {
		command := exec.Command("bash", script) // #nosec G204 -- fixed workflow script.
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GH_TOKEN=test",
			"GITHUB_REPOSITORY=libops/sitectl-ojs",
			"REF_NAME="+refName,
			"REF_TYPE="+refType,
			"RELEASE_MODE=packages-only",
			"RELEASE_VERSION=v1.0.0",
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	if output, err := run("branch", "main"); err != nil {
		t.Fatalf("default-branch package recovery failed: %v\n%s", err, output)
	}
	for _, test := range [][2]string{{"branch", "feature"}, {"tag", "main"}} {
		if output, err := run(test[0], test[1]); err == nil || !strings.Contains(output, "must run from the repository default branch") {
			t.Fatalf("invalid package recovery was accepted: %v\n%s", err, output)
		}
	}
}

func TestActionlintRunsAllGoContracts(t *testing.T) {
	actionlint := githubReadFile(t, ".github/workflows/actionlint.yaml")
	if count := strings.Count(actionlint, `      - "ci/github/**"`); count != 2 {
		t.Errorf("actionlint path filters contain ci/github/** %d times, want pull_request and push", count)
	}
	if count := strings.Count(actionlint, `      - "go.mod"`); count != 2 {
		t.Errorf("actionlint path filters contain go.mod %d times, want pull_request and push", count)
	}
	if count := strings.Count(actionlint, `      - ".github/actionlint.yaml"`); count != 2 {
		t.Errorf("actionlint path filters contain .github/actionlint.yaml %d times, want pull_request and push", count)
	}
	actionlintConfig := githubReadFile(t, ".github/actionlint.yaml")
	requireContains(t, actionlintConfig, `property "workflow_(repository|sha)" is not defined in object type`)
	checkout := stepBlock(t, actionlint, "      - name: Checkout\n", "      - name: Set up Go\n")
	requireContains(t, checkout, "persist-credentials: false")
	requireContains(t, actionlint, "go test ./ci/github/...")
}
