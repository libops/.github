package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testPackage = "sitectl-ojs"
	testVersion = "v1.0.0"
	testCommit  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

type archiveMember struct {
	name     string
	mode     int64
	typeflag byte
	body     []byte
	linkname string
}

type githubFixture struct {
	t             *testing.T
	mu            sync.Mutex
	server        *httptest.Server
	remote        string
	sourceRepo    string
	packageName   string
	version       string
	assets        map[int64][]byte
	assetMetadata []releaseAsset
	assetRequests []int64
	pullNumber    int
	pullPosts     int
	foreignPull   bool
	extraPullFile bool
	annotatedTag  bool
	staleBase     bool
}

func TestValidateArchiveAcceptsExecutableRootBinary(t *testing.T) {
	archive := makeArchive(t,
		archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("binary")},
		archiveMember{name: "LICENSE", mode: 0644, typeflag: tar.TypeReg, body: []byte("license")},
	)
	if err := validateArchive(archive, testPackage); err != nil {
		t.Fatalf("valid archive failed: %v", err)
	}
}

func TestValidateArchiveRejectsUnsafeContents(t *testing.T) {
	tests := []struct {
		name     string
		contents func(*testing.T) []byte
		want     string
	}{
		{
			name: "malformed gzip",
			contents: func(*testing.T) []byte {
				return []byte("not a gzip archive")
			},
			want: "open gzip stream",
		},
		{
			name: "unsafe path",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: "../" + testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("binary")})
			},
			want: "not a root file",
		},
		{
			name: "absolute path",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: "/" + testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("binary")})
			},
			want: "not a root file",
		},
		{
			name: "symlink",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeSymlink, linkname: "/tmp/evil"})
			},
			want: "not a regular file",
		},
		{
			name: "hard link",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeLink, linkname: "other"})
			},
			want: "not a regular file",
		},
		{
			name: "non executable binary",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: testPackage, mode: 0644, typeflag: tar.TypeReg, body: []byte("binary")})
			},
			want: "not executable",
		},
		{
			name: "empty binary",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeReg})
			},
			want: "empty",
		},
		{
			name: "duplicate member",
			contents: func(t *testing.T) []byte {
				return makeArchive(t,
					archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("one")},
					archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("two")},
				)
			},
			want: "duplicate",
		},
		{
			name: "case folded duplicate member",
			contents: func(t *testing.T) []byte {
				return makeArchive(t,
					archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("one")},
					archiveMember{name: strings.ToUpper(testPackage), mode: 0644, typeflag: tar.TypeReg, body: []byte("two")},
				)
			},
			want: "duplicate",
		},
		{
			name: "unsafe permission bits",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: testPackage, mode: 04755, typeflag: tar.TypeReg, body: []byte("binary")})
			},
			want: "unsafe permission",
		},
		{
			name: "missing tar end marker",
			contents: func(t *testing.T) []byte {
				valid := makeArchive(t, archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("binary")})
				return removeTarEndMarker(t, valid)
			},
			want: "end marker",
		},
		{
			name: "second gzip stream",
			contents: func(t *testing.T) []byte {
				valid := makeArchive(t, archiveMember{name: testPackage, mode: 0755, typeflag: tar.TypeReg, body: []byte("binary")})
				return append(append([]byte(nil), valid...), valid...)
			},
			want: "second gzip stream",
		},
		{
			name: "missing package binary",
			contents: func(t *testing.T) []byte {
				return makeArchive(t, archiveMember{name: "README", mode: 0644, typeflag: tar.TypeReg, body: []byte("read me")})
			},
			want: "does not contain",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateArchive(test.contents(t), testPackage)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestParseChecksumsRejectsMalformedAndDuplicateEntries(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		want     string
	}{
		{name: "empty", want: "empty"},
		{name: "malformed", contents: "nope\n", want: "malformed"},
		{
			name:     "unsafe filename",
			contents: strings.Repeat("a", 64) + "  ../archive.tar.gz\n",
			want:     "malformed",
		},
		{
			name: "duplicate",
			contents: strings.Repeat("a", 64) + "  archive.tar.gz\n" +
				strings.Repeat("b", 64) + "  archive.tar.gz\n",
			want: "duplicate",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseChecksums([]byte(test.contents))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("got %v, want error containing %q", err, test.want)
			}
		})
	}
}

