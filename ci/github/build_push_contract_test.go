package github

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func workflowSource(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve contract test path")
	}
	workflowPath := filepath.Join(filepath.Dir(sourceFile), "..", "..", ".github", "workflows", "build-push.yaml")
	contents, err := os.ReadFile(workflowPath)
	if err != nil {
		t.Fatalf("read build-push workflow: %v", err)
	}
	return string(contents)
}

func stepBlock(t *testing.T, workflow, start, end string) string {
	t.Helper()
	startIndex := strings.Index(workflow, start)
	if startIndex == -1 {
		t.Fatalf("workflow is missing %q", start)
	}
	endIndex := strings.Index(workflow[startIndex+len(start):], end)
	if endIndex == -1 {
		t.Fatalf("workflow is missing %q after %q", end, start)
	}
	return workflow[startIndex : startIndex+len(start)+endIndex]
}

func requireContains(t *testing.T, source, value string) {
	t.Helper()
	if !strings.Contains(source, value) {
		t.Errorf("contract is missing %q", value)
	}
}

func workflowRunScript(t *testing.T, step string) string {
	t.Helper()
	const marker = "        run: |\n"
	start := strings.Index(step, marker)
	if start == -1 {
		t.Fatal("workflow step has no run script")
	}
	lines := strings.Split(step[start+len(marker):], "\n")
	for index, line := range lines {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "          ") {
			t.Fatalf("workflow run line is not indented as a script: %q", line)
		}
		lines[index] = strings.TrimPrefix(line, "          ")
	}
	return strings.TrimSpace(strings.Join(lines, "\n")) + "\n"
}

func artifactKey(repository, image, tag, aliases, runID string) string {
	value := strings.Join([]string{repository, image, tag, aliases, runID}, "\n") + "\n"
	return fmt.Sprintf("%x", sha256.Sum256([]byte(value)))[:24]
}

func TestGuardedBuildCannotWriteRegistryCache(t *testing.T) {
	workflow := workflowSource(t)
	direct := stepBlock(t, workflow, "      - name: build+push\n", "      - name: Build native image for verified publication\n")
	verified := stepBlock(t, workflow, "      - name: Build native image for verified publication\n", "      - name: Scan exact native image\n")

	requireContains(t, direct, "!inputs.scan && inputs.additional-gar-registry == '' && inputs.expected-main-sha == ''")
	requireContains(t, direct, "cache-from: ${{ steps.vars.outputs.cache-from }}")
	requireContains(t, direct, "cache-to: ${{ steps.vars.outputs.cache-to }}")
	requireContains(t, direct, "provenance: false")
	requireContains(t, verified, "cache-from: ${{ steps.vars.outputs.cache-from }}")
	requireContains(t, verified, "load: true")
	requireContains(t, verified, "push: false")
	if strings.Contains(verified, "cache-to:") {
		t.Error("verified local build must not export a registry cache before its scan or main-tip gate")
	}
}

func TestReusableSecretContractIsExplicitAndOptional(t *testing.T) {
	workflow := workflowSource(t)
	secrets := stepBlock(t, workflow, "    secrets:\n", "jobs:\n")

	for _, name := range []string{"GCLOUD_OIDC_POOL", "GSA", "DOCKERHUB_USER", "DOCKERHUB_PASSWORD"} {
		secret := stepBlock(t, secrets, "      "+name+":\n", "        description:")
		requireContains(t, secret, "required: false")
	}
}

func TestPublisherRunnersCannotBeCallerControlled(t *testing.T) {
	workflow := workflowSource(t)
	buildHeader := stepBlock(t, workflow, "jobs:\n  build:\n", "    outputs:\n")
	merge := stepBlock(t, workflow, "      - name: merge platform images\n", "      - name: Verify final manifest parity\n")

	requireContains(t, buildHeader, "runner:\n          - ubuntu-24.04\n          - ubuntu-24.04-arm")
	requireContains(t, buildHeader, "runs-on: ${{ matrix.runner }}")
	requireContains(t, workflow, `. == ["ubuntu-24.04", "ubuntu-24.04-arm"]`)
	requireContains(t, merge, "for platform in amd64 arm64; do")
	if strings.Contains(workflow, "fromJSON(inputs.runners)") {
		t.Error("caller-controlled runners input must not construct the publisher matrix")
	}
	if strings.Contains(merge, "inputs.runners") || strings.Contains(merge, "RUNNERS") {
		t.Error("merge platform selection must not derive from the compatibility runners input")
	}
}

