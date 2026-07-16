// Command homebrew-reconcile rebuilds a LibOps Homebrew formula from the
// verified assets of an already-published sitectl release.
package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	defaultAPIBase        = "https://api.github.com"
	defaultHomebrewRepo   = "libops/homebrew"
	defaultHomebrewRemote = "https://github.com/libops/homebrew.git"
	maxAPIResponse        = 4 << 20
	maxChecksumAsset      = 1 << 20
	maxArchiveAsset       = 128 << 20
	maxArchiveContents    = 128 << 20
	maxArchiveEntries     = 32
)

var (
	repositoryPattern  = regexp.MustCompile(`^libops/[A-Za-z0-9_.-]+$`)
	packagePattern     = regexp.MustCompile(`^sitectl(?:-[a-z0-9]+)*$`)
	archiveNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	shaPattern         = regexp.MustCompile(`^[0-9a-f]{40}$`)
	checksumPattern    = regexp.MustCompile(`^([0-9a-fA-F]{64})  \*?([A-Za-z0-9][A-Za-z0-9._-]*)$`)
	digestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	licensePattern     = regexp.MustCompile(`^[A-Za-z0-9.+-]+$`)
	dependencyPattern  = regexp.MustCompile(`^libops/homebrew/sitectl(?:-[a-z0-9][a-z0-9-]*)?$`)
)

type config struct {
	APIBase        string
	SourceRepo     string
	HomebrewRepo   string
	HomebrewRemote string
	PackageName    string
	ReleaseMode    string
	ReleaseVersion string
	RefName        string
	RefType        string
	SourceToken    string
	HomebrewToken  string
	WorkRoot       string
}

type reconciler struct {
	config config
	client *http.Client
	git    gitRunner
}

type gitRunner struct {
	token string
}

type release struct {
	ID          int64          `json:"id"`
	TagName     string         `json:"tag_name"`
	Draft       bool           `json:"draft"`
	Prerelease  bool           `json:"prerelease"`
	PublishedAt time.Time      `json:"published_at"`
	Assets      []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	State  string `json:"state"`
	Digest string `json:"digest"`
}

type gitObject struct {
	SHA  string `json:"sha"`
	Type string `json:"type"`
}

type gitRef struct {
	Object gitObject `json:"object"`
}

type annotatedTag struct {
	Object gitObject `json:"object"`
}

type gitCommit struct {
	Committer struct {
		Date time.Time `json:"date"`
	} `json:"committer"`
}

type repository struct {
	DefaultBranch string `json:"default_branch"`
}

type pullRequest struct {
	Number int    `json:"number"`
	State  string `json:"state"`
	Head   struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

type pullFile struct {
	Filename string `json:"filename"`
}

type formulaMetadata struct {
	Description  string
	Homepage     string
	License      string
	Dependencies []string
}

var canonicalDescriptions = map[string]string{
	"sitectl":               "CLI for managing local and remote Docker Compose projects",
	"sitectl-app-tmpl":      "A sitectl plugin template for application Compose stacks",
	"sitectl-archivesspace": "A sitectl plugin for ArchivesSpace stacks",
	"sitectl-drupal":        "A sitectl plugin for Drupal websites",
	"sitectl-isle":          "A sitectl plugin for Islandora stacks",
	"sitectl-libops":        "A sitectl plugin for LibOps platform operations",
	"sitectl-ojs":           "A sitectl plugin for Open Journal Systems stacks",
	"sitectl-omeka-classic": "A sitectl plugin for Omeka Classic stacks",
	"sitectl-omeka-s":       "A sitectl plugin for Omeka S stacks",
	"sitectl-wp":            "A sitectl plugin for WordPress stacks",
}

type semver struct {
	Major uint64
	Minor uint64
	Patch uint64
}

type resolvedRelease struct {
	Version    string
	TagCommit  string
	CommitDate time.Time
	Checksums  map[string]string
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cfg, err := configFromEnvironment()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	r := newReconciler(cfg)
	if err := r.run(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func configFromEnvironment() (config, error) {
	cfg := config{
		APIBase:        valueOrDefault(os.Getenv("GITHUB_API_URL"), defaultAPIBase),
		SourceRepo:     os.Getenv("GITHUB_REPOSITORY"),
		HomebrewRepo:   valueOrDefault(os.Getenv("HOMEBREW_REPOSITORY"), defaultHomebrewRepo),
		HomebrewRemote: valueOrDefault(os.Getenv("HOMEBREW_REMOTE_URL"), defaultHomebrewRemote),
		PackageName:    os.Getenv("PACKAGE_NAME"),
		ReleaseMode:    os.Getenv("RELEASE_MODE"),
		ReleaseVersion: os.Getenv("RELEASE_VERSION"),
		RefName:        os.Getenv("REF_NAME"),
		RefType:        os.Getenv("REF_TYPE"),
		SourceToken:    os.Getenv("GH_TOKEN"),
		HomebrewToken:  os.Getenv("HOMEBREW_TOKEN"),
		WorkRoot:       os.Getenv("RUNNER_TEMP"),
	}
	if cfg.SourceToken == "" {
		return config{}, errors.New("GH_TOKEN is required")
	}
	if cfg.HomebrewToken == "" {
		return config{}, errors.New("HOMEBREW_TOKEN is required")
	}
	if cfg.WorkRoot == "" {
		cfg.WorkRoot = os.TempDir()
	}
	return cfg, nil
}

func newReconciler(cfg config) *reconciler {
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many HTTP redirects")
			}
			return nil
		},
	}
	return &reconciler{
		config: cfg,
		client: client,
		git:    gitRunner{token: cfg.HomebrewToken},
	}
}