func TestLegacyFormulaMetadataRemainsSupported(t *testing.T) {
	for _, packageName := range []string{"sitectl-app-tmpl", "sitectl-libops"} {
		t.Run(packageName, func(t *testing.T) {
			repository := "libops/" + packageName
			current, err := os.ReadFile(filepath.Join("testdata", packageName+".rb"))
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := formulaMetadataFor(repository, packageName, current, true)
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Description == "" || metadata.License != "MIT" {
				t.Fatalf("legacy metadata was not upgraded: %#v", metadata)
			}
			info := resolvedRelease{
				Version:   testVersion,
				Checksums: formulaChecksums(packageName),
			}
			formula, err := renderFormula(repository, packageName, info, metadata)
			if err != nil {
				t.Fatal(err)
			}
			for _, required := range []string{
				`  desc "` + canonicalDescriptions[packageName] + `"`,
				`  homepage "https://github.com/` + repository + `"`,
				`  license "MIT"`,
				`  depends_on "libops/homebrew/sitectl"`,
			} {
				if !strings.Contains(formula, required) {
					t.Errorf("formula missing %q:\n%s", required, formula)
				}
			}
		})
	}
}

func TestCanonicalFormulaMetadataCoversCurrentPackages(t *testing.T) {
	for packageName, description := range canonicalDescriptions {
		t.Run(packageName, func(t *testing.T) {
			if description == "" {
				t.Fatal("canonical description is empty")
			}
			metadata, err := formulaMetadataFor("libops/"+packageName, packageName, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			if metadata.Description != description || metadata.License != "MIT" {
				t.Fatalf("metadata = %#v", metadata)
			}
		})
	}
}

func TestCanonicalDependenciesAreUnionedWithLegacyFormulaMetadata(t *testing.T) {
	const packageName = "sitectl-isle"
	current := minimalFormula(
		"libops/"+packageName,
		packageName,
		"0.19.0",
		"Islandora",
		"MIT",
		[]string{"libops/homebrew/sitectl"},
	)
	metadata, err := formulaMetadataFor("libops/"+packageName, packageName, []byte(current), true)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"libops/homebrew/sitectl",
		"libops/homebrew/sitectl-drupal",
	}
	if fmt.Sprint(metadata.Dependencies) != fmt.Sprint(want) {
		t.Fatalf("dependencies = %v, want %v", metadata.Dependencies, want)
	}
}

func TestFormulaClassRejectsEmptyPackageSegments(t *testing.T) {
	for _, packageName := range []string{"sitectl-", "sitectl-ojs--test"} {
		t.Run(packageName, func(t *testing.T) {
			if _, err := formulaClass(packageName); err == nil {
				t.Fatalf("formulaClass(%q) accepted an empty package segment", packageName)
			}
		})
	}
}

