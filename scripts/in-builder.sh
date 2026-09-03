#!/usr/bin/env bash
# Run a command inside the pinned builder image.
#
#   scripts/in-builder.sh go test ./...
#   VERSION=v1.0.0 scripts/in-builder.sh scripts/build-all.sh
#
# The image is defined by .github/builder/Dockerfile: Ubuntu 22.04 pinned by
# digest, plus a Go toolchain pinned by version and checksum. The compiler
# never runs on the host, so the host's distribution, its package updates and
# whatever Go it happens to ship cannot reach the output. That is what lets
# every CI job sit on ubuntu-latest -- the one runner label GitHub does not
# eventually delete -- without the environment moving underneath the build.

set -euo pipefail

readonly IMAGE=115dav-builder
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# Cheap when the layers are already cached, and reproducible when they are
# not: the base is a digest and the toolchain is checksum-verified.
docker build --quiet --tag "$IMAGE" "$root/.github/builder" >/dev/null

exec docker run --rm \
	--user "$(id -u):$(id -g)" \
	--volume "$root:/src" \
	--workdir /src \
	--env HOME=/tmp \
	--env GOCACHE=/tmp/go-build \
	--env GOMODCACHE=/tmp/go-mod \
	--env GOFLAGS=-mod=readonly \
	--env CGO_ENABLED="${CGO_ENABLED:-0}" \
	--env VERSION \
	--env OUT \
	"$IMAGE" "$@"
# --user keeps build products owned by the caller rather than root, which
# leaves HOME unset inside the container; the caches are pointed at /tmp so
# nothing needs to write outside the mount.