func (r *reconciler) run(ctx context.Context) error {
	version, err := r.validateInputs(ctx)
	if err != nil {
		return fmt.Errorf("validate reconciliation inputs: %w", err)
	}
	releaseInfo, err := r.resolveRelease(ctx, version)
	if err != nil {
		return fmt.Errorf("resolve release %s: %w", version, err)
	}

	workDir, err := os.MkdirTemp(r.config.WorkRoot, "homebrew-reconcile-")
	if err != nil {
		return fmt.Errorf("create reconciliation workspace: %w", err)
	}
	defer os.RemoveAll(workDir)

	repoDir := filepath.Join(workDir, "homebrew")
	branch := r.config.PackageName + "-" + strings.TrimPrefix(version, "v")
	remoteMain, remoteBranch, err := r.prepareRepository(ctx, repoDir, branch)
	if err != nil {
		return err
	}

	formulaName := r.config.PackageName + ".rb"
	currentFormula, currentExists, err := readRegularFile(repoDir, formulaName)
	if err != nil {
		return fmt.Errorf("read current Homebrew formula: %w", err)
	}
	metadata, err := formulaMetadataFor(r.config.SourceRepo, r.config.PackageName, currentFormula, currentExists)
	if err != nil {
		return fmt.Errorf("resolve Homebrew metadata: %w", err)
	}
	desiredFormula, err := renderFormula(r.config.SourceRepo, r.config.PackageName, releaseInfo, metadata)
	if err != nil {
		return fmt.Errorf("render Homebrew formula: %w", err)
	}

	if currentExists {
		currentVersion, err := formulaVersion(currentFormula)
		if err != nil {
			return fmt.Errorf("parse current Homebrew formula version: %w", err)
		}
		currentSemver, err := parseSemver(currentVersion)
		if err != nil {
			return fmt.Errorf("parse current Homebrew version: %w", err)
		}
		releaseSemver, _ := parseSemver(strings.TrimPrefix(version, "v"))
		if currentSemver.compare(releaseSemver) > 0 {
			return fmt.Errorf("refusing to downgrade %s from %s to %s", formulaName, currentVersion, strings.TrimPrefix(version, "v"))
		}
		if string(currentFormula) == desiredFormula {
			fmt.Printf("%s already publishes %s from verified release assets\n", formulaName, version)
			return nil
		}
	}

	commitSHA, err := r.commitFormula(ctx, repoDir, branch, formulaName, desiredFormula, releaseInfo)
	if err != nil {
		return err
	}
	if err := r.requireRemoteMain(ctx, remoteMain); err != nil {
		return err
	}
	if commitSHA != remoteBranch {
		if err := r.pushBranch(ctx, repoDir, branch, remoteBranch, commitSHA); err != nil {
			return err
		}
	}
	if err := r.ensurePullRequest(ctx, branch, formulaName, commitSHA, remoteMain, releaseInfo); err != nil {
		return err
	}
	fmt.Printf("Homebrew reconciliation is represented by %s at %s\n", branch, commitSHA)
	return nil
}

func (r *reconciler) validateInputs(ctx context.Context) (string, error) {
	if !repositoryPattern.MatchString(r.config.SourceRepo) {
		return "", errors.New("GITHUB_REPOSITORY must name a libops repository")
	}
	if !repositoryPattern.MatchString(r.config.HomebrewRepo) || r.config.HomebrewRepo != defaultHomebrewRepo {
		return "", errors.New("HOMEBREW_REPOSITORY must be libops/homebrew")
	}
	if !packagePattern.MatchString(r.config.PackageName) {
		return "", errors.New("PACKAGE_NAME must be a canonical sitectl package name")
	}
	if pathBase(r.config.SourceRepo) != r.config.PackageName {
		return "", errors.New("PACKAGE_NAME must match the source repository name")
	}
	switch r.config.ReleaseMode {
	case "full":
		if r.config.RefType != "tag" || r.config.ReleaseVersion != "" {
			return "", errors.New("full releases require a tag ref and no release-version input")
		}
		if _, err := parseTaggedSemver(r.config.RefName); err != nil {
			return "", fmt.Errorf("full release ref: %w", err)
		}
		return r.config.RefName, nil
	case "homebrew-only":
		if _, err := parseTaggedSemver(r.config.ReleaseVersion); err != nil {
			return "", fmt.Errorf("homebrew-only release-version: %w", err)
		}
		var repo repository
		if err := r.apiJSON(ctx, r.config.SourceToken, http.MethodGet, "/repos/"+r.config.SourceRepo, nil, &repo); err != nil {
			return "", fmt.Errorf("resolve source default branch: %w", err)
		}
		if repo.DefaultBranch == "" || r.config.RefType != "branch" || r.config.RefName != repo.DefaultBranch {
			return "", fmt.Errorf("homebrew-only recovery must run from the repository default branch (%s)", repo.DefaultBranch)
		}
		return r.config.ReleaseVersion, nil
	default:
		return "", errors.New("RELEASE_MODE must be full or homebrew-only")
	}
}