func TestReconcileCreatesExactBranchAndPullRequest(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	defer fixture.close()

	if err := fixture.run(); err != nil {
		t.Fatal(err)
	}
	branch := testPackage + "-1.0.0"
	changed := fixture.gitOutput("--git-dir", fixture.remote, "diff", "--name-only", "main..."+branch)
	if strings.TrimSpace(changed) != testPackage+".rb" {
		t.Fatalf("branch changed unexpected files: %q", changed)
	}
	formula := fixture.gitOutput("--git-dir", fixture.remote, "show", branch+":"+testPackage+".rb")
	for _, required := range []string{
		`version "1.0.0"`,
		`releases/download/v1.0.0/` + testPackage + `_Darwin_x86_64.tar.gz`,
		`releases/download/v1.0.0/` + testPackage + `_Darwin_arm64.tar.gz`,
		`releases/download/v1.0.0/` + testPackage + `_Linux_x86_64.tar.gz`,
		`releases/download/v1.0.0/` + testPackage + `_Linux_arm64.tar.gz`,
	} {
		if !strings.Contains(formula, required) {
			t.Errorf("formula missing %q:\n%s", required, formula)
		}
	}
	if fixture.pullPosts != 1 {
		t.Fatalf("created %d pull requests, want 1", fixture.pullPosts)
	}
	sort.Slice(fixture.assetRequests, func(i, j int) bool {
		return fixture.assetRequests[i] < fixture.assetRequests[j]
	})
	if got, want := fmt.Sprint(fixture.assetRequests), "[100 101 102 103 104]"; got != want {
		t.Fatalf("asset downloads = %s, want exact IDs %s", got, want)
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	defer fixture.close()

	if err := fixture.run(); err != nil {
		t.Fatal(err)
	}
	branch := testPackage + "-1.0.0"
	firstSHA := strings.TrimSpace(fixture.gitOutput("--git-dir", fixture.remote, "rev-parse", branch))
	firstPosts := fixture.pullPosts
	if err := fixture.run(); err != nil {
		t.Fatal(err)
	}
	secondSHA := strings.TrimSpace(fixture.gitOutput("--git-dir", fixture.remote, "rev-parse", branch))
	if secondSHA != firstSHA {
		t.Fatalf("idempotent rerun changed branch from %s to %s", firstSHA, secondSHA)
	}
	if fixture.pullPosts != firstPosts {
		t.Fatalf("idempotent rerun created another pull request: %d -> %d", firstPosts, fixture.pullPosts)
	}
}

func TestReconcileRepairsTamperedBranchWithLease(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	defer fixture.close()

	if err := fixture.run(); err != nil {
		t.Fatal(err)
	}
	branch := testPackage + "-1.0.0"
	fixture.tamperBranch(branch)
	tamperedSHA := strings.TrimSpace(fixture.gitOutput("--git-dir", fixture.remote, "rev-parse", branch))

	if err := fixture.run(); err != nil {
		t.Fatal(err)
	}
	repairedSHA := strings.TrimSpace(fixture.gitOutput("--git-dir", fixture.remote, "rev-parse", branch))
	if repairedSHA == tamperedSHA {
		t.Fatal("tampered branch was not repaired")
	}
	changed := fixture.gitOutput("--git-dir", fixture.remote, "diff", "--name-only", "main..."+branch)
	if strings.TrimSpace(changed) != testPackage+".rb" {
		t.Fatalf("repaired branch changed unexpected files: %q", changed)
	}
	if strings.Contains(fixture.gitOutput("--git-dir", fixture.remote, "show", branch+":"+testPackage+".rb"), "tampered") {
		t.Fatal("tampered formula content remains after repair")
	}
}

func TestReconcileIgnoresForeignForkPullRequest(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	fixture.foreignPull = true
	defer fixture.close()

	if err := fixture.run(); err != nil {
		t.Fatal(err)
	}
	if fixture.pullPosts != 1 {
		t.Fatalf("foreign pull request suppressed same-repository PR creation; posts=%d", fixture.pullPosts)
	}
}

func TestReconcileRejectsPullRequestWithExtraFile(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	fixture.extraPullFile = true
	defer fixture.close()

	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "must change exactly") {
		t.Fatalf("got %v, want exact one-file PR failure", err)
	}
}

func TestReconcileRejectsDowngrade(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "2.0.0")
	defer fixture.close()

	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "refusing to downgrade") {
		t.Fatalf("got %v, want downgrade failure", err)
	}
	if output, commandErr := exec.Command("git", "--git-dir", fixture.remote, "rev-parse", "--verify", testPackage+"-1.0.0").CombinedOutput(); commandErr == nil {
		t.Fatalf("downgrade created a branch: %s", output)
	}
}

func TestReconcileRejectsChecksumTampering(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	fixture.tamperChecksums()
	defer fixture.close()

	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("got %v, want checksum tampering failure", err)
	}
}

