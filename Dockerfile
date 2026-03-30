# Stage 1: Build the Go binary
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/gso .

# Stage 2: Pre-bake the GitHub Actions runner binary
FROM ubuntu:24.04 AS runner-dl

RUN apt-get update && apt-get install -y --no-install-recommends \
    curl jq ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Download and extract the latest Linux x64 runner binary
# Use the tags API (small JSON, no control character issues) to get the latest version
RUN set -eux; \
    VERSION=$(curl -fsSL https://api.github.com/repos/actions/runner/releases/latest | grep -m1 '"tag_name"' | sed 's/.*"v\([^"]*\)".*/\1/'); \
    URL="https://github.com/actions/runner/releases/download/v${VERSION}/actions-runner-linux-x64-${VERSION}.tar.gz"; \
    mkdir -p "/opt/runner-cache/gso/runner-${VERSION}"; \
    curl -fsSL "$URL" | tar xz -C "/opt/runner-cache/gso/runner-${VERSION}"; \
    echo "Runner ${VERSION} extracted successfully"

# Stage 3: Runtime image
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    git \
    nodejs \
    ca-certificates \
    libicu72 \
    libssl3 \
    && rm -rf /var/lib/apt/lists/*

# Create non-root runner user
RUN useradd -m -s /bin/bash -d /home/runner runner

# Set up directories
RUN mkdir -p /etc/gso \
    && mkdir -p /opt/gso/cache \
    && mkdir -p /home/runner/.cache \
    && chown -R runner:runner /home/runner \
    && chown -R runner:runner /opt/gso \
    && chown -R runner:runner /etc/gso

# Copy the Go binary from builder
COPY --from=builder /out/gso /usr/local/bin/gso

# Copy pre-baked runner binary into the cache location
# os.UserCacheDir() on Linux returns $XDG_CACHE_HOME or $HOME/.cache
# We set XDG_CACHE_HOME so the manager finds the pre-baked runner
COPY --from=runner-dl --chown=runner:runner /opt/runner-cache/gso /home/runner/.cache/gso

# Set XDG_CACHE_HOME so os.UserCacheDir() resolves to /home/runner/.cache
ENV XDG_CACHE_HOME=/home/runner/.cache
ENV HOME=/home/runner

USER runner
WORKDIR /home/runner

COPY --chmod=755 <<'EOF' /usr/local/bin/docker-entrypoint.sh
#!/bin/sh
if [ -n "$GSO_CONFIG_B64" ]; then
  echo "$GSO_CONFIG_B64" | base64 -d > /etc/gso/config.yaml
elif [ -n "$GSO_CONFIG" ]; then
  printf '%s\n' "$GSO_CONFIG" > /etc/gso/config.yaml
fi
exec gso "$@"
EOF

ENTRYPOINT ["docker-entrypoint.sh"]
CMD ["start", "--config", "/etc/gso/config.yaml"]