func (r *reconciler) resolveRelease(ctx context.Context, version string) (resolvedRelease, error) {
	var published release
	if err := r.apiJSON(
		ctx,
		r.config.SourceToken,
		http.MethodGet,
		"/repos/"+r.config.SourceRepo+"/releases/tags/"+url.PathEscape(version),
		nil,
		&published,
	); err != nil {
		return resolvedRelease{}, err
	}
	if published.ID <= 0 || published.TagName != version || published.Draft || published.Prerelease || published.PublishedAt.IsZero() {
		return resolvedRelease{}, errors.New("release must be a published, non-prerelease release for the exact tag")
	}
	tagCommit, commitDate, err := r.resolveTagCommit(ctx, version)
	if err != nil {
		return resolvedRelease{}, err
	}

	requiredNames := archiveNames(r.config.PackageName)
	requiredNames = append(requiredNames, "checksums.txt")
	assets := make(map[string]releaseAsset, len(requiredNames))
	required := make(map[string]bool, len(requiredNames))
	assetIDs := make(map[int64]string, len(requiredNames))
	for _, name := range requiredNames {
		required[name] = true
	}
	for _, asset := range published.Assets {
		if !required[asset.Name] {
			continue
		}
		if _, duplicate := assets[asset.Name]; duplicate {
			return resolvedRelease{}, fmt.Errorf("release contains duplicate asset %s", asset.Name)
		}
		if asset.ID <= 0 || asset.Size <= 0 || asset.State != "uploaded" {
			return resolvedRelease{}, fmt.Errorf("release asset %s is not an uploaded, nonempty asset", asset.Name)
		}
		if !digestPattern.MatchString(asset.Digest) {
			return resolvedRelease{}, fmt.Errorf("release asset %s has no valid sha256 GitHub digest", asset.Name)
		}
		if priorName, duplicate := assetIDs[asset.ID]; duplicate {
			return resolvedRelease{}, fmt.Errorf("release assets %s and %s share asset ID %d", priorName, asset.Name, asset.ID)
		}
		assetIDs[asset.ID] = asset.Name
		assets[asset.Name] = asset
	}
	for _, name := range requiredNames {
		if _, ok := assets[name]; !ok {
			return resolvedRelease{}, fmt.Errorf("release is missing required asset %s", name)
		}
	}

	checksumBytes, err := r.downloadAsset(ctx, assets["checksums.txt"], maxChecksumAsset)
	if err != nil {
		return resolvedRelease{}, fmt.Errorf("download checksums.txt by asset ID: %w", err)
	}
	checksums, err := parseChecksums(checksumBytes)
	if err != nil {
		return resolvedRelease{}, err
	}
	for _, name := range archiveNames(r.config.PackageName) {
		expected, ok := checksums[name]
		if !ok {
			return resolvedRelease{}, fmt.Errorf("checksums.txt is missing %s", name)
		}
		archiveBytes, err := r.downloadAsset(ctx, assets[name], maxArchiveAsset)
		if err != nil {
			return resolvedRelease{}, fmt.Errorf("download %s by asset ID: %w", name, err)
		}
		actual := fmt.Sprintf("%x", sha256.Sum256(archiveBytes))
		if actual != expected {
			return resolvedRelease{}, fmt.Errorf("%s checksum mismatch: got %s, want %s", name, actual, expected)
		}
		if err := validateArchive(archiveBytes, r.config.PackageName); err != nil {
			return resolvedRelease{}, fmt.Errorf("validate %s: %w", name, err)
		}
	}
	return resolvedRelease{
		Version:    version,
		TagCommit:  tagCommit,
		CommitDate: commitDate,
		Checksums:  checksums,
	}, nil
}

func (r *reconciler) resolveTagCommit(ctx context.Context, version string) (string, time.Time, error) {
	var ref gitRef
	if err := r.apiJSON(
		ctx,
		r.config.SourceToken,
		http.MethodGet,
		"/repos/"+r.config.SourceRepo+"/git/ref/tags/"+url.PathEscape(version),
		nil,
		&ref,
	); err != nil {
		return "", time.Time{}, fmt.Errorf("resolve tag ref: %w", err)
	}
	object := ref.Object
	for depth := 0; depth < 8 && object.Type == "tag"; depth++ {
		if !shaPattern.MatchString(object.SHA) {
			return "", time.Time{}, errors.New("annotated tag object has an invalid SHA")
		}
		var tag annotatedTag
		if err := r.apiJSON(
			ctx,
			r.config.SourceToken,
			http.MethodGet,
			"/repos/"+r.config.SourceRepo+"/git/tags/"+object.SHA,
			nil,
			&tag,
		); err != nil {
			return "", time.Time{}, fmt.Errorf("resolve annotated tag: %w", err)
		}
		object = tag.Object
	}
	if object.Type != "commit" || !shaPattern.MatchString(object.SHA) {
		return "", time.Time{}, errors.New("release tag does not resolve to a commit")
	}
	var commit gitCommit
	if err := r.apiJSON(
		ctx,
		r.config.SourceToken,
		http.MethodGet,
		"/repos/"+r.config.SourceRepo+"/git/commits/"+object.SHA,
		nil,
		&commit,
	); err != nil {
		return "", time.Time{}, fmt.Errorf("resolve tag commit: %w", err)
	}
	if commit.Committer.Date.IsZero() {
		return "", time.Time{}, errors.New("tag commit has no committer date")
	}
	return object.SHA, commit.Committer.Date.UTC(), nil
}

func (r *reconciler) downloadAsset(ctx context.Context, asset releaseAsset, limit int64) ([]byte, error) {
	requestPath := fmt.Sprintf("/repos/%s/releases/assets/%d", r.config.SourceRepo, asset.ID)
	req, err := r.newRequest(ctx, r.config.SourceToken, http.MethodGet, requestPath, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	response, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub returned %s", response.Status)
	}
	if response.ContentLength > limit {
		return nil, fmt.Errorf("asset exceeds %d bytes", limit)
	}
	data, err := readLimited(response.Body, limit)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != asset.Size {
		return nil, fmt.Errorf("asset size mismatch: got %d, want %d", len(data), asset.Size)
	}
	actual := fmt.Sprintf("%x", sha256.Sum256(data))
	if actual != strings.TrimPrefix(asset.Digest, "sha256:") {
		return nil, errors.New("asset does not match its GitHub digest")
	}
	return data, nil
}

