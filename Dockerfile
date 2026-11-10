# The toolchain tag must satisfy the go directive in go.mod; GOTOOLCHAIN=local fails fast instead of downloading one.
# The bindings load rdma-core at run time, so the build stage is Debian with the Go toolchain alone and cross-compiles
# with CGO_ENABLED=0 on the build platform: the golang image carries a C compiler, and on alpine the linker would take
# the musl loader as the interpreter of binaries for the build platform's own architecture.
FROM --platform=$BUILDPLATFORM golang:1.27 AS toolchain
FROM --platform=$BUILDPLATFORM debian:trixie-slim AS build
COPY --from=toolchain /usr/local/go /usr/local/go
COPY --from=toolchain /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
ENV PATH=/usr/local/go/bin:$PATH GOPATH=/go CGO_ENABLED=0 GOTOOLCHAIN=local
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/rdma ./cmd/rdma

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
