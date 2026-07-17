package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const linuxPackagePublisherCommit = "b14fd2f95b4017e897c595b3321b9cc3f48b5ddd"

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
		"version: ${{ steps.validate_full.outputs.version }}",
		"ref: refs/tags/${{ steps.validate_full.outputs.version }}",
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
		"RELEASE_VERSION: ${{ inputs.release-mode == 'full' && needs.release.outputs.version || inputs.release-version }}",
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
		"RELEASE_VERSION: ${{ inputs.release-mode == 'full' && needs.release.outputs.version || inputs.release-version }}",
		"APTLY_LABEL: sitectl",
		"APTLY_PUBLIC_KEY_NAME: sitectl-archive-keyring",
		`EXCLUDED_PACKAGE_NAMES: ""`,
		"GCS_BUCKET_PREFIX: sitectl",
		"PACKAGE_TOOLS_IMAGE: ${{ steps.package-tools.outputs.image }}",
		"EXPECTED_PACKAGE_TOOLS_IMAGE_ID: ${{ steps.package-tools.outputs.image-id }}",
		"PACKAGE_PUBLISHER_SHA: " + linuxPackagePublisherCommit,
		"run: bash scripts/publish-release-from-environment.sh",
	} {
		requireContains(t, publish, required)
	}
	runStart := strings.Index(publish, "        run: ")
	if runStart == -1 {
		t.Fatal("package publication step has no direct run command")
	}
	run := publish[runStart:]
	if strings.Contains(run, "${{ inputs.package-name }}") || strings.Contains(run, "${{ inputs.release-version") {
		t.Error("post-authentication package shell interpolates caller input directly")
	}
	if strings.Contains(run, "make package") {
		t.Error("credentialed package publication must bypass GNU Make")
	}
}

