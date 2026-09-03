# The build environment, defined here and nowhere else.
#
# Two things decide what a release binary looks like: the operating system the
# compiler runs on, and the compiler itself. Both are pinned in this file --
# the base by digest, the toolchain by version and checksum -- so the image is
# the same set of bytes today and in five years, whatever GitHub does to its
# runners in the meantime.

# Ubuntu 22.04 LTS. A tag can be repointed at new content by its publisher; a
# digest is the content. To move to a newer base, resolve its digest with:
#   docker buildx imagetools inspect ubuntu:24.04 | head -2
FROM ubuntu:22.04@sha256:2edbbc5dc405e9612ba3584ce95480277e3eb374407b5505fe26f17df77c7dbc

# The Go toolchain, fetched and verified rather than inherited from somebody
# else's image. To change it, edit these three lines together; the checksums
# are published at https://go.dev/dl/
#
# Held at 1.22 deliberately, for binary size: each release since has grown
# the output, and 1.22 is the oldest the source still compiles on (it uses
# range-over-int). Measured on linux/amd64, 1.22 is about 1.5 MB smaller
# than 1.27. The cost is that this is past end of life, so the crypto/tls
# and net/http in the release binaries no longer receive fixes -- worth
# knowing before exposing the port beyond a trusted network.
ARG GO_VERSION=1.22.12
ARG GO_SHA256_amd64=4fa4f869b0f7fc6bb1eb2660e74657fbf04cdd290b5aef905585c86051b34d43
ARG GO_SHA256_arm64=fd017e647ec28525e86ae8203236e0653242722a7436929b1f775744e26278e7

RUN set -eux; \
	# ca-certificates and curl fetch the toolchain and the modules; zip packs
	# the Windows archives; gcc and libc6-dev exist only so `go test -race`
	# can run.
	# None of them reach a release binary, which is built with CGO_ENABLED=0
	# and therefore links nothing from this system -- so the exact package
	# versions apt happens to serve cannot change the output.
	apt-get update; \
	apt-get install -y --no-install-recommends ca-certificates curl zip gcc libc6-dev; \
	rm -rf /var/lib/apt/lists/*; \
	\
	arch="$(dpkg --print-architecture)"; \
	case "$arch" in \
	amd64) sha="$GO_SHA256_amd64" ;; \
	arm64) sha="$GO_SHA256_arm64" ;; \
	*) echo "no pinned Go checksum for architecture $arch" >&2; exit 1 ;; \
	esac; \
	\
	curl -fsSL -o /tmp/go.tgz "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz"; \
	echo "${sha}  /tmp/go.tgz" | sha256sum -c -; \
	tar -C /usr/local -xzf /tmp/go.tgz; \
	rm /tmp/go.tgz; \
	\
	/usr/local/go/bin/go version

ENV PATH=/usr/local/go/bin:$PATH

# Belt and braces: even inside the pinned image, never let a go directive in
# go.mod pull down a different compiler than the one verified above.
ENV GOTOOLCHAIN=local