func TestDefaultImageNameReceivesCanonicalValidation(t *testing.T) {
	workflow := workflowSource(t)
	validation := stepBlock(t, workflow, "      - name: validate input\n", "      - uses: actions/checkout@")
	derive := `canonical_image_name="${GITHUB_REPOSITORY#*/}"`
	validate := `[[ "$canonical_image_name" =~ $image_regex ]]`

	requireContains(t, validation, derive)
	requireContains(t, validation, validate)
	if strings.Index(validation, derive) > strings.Index(validation, validate) {
		t.Error("the repository-derived default image name must be resolved before validation")
	}
}

func TestExpectedMainGuardsEveryRegistryWrite(t *testing.T) {
	workflow := workflowSource(t)
	push := stepBlock(t, workflow, "      - name: Push verified native image\n", "      - name: Preserve platform digests\n")
	merge := stepBlock(t, workflow, "      - name: merge platform images\n", "      - name: Verify final manifest parity\n")
	sign := workflow[strings.Index(workflow, "      - name: Sign and verify final manifests\n"):]

	requireContains(t, push, "require_current_main\n            docker push \"$target\"")
	guardedCreate := regexp.MustCompile(`require_current_main\s+docker buildx imagetools create`)
	if got := len(guardedCreate.FindAllString(merge, -1)); got != 1 {
		t.Errorf("want the manifest helper to check current main immediately before its only registry write, got %d guarded writes", got)
	}
	requireContains(t, sign, "require_current_main\n            cosign sign --yes")
}

func TestDigestArtifactsRepairMixedAttempts(t *testing.T) {
	workflow := workflowSource(t)
	buildMetadata := stepBlock(t, workflow, "      - name: Resolve publication metadata\n", "      - name: GHCR Login\n")

	if got := strings.Count(workflow, `"$ADDITIONAL_IMAGE_NAMES" "$GITHUB_RUN_ID" |`); got != 3 {
		t.Errorf("want build, merge, and cleanup keys stable per workflow run, got %d stable definitions", got)
	}
	if strings.Contains(workflow, `"$GITHUB_RUN_ID" "$GITHUB_RUN_ATTEMPT"`) {
		t.Error("artifact key must not vary across failed-job rerun attempts")
	}
	if first, second := artifactKey("libops/api", "ghcr.io/libops/api", "main", `["dash"]`, "123"), artifactKey("libops/api", "ghcr.io/libops/api", "main", `["dash"]`, "123"); first != second {
		t.Fatalf("same workflow run produced different artifact keys: %s != %s", first, second)
	}
	if got := strings.Count(buildMetadata, "ci-${artifact_key}-${GITHUB_RUN_ATTEMPT}-${PLATFORM}"); got != 1 {
		t.Errorf("want the run attempt isolated in the temporary registry tag, got %d definitions", got)
	}
	if got := strings.Count(buildMetadata, "GITHUB_RUN_ATTEMPT"); got != 1 {
		t.Errorf("build metadata must use the run attempt only in temporary registry tags: got %d references", got)
	}

	upload := stepBlock(t, workflow, "      - name: Preserve platform digests\n", "      - name: record image tag\n")
	requireContains(t, upload, "overwrite: true")
	if strings.Contains(upload, "        if:") {
		t.Error("every publication path must preserve its digest artifact")
	}
	download := stepBlock(t, workflow, "      - name: Restore platform digests\n", "      - name: merge platform images\n")
	requireContains(t, download, "pattern: ${{ steps.metadata.outputs.artifact-name }}-*")
	requireContains(t, download, "merge-multiple: true")
	requireContains(t, workflow, "sources+=(\"${target_image}@${digest}\")")
}

