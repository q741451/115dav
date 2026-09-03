#!/usr/bin/env bash
# Cross-compile every release target, deterministically.
#
# The output must depend only on the source, the module graph and the Go
# toolchain -- never on the machine doing the build. That is what lets the
# release workflow prove, rather than assume, that a runner image update
# cannot change the binaries it ships.
#
#   VERSION=v1.2.3 OUT=dist scripts/build-all.sh
#
# Run it inside the pinned builder image for a reproducible result; running it
# on a host toolchain is fine for local use but the hashes will only match if
# that toolchain is the same version.

set -euo pipefail

VERSION="${VERSION:-dev}"
OUT="${OUT:-dist}"
BIN=115dav

# Anything the compiler could pick up from the environment is set explicitly.
export CGO_ENABLED=0 # no system libc, headers or linker in the output
export GOFLAGS=-mod=readonly

# Never let go.mod pull down a different compiler behind our back: the whole
# point of pinning the builder image is that the toolchain is decided there.
# Overridable, because a developer whose local Go predates the go directive
# will want the automatic download that CI must not have.
export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

# Fixed timestamp for everything we archive, so the archives hash the same way
# on every run. Not the epoch: zip cannot store dates before 1980.
readonly ARCHIVE_MTIME='2000-01-01 00:00:00 UTC'

# GOOS/GOARCH, plus any extra variables that target needs.
readonly TARGETS=(
	"linux/amd64"
	"linux/arm64"
	"linux/arm      GOARM=7"
	"linux/mipsle   GOMIPS=softfloat" # routers without an FPU
	"linux/mips64le GOMIPS64=softfloat"
	"darwin/amd64"
	"darwin/arm64"
	"windows/amd64"
	"windows/arm64"
	"freebsd/amd64"
)

rm -rf "$OUT"
mkdir -p "$OUT/bin"

for target in "${TARGETS[@]}"; do
	read -r platform extra <<<"$target"
	goos="${platform%%/*}"
	goarch="${platform##*/}"

	name="$BIN"
	[ "$goos" = windows ] && name="$BIN.exe"

	stage="$OUT/bin/${goos}_${goarch}"
	mkdir -p "$stage"

	echo "==> $goos/$goarch ${extra:+($extra)}"
	# shellcheck disable=SC2086 # extra is a deliberate word split
	env GOOS="$goos" GOARCH="$goarch" $extra go build \
		-trimpath \
		-buildvcs=false \
		-ldflags "-s -w -buildid= -X main.version=$VERSION" \
		-o "$stage/$name" \
		.
	# -trimpath      strips the build directory out of the binary
	# -buildvcs=false keeps the commit and a dirty flag from leaking in; the
	#                 version is stamped explicitly above instead
	# -buildid=      drops the one remaining build-machine-derived field
	# -s -w          no symbol table, no DWARF: nothing here is debugged

	# Archive with the docs, so a downloaded tarball is self-contained.
	archive="${BIN}_${VERSION}_${goos}_${goarch}"
	pack="$OUT/.pack/$archive"
	mkdir -p "$pack"
	cp "$stage/$name" README.md "$pack/"
	find "$pack" -exec touch -d "$ARCHIVE_MTIME" {} +

	if [ "$goos" = windows ]; then
		(cd "$OUT/.pack" && TZ=UTC zip -qXr9 "../$archive.zip" "$archive")
	else
		# --sort and the fixed ownership make the member order and metadata
		# stable; gzip -n keeps its own timestamp out of the header.
		tar --sort=name --owner=0 --group=0 --numeric-owner \
			--mtime="$ARCHIVE_MTIME" \
			-C "$OUT/.pack" -cf - "$archive" | gzip -n9 >"$OUT/$archive.tar.gz"
	fi
done

rm -rf "$OUT/.pack"

# Two checksum files with different jobs to do:
#   SHA256SUMS      ships with the release, for people verifying a download
#   SHA256SUMS.bin  covers the raw binaries, and is what the workflow compares
#                   across runner images to detect a build that is not hermetic
(cd "$OUT" && sha256sum ./*.tar.gz ./*.zip | sed 's| \./| |' >SHA256SUMS)
(cd "$OUT" && find bin -type f | sort | xargs sha256sum >SHA256SUMS.bin)

echo
echo "built $BIN $VERSION into $OUT/"
cat "$OUT/SHA256SUMS"
