#!/usr/bin/env bash
# Run a command inside the pinned builder image.
#
#   BUILDER=golang:...@sha256:... scripts/in-builder.sh go test ./...
#   BUILDER=golang:...@sha256:... VERSION=v1.0.0 scripts/in-builder.sh scripts/build-all.sh
#
# The compiler never runs on the host, so the host's OS, its package updates
# and whatever Go it happens to ship cannot reach the output. Upgrading the
# build environment means changing one digest in the workflow, deliberately.

set -euo pipefail

: "${BUILDER:?set BUILDER to a digest-pinned golang image}"

case "$BUILDER" in
*@sha256:*) ;;
*)
	# A tag can be repointed at new content by whoever owns it; a digest
	# cannot. Refusing here keeps a careless edit from quietly unpinning the
	# whole build.
	echo "in-builder: BUILDER must be pinned by digest, got '$BUILDER'" >&2
	exit 2
	;;
esac

exec docker run --rm \
	--user "$(id -u):$(id -g)" \
	--volume "$PWD:/src" \
	--workdir /src \
	--env HOME=/tmp \
	--env GOCACHE=/tmp/go-build \
	--env GOMODCACHE=/tmp/go-mod \
	--env GOTOOLCHAIN="${GOTOOLCHAIN:-local}" \
	--env CGO_ENABLED="${CGO_ENABLED:-0}" \
	--env VERSION \
	--env OUT \
	"$BUILDER" "$@"