func TestStagingCleanupRetainsRepairRefsAndDeletesOnlyGARTags(t *testing.T) {
	workflow := workflowSource(t)
	cleanupStart := strings.Index(workflow, "  cleanup:\n")
	if cleanupStart == -1 {
		t.Fatal("workflow is missing the staging cleanup job")
	}
	cleanup := workflow[cleanupStart:]
	gar := stepBlock(t, cleanup, "      - name: Remove exact GAR staging tags after verified merge\n", "      - name: Report retained staging tags\n")

	requireContains(t, cleanup, "needs:\n      - build\n      - merge")
	requireContains(t, cleanup, "if: ${{ always() }}")
	requireContains(t, cleanup, "continue-on-error: true")
	requireContains(t, cleanup, "Publication did not merge successfully; retaining all exact ci-* tags")
	requireContains(t, cleanup, "GHCR has no conditional tag-only delete")
	requireContains(t, gar, "needs.merge.result == 'success'")
	requireContains(t, cleanup, `for attempt in $(seq 1 "$GITHUB_RUN_ATTEMPT"); do`)

	// GAR exposes a true tag resource. Delete that exact resource and never a
	// package, image, version, digest, or manifest.
	requireContains(t, gar, `/packages/${encoded_package}/tags/${staging_tag}`)
	requireContains(t, gar, `GAR tag cleanup lacks artifactregistry.tags.delete`)
	for _, unsafe := range []string{
		"/versions/",
		"gh api --method DELETE",
		"docker push",
		"docker images delete",
		"manifests/sha256:",
		"--delete-tags",
		"crane delete",
		"regctl manifest delete",
		"docker buildx imagetools rm",
	} {
		if strings.Contains(cleanup, unsafe) {
			t.Errorf("staging cleanup contains unsafe deletion primitive %q", unsafe)
		}
	}
	if got := strings.Count(gar, "--request DELETE"); got != 1 {
		t.Fatalf("GAR cleanup DELETE primitives = %d, want one exact tag-resource call", got)
	}
}