func (r *reconciler) prepareRepository(ctx context.Context, repoDir, branch string) (string, string, error) {
	if err := os.MkdirAll(repoDir, 0700); err != nil {
		return "", "", fmt.Errorf("create Homebrew checkout: %w", err)
	}
	for _, args := range [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", r.config.HomebrewRemote},
		{"fetch", "--quiet", "--no-tags", "--depth=1", "origin", "+refs/heads/main:refs/remotes/origin/main"},
		{"checkout", "--quiet", "--detach", "refs/remotes/origin/main"},
	} {
		if _, err := r.git.run(ctx, repoDir, nil, args...); err != nil {
			return "", "", fmt.Errorf("prepare Homebrew repository with git %s: %w", args[0], err)
		}
	}
	mainSHA, err := r.git.revParse(ctx, repoDir, "refs/remotes/origin/main")
	if err != nil {
		return "", "", fmt.Errorf("resolve Homebrew main: %w", err)
	}
	branchSHA, err := r.remoteBranchSHA(ctx, repoDir, branch)
	if err != nil {
		return "", "", err
	}
	return mainSHA, branchSHA, nil
}

func (r *reconciler) remoteBranchSHA(ctx context.Context, repoDir, branch string) (string, error) {
	output, err := r.git.run(ctx, repoDir, nil, "ls-remote", "--heads", "origin", "refs/heads/"+branch)
	if err != nil {
		return "", fmt.Errorf("inspect remote Homebrew branch: %w", err)
	}
	line := strings.TrimSpace(output)
	if line == "" {
		return "", nil
	}
	fields := strings.Fields(line)
	if len(fields) != 2 || !shaPattern.MatchString(fields[0]) || fields[1] != "refs/heads/"+branch {
		return "", errors.New("remote Homebrew branch returned an unexpected ref")
	}
	return fields[0], nil
}

func (r *reconciler) commitFormula(
	ctx context.Context,
	repoDir, branch, formulaName, desired string,
	releaseInfo resolvedRelease,
) (string, error) {
	if _, err := r.git.run(ctx, repoDir, nil, "checkout", "--quiet", "-B", branch, "refs/remotes/origin/main"); err != nil {
		return "", fmt.Errorf("create Homebrew reconciliation branch: %w", err)
	}
	formulaPath := filepath.Join(repoDir, formulaName)
	if err := os.WriteFile(formulaPath, []byte(desired), 0600); err != nil {
		return "", fmt.Errorf("write reconciled formula: %w", err)
	}
	if _, err := r.git.run(ctx, repoDir, nil, "add", "--", formulaName); err != nil {
		return "", fmt.Errorf("stage reconciled formula: %w", err)
	}
	names, err := r.git.run(ctx, repoDir, nil, "diff", "--cached", "--name-only")
	if err != nil {
		return "", fmt.Errorf("inspect reconciled formula diff: %w", err)
	}
	if strings.TrimSpace(names) != formulaName {
		return "", fmt.Errorf("reconciliation must change exactly %s", formulaName)
	}
	for _, args := range [][]string{
		{"config", "user.name", "sitectl-dev[bot]"},
		{"config", "user.email", "2408410+sitectl-dev[bot]@users.noreply.github.com"},
	} {
		if _, err := r.git.run(ctx, repoDir, nil, args...); err != nil {
			return "", fmt.Errorf("configure Homebrew commit identity: %w", err)
		}
	}
	commitDate := releaseInfo.CommitDate.Format(time.RFC3339)
	env := []string{
		"GIT_AUTHOR_DATE=" + commitDate,
		"GIT_COMMITTER_DATE=" + commitDate,
	}
	message := fmt.Sprintf("%s %s from %s", r.config.PackageName, releaseInfo.Version, releaseInfo.TagCommit[:12])
	if _, err := r.git.run(ctx, repoDir, env, "commit", "--quiet", "-m", message); err != nil {
		return "", fmt.Errorf("commit reconciled formula: %w", err)
	}
	commitSHA, err := r.git.revParse(ctx, repoDir, "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolve reconciled formula commit: %w", err)
	}
	committed, err := r.git.run(ctx, repoDir, nil, "show", "HEAD:"+formulaName)
	if err != nil {
		return "", fmt.Errorf("read committed formula: %w", err)
	}
	if committed != desired {
		return "", errors.New("committed Homebrew formula differs from verified release data")
	}
	return commitSHA, nil
}

func (r *reconciler) requireRemoteMain(ctx context.Context, expected string) error {
	workDir, err := os.MkdirTemp(r.config.WorkRoot, "homebrew-main-check-")
	if err != nil {
		return fmt.Errorf("create Homebrew main verification workspace: %w", err)
	}
	defer os.RemoveAll(workDir)
	if _, err := r.git.run(ctx, workDir, nil, "init", "--quiet"); err != nil {
		return fmt.Errorf("initialize Homebrew main verification: %w", err)
	}
	if _, err := r.git.run(ctx, workDir, nil, "remote", "add", "origin", r.config.HomebrewRemote); err != nil {
		return fmt.Errorf("configure Homebrew main verification: %w", err)
	}
	output, err := r.git.run(ctx, workDir, nil, "ls-remote", "--heads", "origin", "refs/heads/main")
	if err != nil {
		return fmt.Errorf("verify current Homebrew main: %w", err)
	}
	fields := strings.Fields(strings.TrimSpace(output))
	if len(fields) != 2 || fields[0] != expected || fields[1] != "refs/heads/main" {
		return errors.New("Homebrew main changed during reconciliation; rerun against the new base")
	}
	return nil
}

