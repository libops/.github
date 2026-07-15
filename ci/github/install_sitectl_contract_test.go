package github

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const sitectlArchiveKeyFingerprint = "FBF887BCE093167F499F537BCFB2A9DBD0A2156A"
const sitectlArchiveKeySHA256 = "caa22fc0474b2f0934ee0da2749265db297b0e3e74ee0e994c3225900a04ff59"

func githubRepositoryRoot(t *testing.T) string {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve GitHub contract test path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", ".."))
}

func githubReadFile(t *testing.T, relative string) string {
	t.Helper()
	contents, err := os.ReadFile(filepath.Join(githubRepositoryRoot(t), relative))
	if err != nil {
		t.Fatalf("read %s: %v", relative, err)
	}
	return string(contents)
}

func TestSitectlInstallerShellIsValid(t *testing.T) {
	script := filepath.Join(githubRepositoryRoot(t), ".github", "actions", "install-sitectl", "install.sh")
	cmd := exec.Command("bash", "-n", script) // #nosec G204 -- fixed repository script.
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("bash -n install.sh: %v\n%s", err, output)
	}
}

func runSitectlInstaller(t *testing.T, packages, versions, allow string) (string, string, error) {
	t.Helper()
	tempDir := t.TempDir()
	fakeBin := filepath.Join(tempDir, "bin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	sudoLog := filepath.Join(tempDir, "sudo.log")
	summary := filepath.Join(tempDir, "summary.md")
	fakeSudo := `#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >> "${FAKE_SUDO_LOG}"
case "${1:-}" in
  gpg|tee) cat >/dev/null ;;
esac
`
	if err := os.WriteFile(filepath.Join(fakeBin, "sudo"), []byte(fakeSudo), 0700); err != nil {
		t.Fatal(err)
	}

	actionDir := filepath.Join(githubRepositoryRoot(t), ".github", "actions", "install-sitectl")
	script := filepath.Join(actionDir, "install.sh")
	cmd := exec.Command("bash", script) // #nosec G204 -- fixed repository script with test-controlled environment.
	cmd.Env = append(os.Environ(),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"PACKAGES="+packages,
		"PACKAGE_VERSIONS="+versions,
		"ALLOW_UNVERSIONED="+allow,
		"GITHUB_ACTION_PATH="+actionDir,
		"FAKE_SUDO_LOG="+sudoLog,
		"GITHUB_STEP_SUMMARY="+summary,
	)
	output, runErr := cmd.CombinedOutput()
	log, readErr := os.ReadFile(sudoLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	return string(output), string(log), runErr
}

func TestSitectlInstallerPinsEveryAptPackage(t *testing.T) {
	output, log, err := runSitectlInstaller(
		t,
		"sitectl sitectl-omeka-s",
		"sitectl-omeka-s=1.2.3-rc.1+build.7 sitectl=0.39.0",
		"false",
	)
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, output)
	}
	if !strings.Contains(log, "apt-get install -y sitectl=0.39.0 sitectl-omeka-s=1.2.3-rc.1+build.7") {
		t.Fatalf("apt install was not fully and deterministically pinned:\n%s", log)
	}
	if strings.Contains(output, "::warning::") {
		t.Fatalf("exact install emitted a compatibility warning:\n%s", output)
	}
}

func TestSitectlInstallerMakesCompatibilityModeVisible(t *testing.T) {
	output, log, err := runSitectlInstaller(t, "sitectl", "", "true")
	if err != nil {
		t.Fatalf("installer failed: %v\n%s", err, output)
	}
	if !strings.Contains(output, "::warning::No exact sitectl package versions were supplied") {
		t.Fatalf("compatibility install did not emit an annotation warning:\n%s", output)
	}
	if !strings.Contains(log, "apt-get install -y sitectl") {
		t.Fatalf("compatibility install did not reach apt:\n%s", log)
	}
}

