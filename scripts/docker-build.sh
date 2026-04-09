#!/usr/bin/env bash

set -euo pipefail

tag=""
dockerfile=""
context=""
platforms="linux/amd64,linux/arm64"
builder="default-kratix-image-builder"
push="false"
load="false"
extra_tags=()
build_args=()

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
    --extra-tag)
      extra_tags+=("$2"); shift 2 ;;
    --build-arg)
      build_args+=("--build-arg" "$2"); shift 2 ;;
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

if docker buildx inspect "$builder" >/dev/null 2>&1; then
  docker buildx use "$builder"
else
  docker buildx create --name "$builder" --use
fi

args=(--builder "$builder" --platform "$platforms" --file "$dockerfile" -t "$tag")
if ((${#extra_tags[@]} > 0)); then
  for t in "${extra_tags[@]}"; do
    args+=(-t "$t")
  done
fi
if ((${#build_args[@]} > 0)); then
  args+=("${build_args[@]}")
fi

if [[ "$push" == "true" ]]; then
  args+=(--push)
elif [[ "$load" == "true" ]]; then
  args+=(--load)
fi

docker buildx build "${args[@]}" "$context"