func TestPackagePublisherIsPinnedAndPreparedBeforeAuthentication(t *testing.T) {
	packages := packageJob(t)
	checkout := stepBlock(
		t,
		packages,
		"      - name: Checkout exact package publisher\n",
		"      - name: Verify exact package publisher\n",
	)
	verify := stepBlock(
		t,
		packages,
		"      - name: Verify exact package publisher\n",
		"      - name: Validate mandatory Linux package exclusions\n",
	)
	exclusions := stepBlock(
		t,
		packages,
		"      - name: Validate mandatory Linux package exclusions\n",
		"      - name: Build exact package tools image\n",
	)
	build := stepBlock(
		t,
		packages,
		"      - name: Build exact package tools image\n",
		"      - name: Authenticate to Google Cloud\n",
	)

	for _, required := range []string{
		"repository: libops/terraform-linux-packages",
		"ref: " + linuxPackagePublisherCommit,
		"persist-credentials: false",
	} {
		requireContains(t, checkout, required)
	}
	for _, required := range []string{
		"PACKAGE_PUBLISHER_SHA: " + linuxPackagePublisherCommit,
		`actual_sha="$(git rev-parse HEAD)"`,
		`[[ "$actual_sha" != "$PACKAGE_PUBLISHER_SHA" ]]`,
	} {
		requireContains(t, verify, required)
	}
	for _, required := range []string{
		"PACKAGE_NAME: ${{ inputs.package-name }}",
		`EXCLUDED_PACKAGE_NAMES: ""`,
		"run: bash scripts/validate-package-exclusions.sh",
	} {
		requireContains(t, exclusions, required)
	}
	for _, required := range []string{
		"PACKAGE_PUBLISHER_SHA: " + linuxPackagePublisherCommit,
		`image="libops/terraform-linux-packages:publisher-${PACKAGE_PUBLISHER_SHA}"`,
		`docker build --tag "$image" .`,
		`docker image inspect --format '{{.Id}}' "$image"`,
		`[[ ! "$image_id" =~ ^sha256:[0-9a-f]{64}$ ]]`,
		`echo "image-id=$image_id"`,
	} {
		requireContains(t, build, required)
	}

	workflow := pluginReleaseWorkflow(t)
	if strings.Contains(workflow, "excluded-package-names:") ||
		strings.Contains(packages, "EXCLUDED_PACKAGE_NAMES: ${{") {
		t.Error("plugin callers must not be able to add Linux package exclusions")
	}
	for _, forbidden := range []string{
		"ref: main",
		"terraform-linux-packages:main",
		"docker pull",
	} {
		if strings.Contains(packages, forbidden) {
			t.Errorf("package publication contains mutable publisher behavior %q", forbidden)
		}
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
	release := releaseJob(t)
	step := stepBlock(
		t,
		release,
		"      - name: Validate full release\n",
		"      - name: Checkout release source\n",
	)
	checkout := stepBlock(
		t,
		release,
		"      - name: Checkout release source\n",
		"      - name: Checkout sitectl SDK\n",
	)
	requireContains(t, checkout, "ref: refs/tags/${{ steps.validate_full.outputs.version }}")
	requireContains(t, checkout, "persist-credentials: false")

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
case "$*" in
  *"/git/ref/tags/"*)
    if [[ "${TAG_EXISTS:-true}" != "true" ]]; then exit 1; fi
    printf 'refs/tags/%s\n' "${EXPECTED_VERSION:-v1.0.0}"
    ;;
  *"/commits/"*) printf '%s\n' "${EXPECTED_COMMIT}" ;;
  *"/releases?per_page=100"*)
    case "${RELEASE_STATE:-absent}" in
      absent) ;;
      empty) printf '%s\tfalse\tfalse\ttrue\tfalse\t0\n' "${EXPECTED_VERSION:-v1.0.0}" ;;
      assets) printf '%s\tfalse\tfalse\ttrue\tfalse\t1\n' "${EXPECTED_VERSION:-v1.0.0}" ;;
      draft) printf '%s\ttrue\tfalse\tfalse\tfalse\t0\n' "${EXPECTED_VERSION:-v1.0.0}" ;;
      prerelease) printf '%s\tfalse\ttrue\ttrue\tfalse\t0\n' "${EXPECTED_VERSION:-v1.0.0}" ;;
      unpublished) printf '%s\tfalse\tfalse\tfalse\tfalse\t0\n' "${EXPECTED_VERSION:-v1.0.0}" ;;
      immutable) printf '%s\tfalse\tfalse\ttrue\ttrue\t0\n' "${EXPECTED_VERSION:-v1.0.0}" ;;
      mismatch) printf 'v2.0.0\tfalse\tfalse\ttrue\tfalse\t0\n' ;;
      api-error) exit 1 ;;
      *) exit 2 ;;
    esac
    ;;
  *"repos/libops/sitectl-ojs --jq .default_branch"*) printf '%s\n' main ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte(fakeGH), 0700); err != nil {
		t.Fatal(err)
	}
	outputFile := filepath.Join(t.TempDir(), "github-output")
	run := func(refType, refName, version, releaseState, tagExists string) (string, error) {
		if err := os.WriteFile(outputFile, nil, 0600); err != nil {
			t.Fatal(err)
		}
		command := exec.Command("bash", script) // #nosec G204 -- fixed workflow script.
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"EXPECTED_COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"EXPECTED_VERSION=v1.0.0",
			"GH_TOKEN=test",
			"GITHUB_OUTPUT="+outputFile,
			"GITHUB_REPOSITORY=libops/sitectl-ojs",
			"REF_NAME="+refName,
			"REF_TYPE="+refType,
			"RELEASE_STATE="+releaseState,
			"RELEASE_VERSION="+version,
			"TAG_EXISTS="+tagExists,
		)
		output, err := command.CombinedOutput()
		written, readErr := os.ReadFile(outputFile)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(output) + string(written), err
	}
	for name, test := range map[string][5]string{
		"new tag":                 {"tag", "v1.0.0", "", "absent", "true"},
		"empty release":           {"tag", "v1.0.0", "", "empty", "true"},
		"default branch recovery": {"branch", "main", "v1.0.0", "empty", "true"},
	} {
		t.Run(name, func(t *testing.T) {
			output, err := run(test[0], test[1], test[2], test[3], test[4])
			if err != nil {
				t.Fatalf("valid full release failed: %v\n%s", err, output)
			}
			if !strings.Contains(output, "version=v1.0.0") {
				t.Fatalf("validated version output missing:\n%s", output)
			}
			if !strings.Contains(output, "tag_commit=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa") {
				t.Fatalf("validated tag commit output missing:\n%s", output)
			}
		})
	}
	for name, test := range map[string][5]string{
		"branch without version":  {"branch", "main", "", "absent", "true"},
		"feature branch":          {"branch", "feature", "v1.0.0", "absent", "true"},
		"leading zero":            {"tag", "v01.0.0", "", "absent", "true"},
		"version input on tag":    {"tag", "v1.0.0", "v1.0.0", "absent", "true"},
		"missing tag":             {"branch", "main", "v1.0.0", "absent", "false"},
		"absent recovery release": {"branch", "main", "v1.0.0", "absent", "true"},
		"release with assets":     {"branch", "main", "v1.0.0", "assets", "true"},
		"draft release":           {"branch", "main", "v1.0.0", "draft", "true"},
		"prerelease":              {"branch", "main", "v1.0.0", "prerelease", "true"},
		"unpublished release":     {"branch", "main", "v1.0.0", "unpublished", "true"},
		"immutable release":       {"branch", "main", "v1.0.0", "immutable", "true"},
		"mismatched release":      {"branch", "main", "v1.0.0", "mismatch", "true"},
		"release API failure":     {"branch", "main", "v1.0.0", "api-error", "true"},
	} {
		t.Run(name, func(t *testing.T) {
			output, err := run(test[0], test[1], test[2], test[3], test[4])
			if err == nil {
				t.Fatalf("invalid full release passed:\n%s", output)
			}
		})
	}
}