func (r *reconciler) pushBranch(ctx context.Context, repoDir, branch, expectedOldSHA, commitSHA string) error {
	lease := "refs/heads/" + branch + ":" + expectedOldSHA
	if _, err := r.git.run(
		ctx,
		repoDir,
		nil,
		"push",
		"--quiet",
		"--force-with-lease="+lease,
		"origin",
		"HEAD:refs/heads/"+branch,
	); err != nil {
		return fmt.Errorf("repair Homebrew branch with force-with-lease: %w", err)
	}
	actual, err := r.remoteBranchSHA(ctx, repoDir, branch)
	if err != nil {
		return err
	}
	if actual != commitSHA {
		return fmt.Errorf("remote Homebrew branch is %s after push, want %s", actual, commitSHA)
	}
	return nil
}

func (r *reconciler) ensurePullRequest(
	ctx context.Context,
	branch, formulaName, commitSHA, expectedBaseSHA string,
	releaseInfo resolvedRelease,
) error {
	query := url.Values{
		"state":    {"all"},
		"head":     {"libops:" + branch},
		"base":     {"main"},
		"per_page": {"100"},
	}
	var pulls []pullRequest
	if err := r.apiJSON(
		ctx,
		r.config.HomebrewToken,
		http.MethodGet,
		"/repos/"+r.config.HomebrewRepo+"/pulls?"+query.Encode(),
		nil,
		&pulls,
	); err != nil {
		return fmt.Errorf("list Homebrew pull requests: %w", err)
	}
	var openSameRepo []pullRequest
	for _, pull := range pulls {
		if pull.State == "open" &&
			pull.Head.Ref == branch &&
			pull.Head.Repo.FullName == r.config.HomebrewRepo &&
			pull.Base.Ref == "main" &&
			pull.Base.Repo.FullName == r.config.HomebrewRepo {
			openSameRepo = append(openSameRepo, pull)
		}
	}
	if len(openSameRepo) > 1 {
		return errors.New("multiple same-repository Homebrew pull requests target the reconciliation branch")
	}

	var pull pullRequest
	if len(openSameRepo) == 1 {
		pull = openSameRepo[0]
	} else {
		body := map[string]string{
			"title": fmt.Sprintf("%s %s", r.config.PackageName, releaseInfo.Version),
			"head":  branch,
			"base":  "main",
			"body": fmt.Sprintf(
				"Reconciles `%s` from published release `%s` at source commit `%s`. "+
					"The four archives were downloaded by exact release asset ID and verified against `checksums.txt`.",
				formulaName,
				releaseInfo.Version,
				releaseInfo.TagCommit,
			),
		}
		if err := r.apiJSON(
			ctx,
			r.config.HomebrewToken,
			http.MethodPost,
			"/repos/"+r.config.HomebrewRepo+"/pulls",
			body,
			&pull,
		); err != nil {
			return fmt.Errorf("create Homebrew pull request: %w", err)
		}
	}
	if pull.Number <= 0 {
		return errors.New("Homebrew pull request has no number")
	}

	var current pullRequest
	if err := r.apiJSON(
		ctx,
		r.config.HomebrewToken,
		http.MethodGet,
		fmt.Sprintf("/repos/%s/pulls/%d", r.config.HomebrewRepo, pull.Number),
		nil,
		&current,
	); err != nil {
		return fmt.Errorf("resolve Homebrew pull request: %w", err)
	}
	if current.State != "open" ||
		current.Head.Ref != branch ||
		current.Head.SHA != commitSHA ||
		current.Head.Repo.FullName != r.config.HomebrewRepo ||
		current.Base.Ref != "main" ||
		current.Base.SHA != expectedBaseSHA ||
		current.Base.Repo.FullName != r.config.HomebrewRepo {
		return errors.New("Homebrew pull request does not represent the exact same-repository reconciliation commit and base")
	}
	var files []pullFile
	if err := r.apiJSON(
		ctx,
		r.config.HomebrewToken,
		http.MethodGet,
		fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=100", r.config.HomebrewRepo, pull.Number),
		nil,
		&files,
	); err != nil {
		return fmt.Errorf("list Homebrew pull request files: %w", err)
	}
	if len(files) != 1 || files[0].Filename != formulaName {
		return fmt.Errorf("Homebrew pull request must change exactly %s", formulaName)
	}
	return nil
}

func (r *reconciler) apiJSON(
	ctx context.Context,
	token, method, requestPath string,
	body any,
	target any,
) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = strings.NewReader(string(encoded))
	}
	req, err := r.newRequest(ctx, token, method, requestPath, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		data, _ := readLimited(response.Body, 64<<10)
		return fmt.Errorf("GitHub returned %s: %s", response.Status, strings.TrimSpace(string(data)))
	}
	data, err := readLimited(response.Body, maxAPIResponse)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("decode GitHub response: %w", err)
	}
	return nil
}

func (r *reconciler) newRequest(
	ctx context.Context,
	token, method, requestPath string,
	body io.Reader,
) (*http.Request, error) {
	if token == "" {
		return nil, errors.New("GitHub API token is empty")
	}
	base, err := url.Parse(r.config.APIBase)
	if err != nil {
		return nil, err
	}
	relative, err := url.Parse(requestPath)
	if err != nil {
		return nil, err
	}
	endpoint := base.ResolveReference(relative)
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "libops-homebrew-reconcile")
	return req, nil
}