func TestReconcileRejectsMissingOrDuplicateReleaseAssets(t *testing.T) {
	t.Run("missing", func(t *testing.T) {
		fixture := newGitHubFixture(t, testPackage, "0.9.0")
		fixture.assetMetadata = fixture.assetMetadata[1:]
		defer fixture.close()
		err := fixture.run()
		if err == nil || !strings.Contains(err.Error(), "missing required asset") {
			t.Fatalf("got %v, want missing asset failure", err)
		}
	})
	t.Run("missing digest", func(t *testing.T) {
		fixture := newGitHubFixture(t, testPackage, "0.9.0")
		fixture.assetMetadata[0].Digest = ""
		defer fixture.close()
		err := fixture.run()
		if err == nil || !strings.Contains(err.Error(), "no valid sha256 GitHub digest") {
			t.Fatalf("got %v, want missing digest failure", err)
		}
	})
	t.Run("invalid digest", func(t *testing.T) {
		fixture := newGitHubFixture(t, testPackage, "0.9.0")
		fixture.assetMetadata[len(fixture.assetMetadata)-1].Digest = "sha512:" + strings.Repeat("a", 128)
		defer fixture.close()
		err := fixture.run()
		if err == nil || !strings.Contains(err.Error(), "no valid sha256 GitHub digest") {
			t.Fatalf("got %v, want invalid digest failure", err)
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		fixture := newGitHubFixture(t, testPackage, "0.9.0")
		duplicate := fixture.assetMetadata[0]
		duplicate.ID = 999
		fixture.assetMetadata = append(fixture.assetMetadata, duplicate)
		defer fixture.close()
		err := fixture.run()
		if err == nil || !strings.Contains(err.Error(), "duplicate asset") {
			t.Fatalf("got %v, want duplicate asset name failure", err)
		}
	})
	t.Run("duplicate ID", func(t *testing.T) {
		fixture := newGitHubFixture(t, testPackage, "0.9.0")
		fixture.assetMetadata[1].ID = fixture.assetMetadata[0].ID
		defer fixture.close()
		err := fixture.run()
		if err == nil || !strings.Contains(err.Error(), "share asset ID") {
			t.Fatalf("got %v, want duplicate asset ID failure", err)
		}
	})
}

func TestReconcileResolvesAnnotatedTagToCommit(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	fixture.annotatedTag = true
	defer fixture.close()

	if err := fixture.run(); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileRejectsPullRequestWhoseBaseAdvanced(t *testing.T) {
	fixture := newGitHubFixture(t, testPackage, "0.9.0")
	fixture.staleBase = true
	defer fixture.close()
	err := fixture.run()
	if err == nil || !strings.Contains(err.Error(), "exact same-repository reconciliation commit and base") {
		t.Fatalf("got %v, want stale PR base failure", err)
	}
}

func TestHomebrewRecoveryRequiresSourceDefaultBranch(t *testing.T) {
	t.Run("default branch", func(t *testing.T) {
		fixture := newGitHubFixture(t, testPackage, "0.9.0")
		defer fixture.close()
		if err := fixture.runMode("homebrew-only", "branch", "main", testVersion); err != nil {
			t.Fatal(err)
		}
	})
	for name, ref := range map[string][2]string{
		"feature branch": {"branch", "feature"},
		"tag":            {"tag", "main"},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newGitHubFixture(t, testPackage, "0.9.0")
			defer fixture.close()
			err := fixture.runMode("homebrew-only", ref[0], ref[1], testVersion)
			if err == nil || !strings.Contains(err.Error(), "must run from the repository default branch") {
				t.Fatalf("got %v, want default-branch failure", err)
			}
		})
	}
}

