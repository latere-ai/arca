#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Latere AI
# SPDX-License-Identifier: MIT
#
# Brings the kind stack of spec 016 up: the cluster, the images, the
# overlay, the migration, and the wait until arcad is ready.
#
# Usage:
#   deploy/examples/kind/up.sh [arcad.tar [arca-stubs.tar]]
#
# With no argument the images are pulled from the registry under the tag
# ARCA_IMAGE_TAG names. With one or two image archives, as `docker save`
# writes them, those bytes are loaded instead, which is how CI runs the
# stack on the image it just built or just published rather than on a
# second build of it.
#
# Environment:
#   CLUSTER          the kind cluster name, default arca
#   ARCA_IMAGE_TAG   the tag the overlay pins, default candidate
#   ARCAD_IMAGE      the arcad reference to pull when no archive is given
#   STUBS_IMAGE      the arca-stubs reference to pull when no archive is given

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
CLUSTER="${CLUSTER:-arca}"
TAG="${ARCA_IMAGE_TAG:-candidate}"
ARCAD_IMAGE="${ARCAD_IMAGE:-ghcr.io/latere-ai/arcad:${TAG}}"
STUBS_IMAGE="${STUBS_IMAGE:-ghcr.io/latere-ai/arca-stubs:${TAG}}"
NAMESPACE=arca

for cmd in kind kubectl docker; do
  command -v "$cmd" >/dev/null || { echo "$cmd is required" >&2; exit 1; }
done

if ! kind get clusters | grep -qx "$CLUSTER"; then
  kind create cluster --name "$CLUSTER" --config "$here/kind.yaml"
fi

# An archive is loaded under the tag the overlay pins, so the stack runs
# the bytes that were handed to it.
load() {
  local archive="$1" want="$2"
  local loaded
  loaded=$(docker load -i "$archive" | sed -n 's/^Loaded image: //p' | head -1)
  [ -n "$loaded" ] || { echo "nothing loaded from $archive" >&2; exit 1; }
  [ "$loaded" = "$want" ] || docker tag "$loaded" "$want"
  kind load docker-image --name "$CLUSTER" "$want"
}

if [ $# -ge 1 ]; then
  load "$1" "ghcr.io/latere-ai/arcad:${TAG}"
else
  docker pull "$ARCAD_IMAGE"
  docker tag "$ARCAD_IMAGE" "ghcr.io/latere-ai/arcad:${TAG}"
  kind load docker-image --name "$CLUSTER" "ghcr.io/latere-ai/arcad:${TAG}"
fi

if [ $# -ge 2 ]; then
  load "$2" "ghcr.io/latere-ai/arca-stubs:${TAG}"
else
  docker pull "$STUBS_IMAGE"
  docker tag "$STUBS_IMAGE" "ghcr.io/latere-ai/arca-stubs:${TAG}"
  kind load docker-image --name "$CLUSTER" "ghcr.io/latere-ai/arca-stubs:${TAG}"
fi

kubectl apply -k "$here"

# The stores first: the migration cannot run before Postgres answers, and
# arcad cannot report ready before the bucket exists.
kubectl -n "$NAMESPACE" rollout status deployment/postgres --timeout=300s
kubectl -n "$NAMESPACE" rollout status deployment/minio --timeout=300s
kubectl -n "$NAMESPACE" wait --for=condition=complete job/minio-init --timeout=300s
kubectl -n "$NAMESPACE" rollout status deployment/arca-stubs --timeout=300s

# The schema, applied by a Job and never by an init container, so two
# replicas rolling at once never migrate twice (spec 016).
kubectl -n "$NAMESPACE" delete job arcad-migrate --ignore-not-found
sed "s|ghcr.io/latere-ai/arcad:unreleased|ghcr.io/latere-ai/arcad:${TAG}|" \
  "$here/../../bootstrap/migrate-job.yaml" | kubectl -n "$NAMESPACE" apply -f -
kubectl -n "$NAMESPACE" wait --for=condition=complete job/arcad-migrate --timeout=300s

kubectl -n "$NAMESPACE" rollout status deployment/arcad --timeout=300s

echo
echo "the stack is up:"
echo "  arcad            http://localhost:30180"
echo "  the stub issuer  http://localhost:30081"
echo "  the authorizer   http://localhost:30082"
echo "  Postgres         postgres://arca:arca@localhost:30432/arca?sslmode=disable"
echo "  MinIO            http://localhost:30900 (minioadmin/minioadmin, bucket arca-test)"
