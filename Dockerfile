# Stage 1: Build the Go binary. This stage runs on the build host's platform
# and cross-compiles for the target, so arm64 images need no emulated Go build.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-s -w \
      -X github.com/aboldnewlook/github-scaleset-orchestrator/internal/buildinfo.Version=${VERSION} \
      -X github.com/aboldnewlook/github-scaleset-orchestrator/internal/buildinfo.Commit=${COMMIT} \
      -X github.com/aboldnewlook/github-scaleset-orchestrator/internal/buildinfo.Date=${BUILD_DATE}" \
    -o /out/gso .

# Stage 2: Pre-bake the pinned GitHub Actions runner (runner/pin.json) for the
# target platform and verify its SHA-256.
FROM ubuntu:24.04 AS runner-dl
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY runner/pin.json /tmp/pin.json
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      amd64) PLATFORM=linux-x64 ;; \
      arm64) PLATFORM=linux-arm64 ;; \
      *) echo "unsupported TARGETARCH=${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    VERSION=$(jq -r .version /tmp/pin.json); \
    SHA=$(jq -r --arg p "$PLATFORM" '.sha256[$p]' /tmp/pin.json); \
    curl -fsSL -o /tmp/runner.tar.gz \
      "https://github.com/actions/runner/releases/download/v${VERSION}/actions-runner-${PLATFORM}-${VERSION}.tar.gz"; \
    echo "${SHA}  /tmp/runner.tar.gz" | sha256sum -c -; \
    DEST="/opt/runner-cache/gso/runner-${VERSION}"; \
    mkdir -p "$DEST"; \
    tar xzf /tmp/runner.tar.gz -C "$DEST"; \
    rm /tmp/runner.tar.gz; \
    printf '%s' "$SHA" > "$DEST/.sha256"; \
    echo "Runner ${VERSION} (${PLATFORM}) verified and extracted"

# Stage 3: Runtime image
# Ubuntu is used because the GitHub Actions runner spawns job processes
# inside this container, and workflows expect a full Ubuntu environment.
FROM ubuntu:24.04

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    curl \
    jq \
    ca-certificates \
    build-essential \
    libicu74 \
    libssl3t64 \
    && rm -rf /var/lib/apt/lists/*

# Create non-root runner user
RUN groupadd -g 1001 runner && useradd -u 1001 -g runner -m -s /bin/bash -d /home/runner runner

# Set up directories
RUN mkdir -p /etc/gso \
    && mkdir -p /opt/gso/cache \
    && mkdir -p /cache \
    && mkdir -p /home/runner/.cache \
    && mkdir -p /home/runner/.config/gso/tls \
    && chown -R runner:runner /home/runner \
    && chown -R runner:runner /opt/gso \
    && chown -R runner:runner /cache \
    && chown -R runner:runner /etc/gso

# Copy the Go binary from builder
COPY --from=builder /out/gso /usr/local/bin/gso

# Copy pre-baked runner binary into the cache location
# os.UserCacheDir() on Linux returns $XDG_CACHE_HOME or $HOME/.cache
# We set XDG_CACHE_HOME so the manager finds the pre-baked runner
COPY --from=runner-dl --chown=runner:runner /opt/runner-cache/gso /home/runner/.cache/gso

# Docker executor jobs use this immutable image copy; the manager's mutable
# runner cache is never mounted into a job container.
COPY --from=runner-dl --chown=runner:runner /opt/runner-cache/gso /opt/gso-runner
COPY --chmod=755 docker/gso-job /usr/local/bin/gso-job

# Set XDG_CACHE_HOME so os.UserCacheDir() resolves to /home/runner/.cache
ENV XDG_CACHE_HOME=/home/runner/.cache
ENV XDG_CONFIG_HOME=/home/runner/.config
ENV HOME=/home/runner

USER runner
WORKDIR /home/runner

ENTRYPOINT ["gso"]
CMD ["start"]
