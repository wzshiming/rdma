# The builder tag must satisfy the go directive in go.mod; GOTOOLCHAIN=local fails fast instead of downloading one.
# The builder runs on the build platform and cross-compiles through Debian multiarch: the target's gcc, rdma-core
# headers and libraries install beside the native ones, and target-gcc names whichever gcc emits the target's code.
FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETARCH
RUN set -eux; \
    case "$TARGETARCH" in \
        amd64) triplet=x86_64-linux-gnu; cross=gcc-x86-64-linux-gnu ;; \
        arm64) triplet=aarch64-linux-gnu; cross=gcc-aarch64-linux-gnu ;; \
        *) echo "unsupported TARGETARCH $TARGETARCH" >&2; exit 1 ;; \
    esac; \
    if [ "$TARGETARCH" = "$(dpkg --print-architecture)" ]; then \
        packages='libibverbs-dev librdmacm-dev'; \
        ln -s /usr/bin/gcc /usr/local/bin/target-gcc; \
    else \
        dpkg --add-architecture "$TARGETARCH"; \
        packages="$cross libc6-dev:$TARGETARCH libibverbs-dev:$TARGETARCH librdmacm-dev:$TARGETARCH"; \
        ln -s "/usr/bin/$triplet-gcc" /usr/local/bin/target-gcc; \
    fi; \
    apt-get update; \
    apt-get install -y --no-install-recommends $packages; \
    rm -rf /var/lib/apt/lists/*
WORKDIR /src
ENV CGO_ENABLED=1 GOTOOLCHAIN=local
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CC=target-gcc GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/rdma ./cmd/rdma

FROM debian:trixie-slim
# libibverbs1's postinst creates the rdma group, so the user joins it instead of a group of its own.
RUN apt-get update \
    && apt-get install -y --no-install-recommends libibverbs1 librdmacm1t64 ibverbs-providers \
    && rm -rf /var/lib/apt/lists/* \
    && useradd -r -u 10001 -g rdma rdma
COPY --from=build /out/rdma /usr/local/bin/rdma
# Numeric so runAsNonRoot-style checks can verify it; useradd above gives the id its name and group.
USER 10001
ENTRYPOINT ["rdma"]
CMD ["help"]
