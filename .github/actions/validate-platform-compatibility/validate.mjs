import fs from "node:fs";

function die(message) {
  process.stderr.write(`platform compatibility manifest: ${message}\n`);
  process.exit(1);
}

const args = process.argv.slice(2);
let schemaPath;
let manifestPath;
for (let i = 0; i < args.length; i += 1) {
  if (args[i] === "--schema") {
    schemaPath = args[++i];
  } else if (!manifestPath) {
    manifestPath = args[i];
  } else {
    die(`unexpected argument ${args[i]}`);
  }
}
if (!schemaPath || !manifestPath) die("usage: validate.mjs --schema SCHEMA MANIFEST");

function regularFile(path, name) {
  let stat;
  try {
    stat = fs.lstatSync(path);
  } catch (error) {
    die(`${name} cannot be read: ${error.message}`);
  }
  if (!stat.isFile() || stat.isSymbolicLink()) die(`${name} must be a regular file`);
  if (stat.size > 1024 * 1024) die(`${name} exceeds 1 MiB`);
}

function readJSON(path, name) {
  regularFile(path, name);
  try {
    return JSON.parse(fs.readFileSync(path, "utf8"));
  } catch (error) {
    die(`${name} is not valid JSON: ${error.message}`);
  }
}

const schema = readJSON(schemaPath, "schema");
if (schema.$id !== "https://libops.io/schemas/platform-release.v1.json") {
  die("unsupported schema identity");
}
const manifest = readJSON(manifestPath, "manifest");

