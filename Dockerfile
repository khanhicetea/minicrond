# syntax=docker/dockerfile:1.7
# minicrond as a container process manager. See docs/operations.md#containers.
ARG GO_VERSION=1.27.2

FROM golang:${GO_VERSION}-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=unknown
RUN --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/root/go/pkg/mod \
    CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" \
      -o /out/minicrond ./cmd/minicrond

# Jobs usually need a shell and common tools; replace the base with your
# application image (and copy the binary in) to supervise your own programs.
FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates tzdata \
 && rm -rf /var/lib/apt/lists/* \
 && install -d -m 0755 /etc/minicron \
 && install -d -m 0700 /var/lib/minicron
COPY --from=build /out/minicrond /usr/local/bin/minicrond
COPY examples/docker/minicron.toml /etc/minicron/minicron.toml
ENV MINICRON_CONFIG=/etc/minicron/minicron.toml \
    MINICRON_DATA=/var/lib/minicron
VOLUME ["/var/lib/minicron"]
# minicrond detects that it is PID 1 and runs a built-in init that forwards
# signals and reaps orphaned processes; no tini/--init is needed (set
# MINICROND_INIT=0 when you use one anyway).
STOPSIGNAL SIGTERM
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
  CMD ["/usr/local/bin/minicrond", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/minicrond"]
CMD ["daemon"]
