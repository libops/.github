package github

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

func prStatusRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve PR status contract test path")
	}
	return filepath.Join(filepath.Dir(sourceFile), "..", "..")
}

func prStatusSource(t *testing.T, path ...string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(append([]string{prStatusRepositoryRoot(t)}, path...)...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(path...), err)
	}
	return string(contents)
}

func requirePRStatusContains(t *testing.T, source, value string) {
	t.Helper()
	if !strings.Contains(source, value) {
		t.Errorf("PR status contract is missing %q", value)
	}
}

func TestPRStatusReusableWorkflowContract(t *testing.T) {
	workflow := prStatusSource(t, ".github", "workflows", "pr-status.yaml")

	for _, required := range []string{
		"  workflow_call:\n",
		"      needs-json:\n",
		"        required: true\n",
		"        type: string\n",
		"permissions: {}\n",
		"jobs:\n  merge:\n",
		"    name: merge\n",
		"    runs-on: ubuntu-24.04\n",
		"    timeout-minutes: 5\n",
		"      NEEDS_JSON: ${{ inputs.needs-json }}\n",
		"      - name: Require every dependency to succeed\n",
		"length > 0 and",
		`(.key | test("^[A-Za-z_][A-Za-z0-9_-]*$"))`,
		`(.value.outputs | type == "object")`,
		`(.value.result == "success")`,
	} {
		requirePRStatusContains(t, workflow, required)
	}

	jobs := workflow[strings.Index(workflow, "jobs:\n")+len("jobs:\n"):]
	jobPattern := regexp.MustCompile(`(?m)^  ([A-Za-z_][A-Za-z0-9_-]*):\s*$`)
	jobMatches := jobPattern.FindAllStringSubmatch(jobs, -1)
	if len(jobMatches) != 1 || jobMatches[0][1] != "merge" {
		t.Fatalf("reusable workflow jobs = %#v, want only merge", jobMatches)
	}
	if got := strings.Count(workflow, "permissions:"); got != 1 {
		t.Errorf("reusable workflow has %d permission declarations, want one empty workflow-level declaration", got)
	}
	if got := strings.Count(workflow, "      - name:"); got != 1 {
		t.Errorf("reusable workflow has %d steps, want one validation step", got)
	}

	for _, forbidden := range []string{
		"actions/checkout@",
		"secrets:",
		"secrets.",
		"id-token:",
		"packages:",
		"GITHUB_TOKEN",
		"uses:",
		"docker ",
		"ghcr.io",
		"pkg.dev",
		"gcloud ",
		"cosign ",
	} {
		if strings.Contains(workflow, forbidden) {
			t.Errorf("credential-free PR status workflow must not contain %q", forbidden)
		}
	}
}

func TestPRStatusValidationBehavior(t *testing.T) {
	workflow := prStatusSource(t, ".github", "workflows", "pr-status.yaml")
	const filterStart = "          if ! jq -e '\n"
	const filterEnd = "\n          ' <<< \"$NEEDS_JSON\""
	start := strings.Index(workflow, filterStart)
	if start == -1 {
		t.Fatal("find jq validation filter start")
	}
	start += len(filterStart)
	end := strings.Index(workflow[start:], filterEnd)
	if end == -1 {
		t.Fatal("find jq validation filter end")
	}
	filter := workflow[start : start+end]
	if _, err := exec.LookPath("jq"); err != nil {
		t.Fatalf("jq is required by the reusable workflow: %v", err)
	}

	tests := []struct {
		name  string
		input string
		valid bool
	}{
		{
			name:  "one successful dependency",
			input: `{"lint":{"result":"success","outputs":{}}}`,
			valid: true,
		},
		{
			name:  "multiple successful dependencies",
			input: `{"lint":{"result":"success","outputs":{}},"test-matrix":{"result":"success","outputs":{"artifact":"ready"}}}`,
			valid: true,
		},
		{name: "empty input", input: "", valid: false},
		{name: "malformed JSON", input: `{`, valid: false},
		{name: "null", input: `null`, valid: false},
		{name: "array", input: `[]`, valid: false},
		{name: "empty object", input: `{}`, valid: false},
		{name: "failure", input: `{"test":{"result":"failure","outputs":{}}}`, valid: false},
		{name: "cancelled", input: `{"test":{"result":"cancelled","outputs":{}}}`, valid: false},
		{name: "skipped", input: `{"test":{"result":"skipped","outputs":{}}}`, valid: false},
		{name: "missing result", input: `{"test":{"outputs":{}}}`, valid: false},
		{name: "missing outputs", input: `{"test":{"result":"success"}}`, valid: false},
		{name: "invalid job ID", input: `{"test job":{"result":"success","outputs":{}}}`, valid: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command := exec.Command("jq", "-e", filter)
			command.Stdin = strings.NewReader(test.input)
			if err := command.Run(); (err == nil) != test.valid {
				t.Fatalf("validation success = %t, want %t (error: %v)", err == nil, test.valid, err)
			}
		})
	}
}

func TestPRStatusCallerContractIsDocumented(t *testing.T) {
	readme := prStatusSource(t, "README.md")
	for _, required := range []string{
		"  run:\n",
		"    name: run\n",
		"    if: ${{ always() }}\n",
		"    needs: [lint, test]\n",
		"    permissions: {}\n",
		"    uses: libops/.github/.github/workflows/pr-status.yaml@FULL_40_CHARACTER_COMMIT_SHA\n",
		"      needs-json: ${{ toJSON(needs) }}\n",
		"`run / merge`",
	} {
		requirePRStatusContains(t, readme, required)
	}
}