func newGitHubFixture(t *testing.T, packageName, currentVersion string) *githubFixture {
	t.Helper()
	fixture := &githubFixture{
		t:           t,
		sourceRepo:  "libops/" + packageName,
		packageName: packageName,
		version:     testVersion,
		assets:      make(map[int64][]byte),
		pullNumber:  41,
	}
	checksumLines := make([]string, 0, 4)
	for index, name := range archiveNames(packageName) {
		contents := makeArchive(t, archiveMember{
			name:     packageName,
			mode:     0755,
			typeflag: tar.TypeReg,
			body:     []byte("binary for " + name),
		})
		id := int64(101 + index)
		fixture.assets[id] = contents
		digest := fmt.Sprintf("%x", sha256.Sum256(contents))
		checksumLines = append(checksumLines, digest+"  "+name)
		fixture.assetMetadata = append(fixture.assetMetadata, releaseAsset{
			ID:     id,
			Name:   name,
			Size:   int64(len(contents)),
			State:  "uploaded",
			Digest: "sha256:" + digest,
		})
	}
	checksums := []byte(strings.Join(checksumLines, "\n") + "\n")
	fixture.assets[100] = checksums
	checksumDigest := fmt.Sprintf("%x", sha256.Sum256(checksums))
	fixture.assetMetadata = append(fixture.assetMetadata, releaseAsset{
		ID:     100,
		Name:   "checksums.txt",
		Size:   int64(len(checksums)),
		State:  "uploaded",
		Digest: "sha256:" + checksumDigest,
	})

	fixture.remote = filepath.Join(t.TempDir(), "homebrew.git")
	runGit(t, "", "init", "--bare", "--quiet", fixture.remote)
	seed := filepath.Join(t.TempDir(), "seed")
	runGit(t, "", "init", "--quiet", seed)
	runGit(t, seed, "config", "user.name", "Fixture")
	runGit(t, seed, "config", "user.email", "fixture@example.test")
	current := minimalFormula(
		fixture.sourceRepo,
		packageName,
		currentVersion,
		"Fixture plugin",
		"MIT",
		[]string{"libops/homebrew/sitectl"},
	)
	if err := os.WriteFile(filepath.Join(seed, packageName+".rb"), []byte(current), 0644); err != nil {
		t.Fatal(err)
	}
	runGit(t, seed, "add", packageName+".rb")
	runGitWithEnv(t, seed, []string{
		"GIT_AUTHOR_DATE=2026-01-01T00:00:00Z",
		"GIT_COMMITTER_DATE=2026-01-01T00:00:00Z",
	}, "commit", "--quiet", "-m", "seed")
	runGit(t, seed, "branch", "-M", "main")
	runGit(t, seed, "remote", "add", "origin", fixture.remote)
	runGit(t, seed, "push", "--quiet", "origin", "main")

	fixture.server = httptest.NewServer(http.HandlerFunc(fixture.handleAPI))
	return fixture
}

func (f *githubFixture) close() {
	f.server.Close()
}

func (f *githubFixture) run() error {
	return f.runMode("full", "tag", f.version, "")
}

