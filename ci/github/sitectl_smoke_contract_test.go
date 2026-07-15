package github

import (
	"regexp"
	"strings"
	"testing"
)

func TestSitectlSmokeWorkflowExposesAndWiresExactVersions(t *testing.T) {
	workflow := githubReadFile(t, ".github/workflows/sitectl-create-smoke-test.yaml")
	for _, required := range []string{
		"      package-versions:\n",
		"      allow-unversioned-packages:\n",
		"          package-versions: ${{ inputs.package-versions }}",
		"          allow-unversioned: ${{ inputs.allow-unversioned-packages }}",
		"echo \"package-versions=${PACKAGE_VERSIONS}\"",
		"echo \"allow-unversioned-packages=${ALLOW_UNVERSIONED_PACKAGES}\"",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("sitectl smoke workflow is missing %q", required)
		}
	}
	actionPin := regexp.MustCompile(`uses: libops/\.github/\.github/actions/install-sitectl@[0-9a-f]{40}`)
	if !actionPin.MatchString(workflow) {
		t.Error("install-sitectl action must remain pinned to a full commit SHA")
	}
}

func TestSitectlSmokeWorkflowCanGateRetainedTemplateProvenance(t *testing.T) {
	workflow := githubReadFile(t, ".github/workflows/sitectl-create-smoke-test.yaml")
	for _, required := range []string{
		"      expected-template-lock-revision:\n",
		"expected-template-lock-revision requires checkout-source=template",
		"      - name: Verify retained template provenance",
		"if: ${{ inputs.expected-template-lock-revision != '' }}",
		"lock=\"${PROJECT_DIR}/.libops/template.lock.yaml\"",
		"grep -Eq '^  commit: [0-9a-f]{40}([0-9a-f]{24})?$'",
		"grep -Eq '^    digest: sha256:[0-9a-f]{64}$'",
		"$0 == \"componentDefaults:\"",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("sitectl smoke workflow is missing provenance contract %q", required)
		}
	}
}
