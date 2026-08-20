package github

import (
	"regexp"
	"strings"
	"testing"
)

const sitectlInstallActionCommit = "86054f5880f0b782dc1c3338895e1755e2c975f1"

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
	actionPin := "uses: libops/.github/.github/actions/install-sitectl@" + sitectlInstallActionCommit
	if !strings.Contains(workflow, actionPin) {
		t.Errorf("install-sitectl action must remain pinned to %s", sitectlInstallActionCommit)
	}
}

func TestSitectlSmokeWorkflowDefaultsToImmutableStrictReleaseChecks(t *testing.T) {
	workflow := githubReadFile(t, ".github/workflows/sitectl-create-smoke-test.yaml")
	for _, required := range []string{
		"      run-verify:\n        description: Run strict sitectl verification",
		"      allow-unversioned-packages:\n        description: Permit compatibility installs",
		"local-plugin-ref must be an exact 40-character commit SHA",
		"sitectl-ref must be an exact 40-character commit SHA",
		"sitectl verify --strict \"${verify_args[@]}\"",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("sitectl smoke workflow is missing strict release contract %q", required)
		}
	}

	defaultTrue := regexp.MustCompile(`(?m)^      run-verify:\n(?:        .*\n){3}        default: true$`)
	if !defaultTrue.MatchString(workflow) {
		t.Error("run-verify must default to true")
	}
	defaultFalse := regexp.MustCompile(`(?m)^      allow-unversioned-packages:\n(?:        .*\n){3}        default: false$`)
	if !defaultFalse.MatchString(workflow) {
		t.Error("allow-unversioned-packages must default to false")
	}
	if strings.Contains(workflow, "default: main") {
		t.Error("source checkout refs must not default to the mutable main branch")
	}
}

func TestSitectlSmokeWorkflowAcceptsResolvedDefaultsNoninteractively(t *testing.T) {
	workflow := githubReadFile(t, ".github/workflows/sitectl-create-smoke-test.yaml")
	if !strings.Contains(workflow, "            --yolo \\\n") {
		t.Error("sitectl create smoke workflow must explicitly accept resolved defaults")
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
		"grep -Eq '^    commit: [0-9a-f]{40}([0-9a-f]{24})?$'",
		"grep -Eq '^        digest: sha256:[0-9a-f]{64}$'",
		"$0 == \"    revision: \" expected",
		"grep -Fxq \"    repository: ${TEMPLATE_REPO}\"",
		"$0 == \"componentDefaults:\"",
	} {
		if !strings.Contains(workflow, required) {
			t.Errorf("sitectl smoke workflow is missing provenance contract %q", required)
		}
	}
}