func TestExactReleaseSourceValidationIsExecutable(t *testing.T) {
	step := stepBlock(
		t,
		releaseJob(t),
		"      - name: Verify exact release source\n",
		"      - name: Checkout sitectl SDK\n",
	)
	script := filepath.Join(t.TempDir(), "verify-source.sh")
	if err := os.WriteFile(script, []byte(workflowRunScript(t, step)), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(tagType, state string) (string, error) {
		repository := t.TempDir()
		runContractGit(t, repository, "init", "--quiet", "--initial-branch=main")
		runContractGit(t, repository, "config", "user.name", "Contract Test")
		runContractGit(t, repository, "config", "user.email", "contract@example.test")
		runContractGit(t, repository, "commit", "--quiet", "--allow-empty", "-m", "main")
		mainCommit := strings.TrimSpace(runContractGit(t, repository, "rev-parse", "HEAD"))
		switch state {
		case "valid":
			if tagType == "annotated" {
				runContractGit(t, repository, "tag", "-a", "v1.0.0", "-m", "v1.0.0")
			} else {
				runContractGit(t, repository, "tag", "v1.0.0")
			}
			runContractGit(t, repository, "update-ref", "refs/remotes/origin/main", mainCommit)
			runContractGit(t, repository, "checkout", "--quiet", "--detach", "v1.0.0")
		case "head-mismatch":
			runContractGit(t, repository, "tag", "v1.0.0")
			runContractGit(t, repository, "commit", "--quiet", "--allow-empty", "-m", "later")
			laterCommit := strings.TrimSpace(runContractGit(t, repository, "rev-parse", "HEAD"))
			runContractGit(t, repository, "update-ref", "refs/remotes/origin/main", laterCommit)
		case "unreachable":
			runContractGit(t, repository, "update-ref", "refs/remotes/origin/main", mainCommit)
			runContractGit(t, repository, "checkout", "--quiet", "--orphan", "side")
			runContractGit(t, repository, "commit", "--quiet", "--allow-empty", "-m", "side")
			runContractGit(t, repository, "tag", "v1.0.0")
			mainCommit = strings.TrimSpace(runContractGit(t, repository, "rev-parse", "HEAD"))
		default:
			t.Fatalf("unknown source state %q", state)
		}
		command := exec.Command("bash", script) // #nosec G204 -- fixed workflow script.
		command.Dir = repository
		command.Env = append(os.Environ(),
			"DEFAULT_BRANCH=main",
			"EXPECTED_COMMIT="+mainCommit,
			"EXPECTED_VERSION=v1.0.0",
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	for _, tagType := range []string{"lightweight", "annotated"} {
		t.Run(tagType, func(t *testing.T) {
			if output, err := run(tagType, "valid"); err != nil {
				t.Fatalf("valid %s tag failed source verification: %v\n%s", tagType, err, output)
			}
		})
	}
	for _, state := range []string{"head-mismatch", "unreachable"} {
		t.Run(state, func(t *testing.T) {
			if output, err := run("lightweight", state); err == nil {
				t.Fatalf("invalid source state passed verification:\n%s", output)
			}
		})
	}
}

func TestSitectlSDKCheckoutRequiresPinnedCommit(t *testing.T) {
	const sitectlV1Commit = "65cfde137a58ba14aaa9a1512d88b943888872f3"
	workflow := pluginReleaseWorkflow(t)
	requireContains(t, workflow, `default: "`+sitectlV1Commit+`"`)
	sdk := stepBlock(
		t,
		releaseJob(t),
		"      - name: Checkout sitectl SDK\n",
		"      - name: Set up Go\n",
	)
	for _, required := range []string{
		`[[ ! "$SITECTL_REF" =~ ^[0-9a-f]{40}$ ]]`,
		`git -C ../sitectl fetch --quiet --depth=1 origin "$SITECTL_REF"`,
		`git -C ../sitectl checkout --quiet --detach FETCH_HEAD`,
		`[[ "$actual_sitectl_sha" != "$SITECTL_REF" ]]`,
	} {
		requireContains(t, sdk, required)
	}
	if strings.Contains(sdk, "git clone") || strings.Contains(sdk, `default: "main"`) {
		t.Error("release SDK checkout must not resolve a movable branch")
	}

	readme := githubReadFile(t, "README.md")
	requireContains(t, readme, "release-mode: ${{ github.ref_type == 'tag' && 'full' || inputs.release-mode }}")
	requireContains(t, readme, "sitectl-ref: "+sitectlV1Commit)
}

func TestPackageRecoveryRequiresDefaultBranch(t *testing.T) {
	step := stepBlock(
		t,
		packageJob(t),
		"      - name: Validate package publication\n",
		"      - name: Checkout exact package publisher\n",
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
  release) printf 'v1.0.0\tfalse\tfalse\ttrue\n' ;;
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
			"REQUESTED_RELEASE_VERSION=v1.0.0",
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

func TestFullPackagePublicationSupportsTagAndDefaultBranchRecovery(t *testing.T) {
	step := stepBlock(
		t,
		packageJob(t),
		"      - name: Validate package publication\n",
		"      - name: Checkout exact package publisher\n",
	)
	script := filepath.Join(t.TempDir(), "validate-full-packages.sh")
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
  release) printf '%s\tfalse\tfalse\ttrue\n' "${PUBLISHED_VERSION:-v1.0.0}" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "gh"), []byte(fakeGH), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(refType, refName, requested, resolved string) (string, error) {
		command := exec.Command("bash", script) // #nosec G204 -- fixed workflow script.
		command.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"GH_TOKEN=test",
			"GITHUB_REPOSITORY=libops/sitectl-ojs",
			"REF_NAME="+refName,
			"REF_TYPE="+refType,
			"RELEASE_MODE=full",
			"RELEASE_VERSION="+resolved,
			"REQUESTED_RELEASE_VERSION="+requested,
		)
		output, err := command.CombinedOutput()
		return string(output), err
	}
	for name, test := range map[string][4]string{
		"tag":                     {"tag", "v1.0.0", "", "v1.0.0"},
		"default branch recovery": {"branch", "main", "v1.0.0", "v1.0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			if output, err := run(test[0], test[1], test[2], test[3]); err != nil {
				t.Fatalf("valid full package publication failed: %v\n%s", err, output)
			}
		})
	}
	for name, test := range map[string][4]string{
		"tag requested version": {"tag", "v1.0.0", "v1.0.0", "v1.0.0"},
		"mismatched tag":        {"tag", "v2.0.0", "", "v1.0.0"},
		"feature recovery":      {"branch", "feature", "v1.0.0", "v1.0.0"},
		"mismatched recovery":   {"branch", "main", "v2.0.0", "v1.0.0"},
	} {
		t.Run(name, func(t *testing.T) {
			if output, err := run(test[0], test[1], test[2], test[3]); err == nil {
				t.Fatalf("invalid full package publication passed:\n%s", output)
			}
		})
	}
}

func runContractGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	command := exec.Command("git", args...) // #nosec G204 -- fixed test arguments.
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
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