func TestGARTagCleanupExecutesExactEncodedTagDeletes(t *testing.T) {
	workflow := workflowSource(t)
	cleanup := workflow[strings.Index(workflow, "  cleanup:\n"):]
	gar := stepBlock(t, cleanup, "      - name: Remove exact GAR staging tags after verified merge\n", "      - name: Report retained staging tags\n")
	scriptPath := filepath.Join(t.TempDir(), "cleanup-gar.sh")
	if err := os.WriteFile(scriptPath, []byte(workflowRunScript(t, gar)), 0700); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	curlLog := filepath.Join(t.TempDir(), "curl.log")
	fakeCurl := `#!/usr/bin/env bash
set -euo pipefail
output=""
previous=""
for argument in "$@"; do
  if [ "$previous" = --output ]; then output="$argument"; fi
  previous="$argument"
done
if [ -n "$output" ]; then printf '{}\n' > "$output"; fi
printf '%s\n' "$*" >> "$FAKE_CURL_LOG"
printf '%s' "${FAKE_CURL_STATUS:-200}"
`
	if err := os.WriteFile(filepath.Join(fakeBin, "curl"), []byte(fakeCurl), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(token, status string) string {
		t.Helper()
		if err := os.WriteFile(curlLog, nil, 0600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", scriptPath) // #nosec G204 -- fixed test script with a test-owned curl fixture.
		cmd.Env = append(os.Environ(),
			"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
			"ACCESS_TOKEN="+token,
			"ADDITIONAL_GAR_REGISTRY=us-docker.pkg.dev/example-project/public",
			`ADDITIONAL_IMAGE_NAMES=["nested/alias"]`,
			"ARTIFACT_KEY=0123456789abcdef01234567",
			"IMAGE_NAME=api",
			"PRIMARY_IMAGE=ghcr.io/libops/api",
			"PRIMARY_KIND=ghcr",
			"PRIMARY_REGISTRY=ghcr.io/libops",
			"GITHUB_REPOSITORY=libops/api",
			"GITHUB_RUN_ATTEMPT=2",
			"ARTIFACT_REGISTRY_API_ROOT=https://artifact.test/v1",
			"FAKE_CURL_LOG="+curlLog,
			"FAKE_CURL_STATUS="+status,
		)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("GAR cleanup failed: %v\n%s", err, output)
		}
		data, err := os.ReadFile(curlLog)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	if log := run("", "200"); log != "" {
		t.Fatalf("unauthenticated cleanup made registry calls:\n%s", log)
	}
	log := run("test-token", "404")
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 8 {
		t.Fatalf("GAR cleanup calls = %d, want 8 exact tags:\n%s", len(lines), log)
	}
	for _, required := range []string{
		"--request DELETE",
		"https://artifact.test/v1/projects/example-project/locations/us/repositories/public/packages/api/tags/ci-0123456789abcdef01234567-1-amd64",
		"https://artifact.test/v1/projects/example-project/locations/us/repositories/public/packages/nested%2Falias/tags/ci-0123456789abcdef01234567-2-arm64",
	} {
		if !strings.Contains(log, required) {
			t.Errorf("GAR cleanup log missing %q:\n%s", required, log)
		}
	}
}

func TestAliasesFanOutOneVerifiedBuild(t *testing.T) {
	workflow := workflowSource(t)
	inputs := stepBlock(t, workflow, "      additional-image-names:\n", "      scan:\n")
	verified := stepBlock(t, workflow, "      - name: Build native image for verified publication\n", "      - name: Scan exact native image\n")
	push := stepBlock(t, workflow, "      - name: Push verified native image\n", "      - name: Preserve platform digests\n")
	merge := stepBlock(t, workflow, "      - name: merge platform images\n", "      - name: Verify final manifest parity\n")
	parity := stepBlock(t, workflow, "      - name: Verify final manifest parity\n", "      - name: Sign and verify final manifests\n")
	sign := workflow[strings.Index(workflow, "      - name: Sign and verify final manifests\n"):]

	requireContains(t, inputs, `default: "[]"`)
	requireContains(t, workflow, `type == "array" and length <= 8`)
	requireContains(t, verified, "steps.registry.outputs.has-aliases == 'true'")
	requireContains(t, push, `"${PRIMARY_REGISTRY}/${alias_name}:${TEMPORARY_TAG}"`)
	requireContains(t, push, `"${ADDITIONAL_GAR_REGISTRY}/${alias_name}:${TEMPORARY_TAG}"`)
	requireContains(t, merge, `"primary-alias-${alias_index}"`)
	requireContains(t, merge, `"additional-alias-${alias_index}"`)
	requireContains(t, parity, `require_manifest_parity "${PRIMARY_REGISTRY}/${alias_name}"`)
	requireContains(t, parity, `require_manifest_parity "${ADDITIONAL_GAR_REGISTRY}/${alias_name}"`)
	requireContains(t, sign, `sign_and_verify "${PRIMARY_REGISTRY}/${alias_name}"`)
	requireContains(t, sign, `sign_and_verify "${ADDITIONAL_GAR_REGISTRY}/${alias_name}"`)
}

func TestSigningBindsBuilderAndExactCaller(t *testing.T) {
	workflow := workflowSource(t)
	inputs := stepBlock(t, workflow, "      sign:\n", "      workload-identity-provider:\n")
	sign := workflow[strings.Index(workflow, "      - name: Sign and verify final manifests\n"):]

	requireContains(t, inputs, "default: false")
	requireContains(t, inputs, "certificate-identity:")
	requireContains(t, workflow, "sigstore/cosign-installer@6f9f17788090df1f26f669e9d70d6ae9567deba6 # v4.1.2")
	requireContains(t, workflow, "build-push\\.ya?ml@[0-9a-fA-F]{40}")
	requireContains(t, sign, "docker buildx imagetools inspect \"$tagged_ref\"")
	requireContains(t, sign, "digest_ref=\"${image}@${digest}\"")
	requireContains(t, sign, "CALLER_WORKFLOW_REF: ${{ github.workflow_ref }}")
	requireContains(t, sign, `"${ACTIONS_ID_TOKEN_REQUEST_URL}&audience=sigstore"`)
	requireContains(t, sign, `.job_workflow_ref == $job_workflow_ref`)
	requireContains(t, sign, `.workflow_ref == $caller_workflow_ref`)
	requireContains(t, sign, `.repository == $caller_repository`)
	requireContains(t, sign, `.ref == $caller_ref`)
	requireContains(t, sign, `.sha == $caller_sha`)
	requireContains(t, sign, "validate_oidc_identity\n          sign_and_verify \"$PRIMARY_IMAGE\"")
	requireContains(t, sign, "--certificate-identity \"$CERTIFICATE_IDENTITY\"")
	requireContains(t, sign, "--certificate-oidc-issuer \"https://token.actions.githubusercontent.com\"")
	requireContains(t, sign, "--certificate-github-workflow-repository \"$GITHUB_REPOSITORY\"")
	requireContains(t, sign, "--certificate-github-workflow-ref \"$GITHUB_REF\"")
	requireContains(t, sign, "--certificate-github-workflow-sha \"$GITHUB_SHA\"")
	if got := strings.Count(sign, `-a "caller-workflow-ref=${CALLER_WORKFLOW_REF}"`); got != 2 {
		t.Errorf("want the exact caller workflow ref signed and verified, got %d bindings", got)
	}
}