func TestSitectlInstallerRejectsIncompleteOrInvalidVersionMaps(t *testing.T) {
	tests := []struct {
		name     string
		packages string
		versions string
		allow    string
	}{
		{name: "missing", packages: "sitectl sitectl-isle", versions: "sitectl=1.2.3", allow: "false"},
		{name: "extra", packages: "sitectl", versions: "sitectl=1.2.3 sitectl-isle=1.2.3", allow: "false"},
		{name: "duplicate", packages: "sitectl", versions: "sitectl=1.2.3 sitectl=1.2.4", allow: "false"},
		{name: "not-semver", packages: "sitectl", versions: "sitectl=latest", allow: "false"},
		{name: "leading-zero", packages: "sitectl", versions: "sitectl=01.2.3", allow: "false"},
		{name: "unpinned-strict", packages: "sitectl", versions: "", allow: "false"},
		{name: "invalid-policy", packages: "sitectl", versions: "sitectl=1.2.3", allow: "yes"},
		{name: "hidden-package-line", packages: "sitectl\nsitectl-isle", versions: "sitectl=1.2.3", allow: "false"},
		{name: "hidden-version-line", packages: "sitectl", versions: "sitectl=1.2.3\nsitectl-isle=1.2.3", allow: "false"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, log, err := runSitectlInstaller(t, test.packages, test.versions, test.allow)
			if err == nil {
				t.Fatalf("invalid installer input succeeded:\n%s", output)
			}
			if strings.Contains(log, "apt-get install") {
				t.Fatalf("invalid installer input reached apt:\n%s", log)
			}
		})
	}
}

func TestSitectlCompositeActionDelegatesValidatedInputs(t *testing.T) {
	action := githubReadFile(t, ".github/actions/install-sitectl/action.yaml")
	for _, required := range []string{
		"  package-versions:\n",
		"  allow-unversioned:\n",
		"PACKAGE_VERSIONS: ${{ inputs.package-versions }}",
		"ALLOW_UNVERSIONED: ${{ inputs.allow-unversioned }}",
		`run: "${GITHUB_ACTION_PATH}/install.sh"`,
	} {
		if !strings.Contains(action, required) {
			t.Errorf("install-sitectl action is missing %q", required)
		}
	}
}

func TestSitectlInstallerUsesOnlyApprovedVendoredArchiveKey(t *testing.T) {
	actionDir := filepath.Join(githubRepositoryRoot(t), ".github", "actions", "install-sitectl")
	keyPath := filepath.Join(actionDir, "sitectl-archive-keyring.asc")
	keyData, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if digest := fmt.Sprintf("%x", sha256.Sum256(keyData)); digest != sitectlArchiveKeySHA256 {
		t.Fatalf("vendored archive key SHA-256 = %s, want %s", digest, sitectlArchiveKeySHA256)
	}
	cmd := exec.Command(
		"gpg", "--batch", "--no-default-keyring", "--keyring", "/dev/null",
		"--show-keys", "--with-colons", "--fingerprint", keyPath,
	) // #nosec G204 -- fixed gpg command over the repository-owned public key.
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect vendored sitectl archive key: %v\n%s", err, output)
	}
	var primaryFingerprints []string
	wantPrimaryFingerprint := false
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 10 {
			continue
		}
		if fields[0] == "pub" {
			wantPrimaryFingerprint = true
			continue
		}
		if wantPrimaryFingerprint && fields[0] == "fpr" {
			primaryFingerprints = append(primaryFingerprints, fields[9])
			wantPrimaryFingerprint = false
		}
	}
	if len(primaryFingerprints) != 1 || primaryFingerprints[0] != sitectlArchiveKeyFingerprint {
		t.Fatalf("vendored primary fingerprints = %v, want only %s", primaryFingerprints, sitectlArchiveKeyFingerprint)
	}
	script := githubReadFile(t, ".github/actions/install-sitectl/install.sh")
	if !strings.Contains(script, sitectlArchiveKeyFingerprint) {
		t.Error("installer does not pin the approved archive key fingerprint")
	}
	if strings.Contains(script, "curl") || strings.Contains(script, "https://packages.libops.io/sitectl/sitectl-archive-keyring.asc") {
		t.Error("installer must not fetch its archive trust root at run time")
	}
}