const object = (value) => value !== null && typeof value === "object" && !Array.isArray(value);
const sha = /^[0-9a-f]{40}$/;
const semver = /^[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$/;
const repository = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+$/;
const packageName = /^[a-z0-9][a-z0-9-]*$/;
const digest = /^sha256:[0-9a-f]{64}$/;
const image = /^[A-Za-z0-9][A-Za-z0-9._/-]*:[A-Za-z0-9][A-Za-z0-9._-]*@sha256:[0-9a-f]{64}$/;
const runURL = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/actions\/runs\/[0-9]+$/;
const certificateIdentity = /^https:\/\/github\.com\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/\.github\/workflows\/build-push\.ya?ml@[0-9a-f]{40}$/;
const callerWorkflowRef = /^[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/\.github\/workflows\/[A-Za-z0-9_.-]+\.ya?ml@\S+$/;
const families = new Set(["archivesspace", "drupal", "islandora", "ojs", "omeka-classic", "omeka-s", "wordpress"]);

function exactKeys(value, path, required) {
  if (!object(value)) die(`${path} must be an object`);
  const actual = Object.keys(value).sort();
  const expected = [...required].sort();
  if (actual.join("\0") !== expected.join("\0")) {
    die(`${path} keys must be exactly: ${required.join(", ")}`);
  }
}

function source(value, path, extra = []) {
  exactKeys(value, path, ["repository", "commit", ...extra]);
  if (!repository.test(value.repository)) die(`${path}.repository must be an exact GitHub repository URL`);
  if (!sha.test(value.commit)) die(`${path}.commit must be a 40-character lowercase commit SHA`);
}

function packageSource(value, path) {
  source(value, path, ["package", "version"]);
  if (!packageName.test(value.package)) die(`${path}.package is invalid`);
  if (!semver.test(value.version)) die(`${path}.version must be exact SemVer without a v prefix`);
}

exactKeys(manifest, "manifest", ["schemaVersion", "release", "sitectl", "sharedSmokeWorkflow", "applications"]);
if (manifest.schemaVersion !== 1) die("schemaVersion must be 1");
exactKeys(manifest.release, "release", ["id", "status"]);
if (!/^[0-9]{4}\.[0-9]+(?:\.[0-9]+)?$/.test(manifest.release.id)) die("release.id is invalid");
if (!["candidate", "promoted", "revoked"].includes(manifest.release.status)) die("release.status is invalid");
packageSource(manifest.sitectl, "sitectl");
source(manifest.sharedSmokeWorkflow, "sharedSmokeWorkflow");
if (!Array.isArray(manifest.applications) || manifest.applications.length === 0) die("applications must not be empty");

const seenFamilies = new Set();
for (const [index, app] of manifest.applications.entries()) {
  const path = `applications[${index}]`;
  exactKeys(app, path, ["family", "plugin", "composeTemplate", "cloudComposePreset", "images", "evidence"]);
  if (!families.has(app.family)) die(`${path}.family is not supported`);
  if (seenFamilies.has(app.family)) die(`${path}.family duplicates ${app.family}`);
  seenFamilies.add(app.family);
  packageSource(app.plugin, `${path}.plugin`);
  source(app.composeTemplate, `${path}.composeTemplate`, ["contractRevision", "contractDigest"]);
  if (typeof app.composeTemplate.contractRevision !== "string" || app.composeTemplate.contractRevision.length === 0 || app.composeTemplate.contractRevision.length > 200) {
    die(`${path}.composeTemplate.contractRevision is invalid`);
  }
  if (!digest.test(app.composeTemplate.contractDigest)) die(`${path}.composeTemplate.contractDigest must be sha256`);
  source(app.cloudComposePreset, `${path}.cloudComposePreset`, ["preset"]);
  if (!/^[a-z0-9][a-z0-9-]*$/.test(app.cloudComposePreset.preset)) die(`${path}.cloudComposePreset.preset is invalid`);
  if (!Array.isArray(app.images) || app.images.length === 0) die(`${path}.images must not be empty`);
  const services = new Set();
  for (const [imageIndex, item] of app.images.entries()) {
    const imagePath = `${path}.images[${imageIndex}]`;
    exactKeys(item, imagePath, ["service", "reference", "source", "attestations"]);
    if (!/^[a-z0-9][a-z0-9_-]*$/.test(item.service)) die(`${imagePath}.service is invalid`);
    if (services.has(item.service)) die(`${imagePath}.service duplicates ${item.service}`);
    services.add(item.service);
    if (!image.test(item.reference)) die(`${imagePath}.reference must contain an exact tag and sha256 digest`);
    source(item.source, `${imagePath}.source`);
    const attestationsPath = `${imagePath}.attestations`;
    exactKeys(item.attestations, attestationsPath, ["certificateIdentity", "callerWorkflowRef", "sbom", "provenance"]);
    if (!certificateIdentity.test(item.attestations.certificateIdentity)) die(`${attestationsPath}.certificateIdentity must bind the exact shared publisher commit`);
    if (!callerWorkflowRef.test(item.attestations.callerWorkflowRef)) die(`${attestationsPath}.callerWorkflowRef must identify the caller workflow and ref`);
    exactKeys(item.attestations.sbom, `${attestationsPath}.sbom`, ["predicateType", "platforms", "verificationRun"]);
    if (item.attestations.sbom.predicateType !== "https://spdx.dev/Document") die(`${attestationsPath}.sbom.predicateType is invalid`);
    if (!Array.isArray(item.attestations.sbom.platforms) || [...item.attestations.sbom.platforms].sort().join("\0") !== "linux/amd64\0linux/arm64") {
      die(`${attestationsPath}.sbom.platforms must contain exactly linux/amd64 and linux/arm64`);
    }
    if (!runURL.test(item.attestations.sbom.verificationRun)) die(`${attestationsPath}.sbom.verificationRun is invalid`);
    exactKeys(item.attestations.provenance, `${attestationsPath}.provenance`, ["predicateType", "verificationRun"]);
    if (item.attestations.provenance.predicateType !== "https://slsa.dev/provenance/v1") die(`${attestationsPath}.provenance.predicateType is invalid`);
    if (!runURL.test(item.attestations.provenance.verificationRun)) die(`${attestationsPath}.provenance.verificationRun is invalid`);
  }
  exactKeys(app.evidence, `${path}.evidence`, ["contractTestRun", "smokeTestRun", "verifyChecks"]);
  if (!runURL.test(app.evidence.contractTestRun)) die(`${path}.evidence.contractTestRun is invalid`);
  if (!runURL.test(app.evidence.smokeTestRun)) die(`${path}.evidence.smokeTestRun is invalid`);
  if (!Array.isArray(app.evidence.verifyChecks) || app.evidence.verifyChecks.length === 0 || app.evidence.verifyChecks.some((check) => typeof check !== "string" || check.trim() === "")) {
    die(`${path}.evidence.verifyChecks must name at least one behavioral check`);
  }
}

process.stdout.write(`validated ${manifest.release.id} (${manifest.release.status}) with ${manifest.applications.length} application tuple(s)\n`);