func (g gitRunner) run(
	ctx context.Context,
	dir string,
	extraEnv []string,
	args ...string,
) (string, error) {
	askpassDir, err := os.MkdirTemp("", "libops-git-askpass-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(askpassDir)
	askpass := filepath.Join(askpassDir, "askpass")
	const askpassScript = `#!/bin/sh
case "$1" in
  *Username*) printf '%s\n' x-access-token ;;
  *Password*) printf '%s\n' "$HOMEBREW_TOKEN" ;;
  *) exit 1 ;;
esac
` // #nosec G101 -- x-access-token is GitHub's fixed username; the secret is read from the child environment.
	// The credential helper must be executable and contains no credential bytes.
	// #nosec G306
	if err := os.WriteFile(askpass, []byte(askpassScript), 0700); err != nil {
		return "", err
	}
	// Git is invoked directly without a shell. Every dynamic ref/path argument is
	// either a fixed workflow value or validated before reaching this runner.
	// #nosec G204
	command := exec.CommandContext(ctx, "git", args...)
	command.Dir = dir
	command.Env = append(
		os.Environ(),
		"GIT_ASKPASS="+askpass,
		"GIT_TERMINAL_PROMPT=0",
		"HOMEBREW_TOKEN="+g.token,
	)
	command.Env = append(command.Env, extraEnv...)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func (g gitRunner) revParse(ctx context.Context, dir, revision string) (string, error) {
	output, err := g.run(ctx, dir, nil, "rev-parse", "--verify", revision+"^{commit}")
	if err != nil {
		return "", err
	}
	sha := strings.TrimSpace(output)
	if !shaPattern.MatchString(sha) {
		return "", errors.New("git returned an invalid commit SHA")
	}
	return sha, nil
}

func parseChecksums(contents []byte) (map[string]string, error) {
	if len(contents) == 0 {
		return nil, errors.New("checksums.txt is empty")
	}
	checksums := make(map[string]string)
	for lineNumber, line := range strings.Split(strings.TrimSuffix(string(contents), "\n"), "\n") {
		if line == "" {
			return nil, fmt.Errorf("checksums.txt contains an empty line at %d", lineNumber+1)
		}
		match := checksumPattern.FindStringSubmatch(line)
		if match == nil {
			return nil, fmt.Errorf("checksums.txt line %d is malformed", lineNumber+1)
		}
		name := match[2]
		if _, duplicate := checksums[name]; duplicate {
			return nil, fmt.Errorf("checksums.txt contains duplicate entry %s", name)
		}
		checksums[name] = strings.ToLower(match[1])
	}
	return checksums, nil
}

func validateArchive(contents []byte, packageName string) error {
	compressed := bytes.NewReader(contents)
	gzipReader, err := gzip.NewReader(compressed)
	if err != nil {
		return fmt.Errorf("open gzip stream: %w", err)
	}
	gzipReader.Multistream(false)
	decompressed, err := readLimited(gzipReader, maxArchiveContents)
	if err != nil {
		return fmt.Errorf("decompress archive: %w", err)
	}
	if err := gzipReader.Close(); err != nil {
		return fmt.Errorf("close gzip stream: %w", err)
	}
	if compressed.Len() != 0 {
		return errors.New("archive contains a second gzip stream or trailing compressed data")
	}
	if err := validateTarEnvelope(decompressed); err != nil {
		return err
	}

	tarSource := bytes.NewReader(decompressed)
	tarReader := tar.NewReader(tarSource)
	seen := make(map[string]bool)
	var total int64
	var binaryFound bool
	for entry := 0; ; entry++ {
		header, err := tarReader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar header: %w", err)
		}
		if entry >= maxArchiveEntries {
			return fmt.Errorf("archive contains more than %d entries", maxArchiveEntries)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return fmt.Errorf("archive member %q is not a regular file", header.Name)
		}
		if header.Name == "" ||
			!archiveNamePattern.MatchString(header.Name) ||
			header.Name != filepath.Base(header.Name) ||
			strings.ContainsAny(header.Name, `/\`) ||
			header.Name == "." ||
			header.Name == ".." {
			return fmt.Errorf("archive member %q is not a root file", header.Name)
		}
		canonicalName := strings.ToLower(header.Name)
		if seen[canonicalName] {
			return fmt.Errorf("archive contains duplicate member %q", header.Name)
		}
		seen[canonicalName] = true
		if header.Mode < 0 || header.Mode&^0777 != 0 {
			return fmt.Errorf("archive member %q has unsafe permission bits", header.Name)
		}
		if header.Size < 0 || header.Size > maxArchiveContents-total {
			return errors.New("archive expands beyond the permitted size")
		}
		written, err := io.Copy(io.Discard, io.LimitReader(tarReader, header.Size+1))
		if err != nil {
			return fmt.Errorf("read archive member %q: %w", header.Name, err)
		}
		if written != header.Size {
			return fmt.Errorf("archive member %q size mismatch", header.Name)
		}
		total += written
		if header.Name == packageName {
			if header.Size == 0 {
				return errors.New("package binary is empty")
			}
			if header.Mode&0111 == 0 {
				return errors.New("package binary is not executable")
			}
			binaryFound = true
		}
	}
	for _, value := range decompressed[len(decompressed)-tarSource.Len():] {
		if value != 0 {
			return errors.New("archive contains data after the tar end marker")
		}
	}
	if !binaryFound {
		return fmt.Errorf("archive does not contain executable root binary %s", packageName)
	}
	return nil
}

func validateTarEnvelope(contents []byte) error {
	const blockSize = 512
	for offset := 0; ; {
		if offset+2*blockSize > len(contents) {
			return errors.New("tar stream is missing its two-block end marker")
		}
		block := contents[offset : offset+blockSize]
		if allZero(block) {
			if !allZero(contents[offset+blockSize : offset+2*blockSize]) {
				return errors.New("tar stream has only one zero end block")
			}
			for _, value := range contents[offset+2*blockSize:] {
				if value != 0 {
					return errors.New("tar stream contains data after its end marker")
				}
			}
			return nil
		}
		size, err := parseTarSize(block[124:136])
		if err != nil {
			return fmt.Errorf("tar header has an invalid size: %w", err)
		}
		paddedSize := (size + blockSize - 1) / blockSize * blockSize
		if size > maxArchiveContents || paddedSize > int64(len(contents)) || int64(offset)+blockSize+paddedSize > int64(len(contents)) {
			return errors.New("tar member exceeds the archive boundary")
		}
		offset += blockSize + int(paddedSize)
	}
}

func parseTarSize(field []byte) (int64, error) {
	value := strings.Trim(string(field), " \x00")
	if value == "" {
		return 0, nil
	}
	for _, character := range value {
		if character < '0' || character > '7' {
			return 0, errors.New("size is not canonical octal")
		}
	}
	size, err := strconv.ParseInt(value, 8, 64)
	if err != nil || size < 0 {
		return 0, errors.New("size is outside the supported range")
	}
	return size, nil
}

func allZero(contents []byte) bool {
	for _, value := range contents {
		if value != 0 {
			return false
		}
	}
	return true
}

func archiveNames(packageName string) []string {
	return []string{
		packageName + "_Darwin_x86_64.tar.gz",
		packageName + "_Darwin_arm64.tar.gz",
		packageName + "_Linux_x86_64.tar.gz",
		packageName + "_Linux_arm64.tar.gz",
	}
}

func formulaMetadataFor(
	sourceRepo, packageName string,
	current []byte,
	currentExists bool,
) (formulaMetadata, error) {
	metadata := formulaMetadata{
		Description: canonicalDescriptions[packageName],
		Homepage:    "https://github.com/" + sourceRepo,
		License:     "MIT",
	}
	if packageName != "sitectl" {
		metadata.Dependencies = append(metadata.Dependencies, "libops/homebrew/sitectl")
	}
	if packageName == "sitectl-isle" {
		metadata.Dependencies = append(metadata.Dependencies, "libops/homebrew/sitectl-drupal")
	}
	if !currentExists {
		if metadata.Description == "" {
			return formulaMetadata{}, errors.New("new package requires trusted Homebrew description metadata")
		}
		return metadata, nil
	}

	lines := strings.Split(string(current), "\n")
	var sawDescription, sawHomepage, sawLicense bool
	var dependencies []string
	for _, line := range lines {
		switch {
		case strings.HasPrefix(line, `  desc "`):
			value, ok := quotedRubyValue(line, "  desc ")
			if !ok || sawDescription {
				return formulaMetadata{}, errors.New("current formula has invalid description metadata")
			}
			if value != "" {
				metadata.Description = value
			}
			sawDescription = true
		case strings.HasPrefix(line, `  homepage "`):
			value, ok := quotedRubyValue(line, "  homepage ")
			if !ok || sawHomepage || value != metadata.Homepage {
				return formulaMetadata{}, errors.New("current formula has invalid homepage metadata")
			}
			sawHomepage = true
		case strings.HasPrefix(line, `  license "`):
			value, ok := quotedRubyValue(line, "  license ")
			if !ok || sawLicense || !licensePattern.MatchString(value) {
				return formulaMetadata{}, errors.New("current formula has invalid license metadata")
			}
			if value != "MIT" {
				return formulaMetadata{}, errors.New("current formula license must be MIT")
			}
			metadata.License = value
			sawLicense = true
		case strings.HasPrefix(line, `  depends_on "`):
			value, ok := quotedRubyValue(line, "  depends_on ")
			if !ok || !dependencyPattern.MatchString(value) || value == "libops/homebrew/"+packageName {
				return formulaMetadata{}, errors.New("current formula has invalid dependency metadata")
			}
			dependencies = append(dependencies, value)
		}
	}
	if !sawDescription || !sawHomepage {
		return formulaMetadata{}, errors.New("current formula is missing description or homepage metadata")
	}
	if metadata.Description == "" {
		return formulaMetadata{}, errors.New("current formula has no trusted nonempty description")
	}
	if len(dependencies) > 0 {
		metadata.Dependencies = uniqueStrings(append(metadata.Dependencies, dependencies...))
	}
	return metadata, nil
}

func renderFormula(
	sourceRepo, packageName string,
	releaseInfo resolvedRelease,
	metadata formulaMetadata,
) (string, error) {
	version := strings.TrimPrefix(releaseInfo.Version, "v")
	if _, err := parseSemver(version); err != nil {
		return "", err
	}
	className, err := formulaClass(packageName)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString("# typed: false\n")
	builder.WriteString("# frozen_string_literal: true\n\n")
	builder.WriteString("# This file was generated from verified GitHub release assets. DO NOT EDIT.\n")
	fmt.Fprintf(&builder, "class %s < Formula\n", className)
	fmt.Fprintf(&builder, "  desc %s\n", rubyQuote(metadata.Description))
	fmt.Fprintf(&builder, "  homepage %s\n", rubyQuote(metadata.Homepage))
	fmt.Fprintf(&builder, "  version %s\n", rubyQuote(version))
	if metadata.License != "" {
		fmt.Fprintf(&builder, "  license %s\n", rubyQuote(metadata.License))
	}
	for _, dependency := range metadata.Dependencies {
		fmt.Fprintf(&builder, "\n  depends_on %s\n", rubyQuote(dependency))
	}
	builder.WriteString("\n  on_macos do\n")
	writeFormulaPlatform(
		&builder,
		sourceRepo,
		releaseInfo.Version,
		packageName+"_Darwin_x86_64.tar.gz",
		releaseInfo.Checksums[packageName+"_Darwin_x86_64.tar.gz"],
		"    if Hardware::CPU.intel?\n",
		packageName,
	)
	writeFormulaPlatform(
		&builder,
		sourceRepo,
		releaseInfo.Version,
		packageName+"_Darwin_arm64.tar.gz",
		releaseInfo.Checksums[packageName+"_Darwin_arm64.tar.gz"],
		"    if Hardware::CPU.arm?\n",
		packageName,
	)
	builder.WriteString("  end\n\n")
	builder.WriteString("  on_linux do\n")
	writeFormulaPlatform(
		&builder,
		sourceRepo,
		releaseInfo.Version,
		packageName+"_Linux_x86_64.tar.gz",
		releaseInfo.Checksums[packageName+"_Linux_x86_64.tar.gz"],
		"    if Hardware::CPU.intel? && Hardware::CPU.is_64_bit?\n",
		packageName,
	)
	writeFormulaPlatform(
		&builder,
		sourceRepo,
		releaseInfo.Version,
		packageName+"_Linux_arm64.tar.gz",
		releaseInfo.Checksums[packageName+"_Linux_arm64.tar.gz"],
		"    if Hardware::CPU.arm? && Hardware::CPU.is_64_bit?\n",
		packageName,
	)
	builder.WriteString("  end\n")
	builder.WriteString("end\n")
	return builder.String(), nil
}

func writeFormulaPlatform(
	builder *strings.Builder,
	sourceRepo, tag, archiveName, checksum, condition, packageName string,
) {
	builder.WriteString(condition)
	fmt.Fprintf(
		builder,
		"      url %s\n",
		rubyQuote("https://github.com/"+sourceRepo+"/releases/download/"+tag+"/"+archiveName),
	)
	fmt.Fprintf(builder, "      sha256 %s\n\n", rubyQuote(checksum))
	builder.WriteString("      define_method(:install) do\n")
	fmt.Fprintf(builder, "        bin.install %s\n", rubyQuote(packageName))
	builder.WriteString("      end\n")
	builder.WriteString("    end\n")
}

func formulaClass(packageName string) (string, error) {
	if !packagePattern.MatchString(packageName) {
		return "", errors.New("invalid formula package name")
	}
	parts := strings.Split(packageName, "-")
	for index, part := range parts {
		parts[index] = strings.ToUpper(part[:1]) + part[1:]
	}
	return strings.Join(parts, ""), nil
}

func formulaVersion(contents []byte) (string, error) {
	var version string
	for _, line := range strings.Split(string(contents), "\n") {
		if !strings.HasPrefix(line, `  version "`) {
			continue
		}
		value, ok := quotedRubyValue(line, "  version ")
		if !ok || version != "" {
			return "", errors.New("formula has invalid or duplicate version metadata")
		}
		version = value
	}
	if version == "" {
		return "", errors.New("formula has no version metadata")
	}
	return version, nil
}

func quotedRubyValue(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix+`"`) || !strings.HasSuffix(line, `"`) {
		return "", false
	}
	value := strings.TrimSuffix(strings.TrimPrefix(line, prefix+`"`), `"`)
	if strings.ContainsAny(value, "\\\r\n") || strings.Contains(value, "#{") {
		return "", false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return "", false
		}
	}
	return value, true
}

