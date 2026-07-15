#!/usr/bin/env bash
set -euo pipefail
set -f

error() {
  echo "::error::$*"
  exit 1
}

packages_input="${PACKAGES:-}"
versions_input="${PACKAGE_VERSIONS:-}"
allow_unversioned="${ALLOW_UNVERSIONED:-true}"

if [[ "${packages_input}" == *$'\n'* || "${packages_input}" == *$'\r'* ]]; then
  error "packages must be a single space-separated line"
fi
if [[ "${versions_input}" == *$'\n'* || "${versions_input}" == *$'\r'* ]]; then
  error "package-versions must be a single space-separated line"
fi

case "${allow_unversioned}" in
  true|false)
    ;;
  *)
    error "allow-unversioned must be true or false"
    ;;
esac

if [ -z "${packages_input//[[:space:]]/}" ]; then
  error "packages input cannot be empty"
fi

read -r -a packages <<< "${packages_input}"
if [ "${#packages[@]}" -eq 0 ]; then
  error "packages input cannot be empty"
fi

declare -A requested=()
for package in "${packages[@]}"; do
  if [[ ! "${package}" =~ ^sitectl(-[A-Za-z0-9][A-Za-z0-9.+_-]*)?$ ]]; then
    error "Invalid sitectl package: ${package}"
  fi
  if [[ -n "${requested[${package}]+present}" ]]; then
    error "Duplicate sitectl package: ${package}"
  fi
  requested["${package}"]=1
done

install_args=()
if [ -n "${versions_input//[[:space:]]/}" ]; then
  read -r -a assignments <<< "${versions_input}"
  declare -A versions=()
  semver='(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-((0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)(\.(0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*))*))?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?'
  for assignment in "${assignments[@]}"; do
    package="${assignment%%=*}"
    version="${assignment#*=}"
    if [ "${package}" = "${assignment}" ] || [[ ! "${package}" =~ ^sitectl(-[A-Za-z0-9][A-Za-z0-9.+_-]*)?$ ]] || [[ ! "${version}" =~ ^${semver}$ ]]; then
      error "Invalid package-versions assignment: ${assignment}"
    fi
    if [[ -z "${requested[${package}]+present}" ]]; then
      error "package-versions contains an unrequested package: ${package}"
    fi
    if [[ -n "${versions[${package}]+present}" ]]; then
      error "Duplicate package-versions assignment: ${package}"
    fi
    versions["${package}"]="${version}"
  done

  for package in "${packages[@]}"; do
    if [[ -z "${versions[${package}]+present}" ]]; then
      error "package-versions is missing an exact version for ${package}"
    fi
    install_args+=("${package}=${versions[${package}]}")
  done
  echo "Installing exact sitectl package versions: ${install_args[*]}"
else
  if [ "${allow_unversioned}" != true ]; then
    error "package-versions is required when allow-unversioned is false"
  fi
  install_args=("${packages[@]}")
  warning="No exact sitectl package versions were supplied; compatibility mode installs the latest packages available at run time: ${packages[*]}"
  echo "::warning::${warning}"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
    printf '### sitectl package version policy\n\n> [!WARNING]\n> %s\n' "${warning}" >> "${GITHUB_STEP_SUMMARY}"
  fi
fi

key_path="${GITHUB_ACTION_PATH}/sitectl-archive-keyring.asc"
approved_primary_fingerprints=("FBF887BCE093167F499F537BCFB2A9DBD0A2156A")
if [ ! -f "${key_path}" ]; then
  error "Vendored sitectl archive key is missing"
fi
key_details="$(gpg --batch --no-default-keyring --keyring /dev/null --show-keys --with-colons --fingerprint "${key_path}")" \
  || error "Vendored sitectl archive key is invalid"
mapfile -t actual_primary_fingerprints < <(
  awk -F: '
    $1 == "pub" { primary = 1; next }
    primary && $1 == "fpr" { print $10; primary = 0 }
  ' <<< "${key_details}"
)
if [ "${#actual_primary_fingerprints[@]}" -ne "${#approved_primary_fingerprints[@]}" ]; then
  error "Vendored sitectl archive key does not contain the approved primary fingerprints"
fi
for index in "${!approved_primary_fingerprints[@]}"; do
  if [ "${actual_primary_fingerprints[${index}]}" != "${approved_primary_fingerprints[${index}]}" ]; then
    error "Vendored sitectl archive key fingerprint is not approved"
  fi
done

sudo install -d -m 0755 /usr/share/keyrings
sudo gpg --batch --yes --dearmor \
  -o /usr/share/keyrings/sitectl-archive-keyring.gpg \
  "${key_path}"
echo "deb [signed-by=/usr/share/keyrings/sitectl-archive-keyring.gpg] https://packages.libops.io/sitectl ./" \
  | sudo tee /etc/apt/sources.list.d/sitectl.list >/dev/null
sudo apt-get update
sudo apt-get install -y "${install_args[@]}"