func (f *githubFixture) runMode(mode, refType, refName, releaseVersion string) error {
	r := newReconciler(config{
		APIBase:        f.server.URL,
		SourceRepo:     f.sourceRepo,
		HomebrewRepo:   defaultHomebrewRepo,
		HomebrewRemote: f.remote,
		PackageName:    f.packageName,
		ReleaseMode:    mode,
		ReleaseVersion: releaseVersion,
		RefName:        refName,
		RefType:        refType,
		SourceToken:    "source-token",
		HomebrewToken:  "homebrew-token",
		WorkRoot:       f.t.TempDir(),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return r.run(ctx)
}

func (f *githubFixture) handleAPI(response http.ResponseWriter, request *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if request.Header.Get("Authorization") == "" {
		http.Error(response, "missing authorization", http.StatusUnauthorized)
		return
	}
	path := request.URL.Path
	switch {
	case path == "/repos/"+f.sourceRepo+"/releases/tags/"+f.version:
		writeJSON(response, release{
			ID:          7,
			TagName:     f.version,
			PublishedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
			Assets:      f.assetMetadata,
		})
	case path == "/repos/"+f.sourceRepo+"/git/ref/tags/"+f.version:
		object := gitObject{Type: "commit", SHA: testCommit}
		if f.annotatedTag {
			object = gitObject{Type: "tag", SHA: strings.Repeat("b", 40)}
		}
		writeJSON(response, gitRef{Object: object})
	case path == "/repos/"+f.sourceRepo+"/git/tags/"+strings.Repeat("b", 40):
		writeJSON(response, annotatedTag{Object: gitObject{Type: "commit", SHA: testCommit}})
	case path == "/repos/"+f.sourceRepo+"/git/commits/"+testCommit:
		commit := gitCommit{}
		commit.Committer.Date = time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
		writeJSON(response, commit)
	case strings.HasPrefix(path, "/repos/"+f.sourceRepo+"/releases/assets/"):
		var id int64
		if _, err := fmt.Sscanf(filepath.Base(path), "%d", &id); err != nil {
			http.Error(response, "bad asset", http.StatusBadRequest)
			return
		}
		contents, ok := f.assets[id]
		if !ok {
			http.NotFound(response, request)
			return
		}
		f.assetRequests = append(f.assetRequests, id)
		response.Header().Set("Content-Length", fmt.Sprint(len(contents)))
		response.WriteHeader(http.StatusOK)
		_, _ = response.Write(contents)
	case path == "/repos/"+defaultHomebrewRepo+"/pulls" && request.Method == http.MethodGet:
		var pulls []pullRequest
		if f.foreignPull {
			foreign := f.pull(17, "open", "someone/homebrew", strings.Repeat("c", 40))
			pulls = append(pulls, foreign)
		}
		if f.pullPosts > 0 {
			pulls = append(pulls, f.pull(f.pullNumber, "open", defaultHomebrewRepo, f.branchSHA()))
		}
		writeJSON(response, pulls)
	case path == "/repos/"+defaultHomebrewRepo+"/pulls" && request.Method == http.MethodPost:
		f.pullPosts++
		writeJSON(response, f.pull(f.pullNumber, "open", defaultHomebrewRepo, f.branchSHA()))
	case path == fmt.Sprintf("/repos/%s/pulls/%d", defaultHomebrewRepo, f.pullNumber):
		writeJSON(response, f.pull(f.pullNumber, "open", defaultHomebrewRepo, f.branchSHA()))
	case path == fmt.Sprintf("/repos/%s/pulls/%d/files", defaultHomebrewRepo, f.pullNumber):
		files := []pullFile{{Filename: f.packageName + ".rb"}}
		if f.extraPullFile {
			files = append(files, pullFile{Filename: "unexpected.txt"})
		}
		writeJSON(response, files)
	case path == "/repos/"+f.sourceRepo:
		writeJSON(response, repository{DefaultBranch: "main"})
	default:
		http.Error(response, "unexpected endpoint "+request.Method+" "+request.URL.String(), http.StatusNotFound)
	}
}

func (f *githubFixture) pull(number int, state, repositoryName, sha string) pullRequest {
	pull := pullRequest{Number: number, State: state}
	pull.Head.Ref = f.packageName + "-1.0.0"
	pull.Head.SHA = sha
	pull.Head.Repo.FullName = repositoryName
	pull.Base.Ref = "main"
	pull.Base.SHA = f.mainSHA()
	if f.staleBase {
		pull.Base.SHA = strings.Repeat("d", 40)
	}
	pull.Base.Repo.FullName = defaultHomebrewRepo
	return pull
}

func (f *githubFixture) mainSHA() string {
	output, err := exec.Command(
		"git",
		"--git-dir",
		f.remote,
		"rev-parse",
		"--verify",
		"main^{commit}",
	).CombinedOutput()
	if err != nil {
		f.t.Fatalf("resolve fixture main: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *githubFixture) branchSHA() string {
	output, err := exec.Command(
		"git",
		"--git-dir",
		f.remote,
		"rev-parse",
		"--verify",
		f.packageName+"-1.0.0^{commit}",
	).CombinedOutput()
	if err != nil {
		f.t.Fatalf("resolve fixture branch: %v\n%s", err, output)
	}
	return strings.TrimSpace(string(output))
}

func (f *githubFixture) gitOutput(args ...string) string {
	f.t.Helper()
	output, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		f.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return string(output)
}

func (f *githubFixture) tamperBranch(branch string) {
	f.t.Helper()
	work := filepath.Join(f.t.TempDir(), "tamper")
	runGit(f.t, "", "clone", "--quiet", f.remote, work)
	runGit(f.t, work, "checkout", "--quiet", branch)
	runGit(f.t, work, "config", "user.name", "Tamper")
	runGit(f.t, work, "config", "user.email", "tamper@example.test")
	formulaPath := filepath.Join(work, f.packageName+".rb")
	formula, err := os.ReadFile(formulaPath)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(formulaPath, append(formula, []byte("# tampered\n")...), 0644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "unexpected.txt"), []byte("tampered"), 0644); err != nil {
		f.t.Fatal(err)
	}
	runGit(f.t, work, "add", ".")
	runGit(f.t, work, "commit", "--quiet", "-m", "tamper")
	runGit(f.t, work, "push", "--quiet", "--force", "origin", branch)
}

func (f *githubFixture) tamperChecksums() {
	f.t.Helper()
	checksums := append([]byte(nil), f.assets[100]...)
	lines := strings.Split(string(checksums), "\n")
	lines[0] = strings.Repeat("0", 64) + lines[0][64:]
	checksums = []byte(strings.Join(lines, "\n"))
	f.assets[100] = checksums
	digest := fmt.Sprintf("%x", sha256.Sum256(checksums))
	for index := range f.assetMetadata {
		if f.assetMetadata[index].ID == 100 {
			f.assetMetadata[index].Size = int64(len(checksums))
			f.assetMetadata[index].Digest = "sha256:" + digest
		}
	}
}

func makeArchive(t *testing.T, members ...archiveMember) []byte {
	t.Helper()
	var output bytes.Buffer
	gzipWriter := gzip.NewWriter(&output)
	tarWriter := tar.NewWriter(gzipWriter)
	for _, member := range members {
		typeflag := member.typeflag
		if typeflag == 0 {
			typeflag = tar.TypeReg
		}
		header := &tar.Header{
			Name:     member.name,
			Mode:     member.mode,
			Typeflag: typeflag,
			Size:     int64(len(member.body)),
			Linkname: member.linkname,
			ModTime:  time.Unix(0, 0).UTC(),
		}
		if typeflag != tar.TypeReg && typeflag != tar.TypeRegA {
			header.Size = 0
		}
		if err := tarWriter.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if len(member.body) > 0 {
			if _, err := tarWriter.Write(member.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func removeTarEndMarker(t *testing.T, archive []byte) []byte {
	t.Helper()
	reader, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := reader.Close(); err != nil {
		t.Fatal(err)
	}
	if len(decompressed) < 1024 {
		t.Fatal("fixture tar is too short")
	}
	decompressed = decompressed[:len(decompressed)-1024]
	var output bytes.Buffer
	writer := gzip.NewWriter(&output)
	if _, err := writer.Write(decompressed); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func minimalFormula(
	repositoryName, packageName, version, description, license string,
	dependencies []string,
) string {
	var builder strings.Builder
	fmt.Fprintf(&builder, "class %s < Formula\n", mustFormulaClass(packageName))
	fmt.Fprintf(&builder, "  desc %q\n", description)
	fmt.Fprintf(&builder, "  homepage %q\n", "https://github.com/"+repositoryName)
	fmt.Fprintf(&builder, "  version %q\n", version)
	if license != "" {
		fmt.Fprintf(&builder, "  license %q\n", license)
	}
	for _, dependency := range dependencies {
		fmt.Fprintf(&builder, "  depends_on %q\n", dependency)
	}
	builder.WriteString("end\n")
	return builder.String()
}

func mustFormulaClass(packageName string) string {
	className, err := formulaClass(packageName)
	if err != nil {
		panic(err)
	}
	return className
}

func formulaChecksums(packageName string) map[string]string {
	checksums := make(map[string]string)
	for index, name := range archiveNames(packageName) {
		checksums[name] = strings.Repeat(fmt.Sprint(index+1), 64)
	}
	return checksums
}

func writeJSON(response http.ResponseWriter, value any) {
	response.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(response).Encode(value); err != nil {
		panic(err)
	}
}

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	runGitWithEnv(t, dir, nil, args...)
}

func runGitWithEnv(t *testing.T, dir string, extraEnv []string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	if dir != "" {
		command.Dir = dir
	}
	command.Env = append(os.Environ(), extraEnv...)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}