func rubyQuote(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "#{", `\#{`)
	return `"` + value + `"`
}

func parseTaggedSemver(value string) (semver, error) {
	if !strings.HasPrefix(value, "v") {
		return semver{}, errors.New("version must use vMAJOR.MINOR.PATCH form")
	}
	return parseSemver(strings.TrimPrefix(value, "v"))
}

func parseSemver(value string) (semver, error) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return semver{}, errors.New("version must use MAJOR.MINOR.PATCH form")
	}
	values := make([]uint64, len(parts))
	for index, part := range parts {
		if part == "" || (len(part) > 1 && part[0] == '0') {
			return semver{}, errors.New("version components must be canonical decimal integers")
		}
		parsed, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return semver{}, errors.New("version components must be decimal integers")
		}
		values[index] = parsed
	}
	return semver{Major: values[0], Minor: values[1], Patch: values[2]}, nil
}

func (v semver) compare(other semver) int {
	for _, pair := range [][2]uint64{
		{v.Major, other.Major},
		{v.Minor, other.Minor},
		{v.Patch, other.Patch},
	} {
		switch {
		case pair[0] < pair[1]:
			return -1
		case pair[0] > pair[1]:
			return 1
		}
	}
	return 0
}

func readRegularFile(rootPath, name string) ([]byte, bool, error) {
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errors.New("formula path is not a regular file")
	}
	contents, err := root.ReadFile(name)
	return contents, true, err
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("response exceeds %d bytes", limit)
	}
	return contents, nil
}

func uniqueStrings(values []string) []string {
	seen := make(map[string]bool)
	result := make([]string, 0, len(values))
	for _, value := range values {
		if seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func valueOrDefault(value, fallback string) string {
	if value != "" {
		return value
	}
	return fallback
}

func pathBase(repository string) string {
	parts := strings.Split(repository, "/")
	return parts[len(parts)-1]
}
