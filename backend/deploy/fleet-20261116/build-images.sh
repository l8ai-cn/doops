#!/usr/bin/env bash
set -euo pipefail

# Run on the audited builder against the exact committed backend snapshot.
backend_dir="${1:?backend source directory required}"
source_commit="${DOOPS_SOURCE_COMMIT:?full source commit required}"
release="${DOOPS_RELEASE_TAG:?release tag required}"
repository="${DOOPS_IMAGE_REPOSITORY:?registry/repository required}"
runtime_repository="${DO_AGENT_REPOSITORY:?runtime registry/repository required}"
[[ "$source_commit" =~ ^[a-f0-9]{40}$ ]]
[[ "$release" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]
source "$backend_dir/runtime-versions.env"
runtime_image="$runtime_repository@$DO_AGENT_INDEX_DIGEST"
runtime_labels="$(skopeo inspect --config "docker://$runtime_image")"
python3 -c 'import json,sys; d=json.load(sys.stdin)["config"]["Labels"]; assert d["org.opencontainers.image.revision"]==sys.argv[1]; assert d["org.opencontainers.image.version"]==sys.argv[2]' \
  "$DO_AGENT_REVISION" "$DO_AGENT_VERSION" <<<"$runtime_labels"

build_inputs=(
  --frontend dockerfile.v0
  --local "context=$backend_dir"
  --local "dockerfile=$backend_dir"
  --opt platform=linux/amd64
  --opt "build-arg:DO_AGENT_IMAGE=$runtime_image"
  --opt "build-arg:DO_AGENT_VERSION=$DO_AGENT_VERSION"
  --opt "build-arg:DO_AGENT_REVISION=$DO_AGENT_REVISION"
  --opt "build-arg:DO_AGENT_INDEX_DIGEST=$DO_AGENT_INDEX_DIGEST"
  --opt "build-arg:DO_AGENT_PLATFORM=$DO_AGENT_PLATFORM"
  --opt "build-arg:DO_AGENT_PLATFORM_DIGEST=$DO_AGENT_PLATFORM_DIGEST"
  --opt "label:org.opencontainers.image.revision=$source_commit"
  --opt "label:org.opencontainers.image.version=$release"
)

buildctl build "${build_inputs[@]}" --opt filename=Dockerfile.base.light \
  --output "type=image,name=$repository/base-light:$source_commit,push=true"
base_digest="$(skopeo inspect --format '{{.Digest}}' "docker://$repository/base-light:$source_commit")"
[[ "$base_digest" =~ ^sha256:[a-f0-9]{64}$ ]]
base_image="$repository/base-light@$base_digest"

buildctl build "${build_inputs[@]}" --opt filename=Dockerfile \
  --opt "build-arg:DOOPS_AGENT_BASE_IMAGE=$base_image" \
  --opt "build-arg:DOOPS_RELEASE_TAG=$release" \
  --opt "build-arg:DOOPS_SOURCE_COMMIT=$source_commit" \
  --output "type=image,name=$repository:$source_commit,push=true"
agent_digest="$(skopeo inspect --format '{{.Digest}}' "docker://$repository:$source_commit")"
[[ "$agent_digest" =~ ^sha256:[a-f0-9]{64}$ ]]
skopeo inspect --config "docker://$repository@$agent_digest" |
  python3 -c 'import json,sys; d=json.load(sys.stdin)["config"]["Labels"]; expected={"org.opencontainers.image.revision":sys.argv[1],"org.opencontainers.image.version":sys.argv[2],"org.opencontainers.image.base.name":sys.argv[3],"org.l8ai.doagent.image":sys.argv[4]}; assert all(d.get(k)==v for k,v in expected.items()), "candidate labels differ from build inputs"' \
    "$source_commit" "$release" "$base_image" "$runtime_image"
printf 'source_commit=%s\nrelease=%s\nbase_image=%s\nagent_image=%s@%s\ndoagent_image=%s\n' \
  "$source_commit" "$release" "$base_image" "$repository" "$agent_digest" "$runtime_image"
