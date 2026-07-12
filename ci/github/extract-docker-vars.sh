#!/usr/bin/env bash

set -eou pipefail

TAG=$(echo "${GITHUB_REF#refs/heads/}" | sed 's/[^a-zA-Z0-9._-]//g' | awk '{print substr($0, length($0)-120)}')
if [[ "${GITHUB_REF}" == refs/tags/* ]]; then
  TAG=$(echo "${GITHUB_REF#refs/tags/}" | sed 's/[^a-zA-Z0-9._-]//g' | awk '{print substr($0, length($0)-120)}')
fi

PLATFORM="amd64"
if [ "$RUNNER_ARCH" = "ARM64" ]; then
  PLATFORM="arm64"
fi

# If DOCKER_IMAGE ends with /, append the repository name
if [[ "${DOCKER_IMAGE}" == */ ]]; then
  REPO_NAME="${GITHUB_REPOSITORY#*/}"
  DOCKER_IMAGE="${DOCKER_IMAGE}${REPO_NAME}"
fi

# Keep BuildKit cache credentials and storage with the selected image registry.
# For the default GHCR prefix this resolves to the historical cache location.
CACHE_IMAGE="$DOCKER_IMAGE"

CACHE_TO=""
if [ "${TAG}" = "main" ]; then
  CACHE_TO="type=registry,ref=$CACHE_IMAGE:cache-$PLATFORM,mode=max"
fi

CACHE_FROM="type=registry,ref=$CACHE_IMAGE:cache-$PLATFORM"

{
  echo "image=$DOCKER_IMAGE"
  echo "platform=$PLATFORM"
  echo "tag=$TAG"
  echo "cache-to=$CACHE_TO"
  echo "cache-from=$CACHE_FROM"
} >> "$GITHUB_OUTPUT"
