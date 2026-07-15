#!/usr/bin/env bash

set -euo pipefail

tag=""
dockerfile=""
context=""
platforms="linux/amd64,linux/arm64"
builder="default-kratix-image-builder"
push="false"
load="false"
cache_scope=""
extra_tags=()
build_args=()
build_contexts=()

# Remote BuildKit layer cache (GHCR). Override if needed.
CACHE_REPO="${DOCKER_BUILDX_CACHE_REPO:-ghcr.io/syntasso/build-cache}"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --tag)
      tag="$2"; shift 2 ;;
    --dockerfile)
      dockerfile="$2"; shift 2 ;;
    --context)
      context="$2"; shift 2 ;;
    --platforms)
      platforms="$2"; shift 2 ;;
    --builder)
      builder="$2"; shift 2 ;;
    --push)
      push="$2"; shift 2 ;;
    --load)
      load="$2"; shift 2 ;;
    --cache-scope)
      cache_scope="$2"; shift 2 ;;
    --extra-tag)
      extra_tags+=("$2"); shift 2 ;;
    --build-arg)
      build_args+=("--build-arg" "$2"); shift 2 ;;
    --build-context)
      build_contexts+=("--build-context" "$2"); shift 2 ;;
    *)
      echo "Unknown argument: $1" >&2
      exit 1 ;;
  esac
done

if [[ -z "$tag" || -z "$dockerfile" || -z "$context" ]]; then
  echo "tag, dockerfile, and context are required" >&2
  exit 1
fi

if [[ "$push" == "true" && "$load" == "true" ]]; then
  echo "Only one of push or load can be true" >&2
  exit 1
fi

# When remote cache is enabled, prefer the job's buildx builder (from
# docker/setup-buildx-action) so --load works reliably. Otherwise keep a named
# builder for local multi-arch builds.
if [[ "${DOCKER_BUILDX_GHA_ENABLE:-}" == "1" ]]; then
  args=(--platform "$platforms" --file "$dockerfile" -t "$tag")
else
  if docker buildx inspect "$builder" >/dev/null 2>&1; then
    docker buildx use "$builder"
  else
    docker buildx create --name "$builder" --use
  fi
  args=(--builder "$builder" --platform "$platforms" --file "$dockerfile" -t "$tag")
fi

if ((${#extra_tags[@]} > 0)); then
  for t in "${extra_tags[@]}"; do
    args+=(-t "$t")
  done
fi
if ((${#build_args[@]} > 0)); then
  args+=("${build_args[@]}")
fi
if ((${#build_contexts[@]} > 0)); then
  args+=("${build_contexts[@]}")
fi

# Registry cache (GHCR): DOCKER_BUILDX_GHA_ENABLE=1 and --cache-scope <name>.
# (Env name is historical; backend is type=registry — type=gha was a no-op on
# Sprinters runners.) Fork PRs should set DOCKER_BUILDX_GHA_ENABLE=0 so they
# don't attempt cache-to without write access to the cache package.
if [[ "${DOCKER_BUILDX_GHA_ENABLE:-}" == "1" && -n "${cache_scope}" ]]; then
  cache_ref="${CACHE_REPO}:${cache_scope}"
  args+=(--cache-from "type=registry,ref=${cache_ref}")
  # ignore-error: first warm / transient registry issues shouldn't fail the build.
  args+=(--cache-to "type=registry,ref=${cache_ref},mode=max,ignore-error=true")
fi

if [[ "$push" == "true" ]]; then
  args+=(--push)
elif [[ "$load" == "true" ]]; then
  args+=(--load)
fi

docker buildx build "${args[@]}" "$context"
