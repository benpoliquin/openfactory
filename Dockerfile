# syntax=docker/dockerfile:1
# Published package metadata: bb-app@0.47.1 (gitHead 5e1acd765039e874643f34c4d35409e58484e408),
# @oh-my-pi/pi-coding-agent@18.10.3 (requires Bun >=1.3.14), bun@1.3.14.
FROM golang:1.26-bookworm AS go-toolchain

FROM go-toolchain AS controller-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/openfactory ./cmd/openfactory

# Both actors can execute repository Go/Node/Bun/Python checks. Add extra tools
# here rather than mounting the host toolchain, home, or Docker socket.
FROM node:24.21.0-bookworm AS tooling
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
       bash build-essential ca-certificates curl git jq openssh-client python3 ripgrep \
    && dpkg --compare-versions "$(git version | cut -d ' ' -f 3)" ge 2.38 \
    && rm -rf /var/lib/apt/lists/*
COPY --from=go-toolchain /usr/local/go /usr/local/go
ENV PATH="/usr/local/go/bin:${PATH}" \
    GOTOOLCHAIN=local \
    GOTELEMETRY=off
RUN npm install --global --ignore-scripts=false --no-audit --no-fund \
      npm@11.19.0 \
    && npm install --global --ignore-scripts=false --no-audit --no-fund \
      bun@1.3.14 \
    && npm cache clean --force
RUN groupadd --gid 10001 openfactory \
    && useradd --uid 10001 --gid 10001 --create-home --home-dir /home/openfactory openfactory \
    && install -d -o 10001 -g 10001 /workspaces /state /home/bb /run/openfactory-checks

FROM tooling AS controller
COPY --from=controller-build /out/openfactory /usr/local/bin/openfactory
RUN install -d -m 0700 -o 10001 -g 10001 /executor-state /state/artifacts /artifacts
ENV HOME=/home/openfactory \
    GOCACHE=/tmp/go-build \
    GOPATH=/tmp/go \
    NPM_CONFIG_CACHE=/tmp/npm
USER 10001:10001
WORKDIR /workspaces
EXPOSE 38887 38888 38889
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/openfactory"]
CMD ["--config", "/config/openfactory.yaml", "run"]

FROM tooling AS bb
# Keep install scripts enabled: better-sqlite3, node-pty and @parcel/watcher
# require native addons. Python/make/C++ above support source builds when no
# architecture-specific prebuild exists. npm 11 does not block these scripts.
RUN npm install --global --ignore-scripts=false --no-audit --no-fund \
      @oh-my-pi/pi-coding-agent@18.10.3 bb-app@0.47.1 \
    && npm cache clean --force
# Path installation builds inside the writable BB home, not the read-only
# image. Ship the locked SDK/source dependencies; BB caches its build toolchain
# separately under BB_DATA_DIR on the first supported plugin build.
COPY plugins/openfactory /opt/openfactory/plugin
RUN npm ci --prefix /opt/openfactory/plugin --include=dev --ignore-scripts=false --no-audit --no-fund \
    && npm cache clean --force
COPY deploy/bb-start.mjs /opt/openfactory/bin/bb-start.mjs
COPY --chmod=755 deploy/bb-entrypoint.sh /usr/local/bin/bb-entrypoint
COPY --chmod=755 deploy/omp.sh /opt/openfactory/bin/omp
COPY deploy/research-extension.ts /usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/openfactory/research-extension.ts
COPY deploy/research-config.yaml /opt/openfactory/discovery/research-config.yaml
COPY deploy/intake-extension.ts /usr/local/lib/node_modules/@oh-my-pi/pi-coding-agent/openfactory/intake-extension.ts
COPY deploy/intake-config.yaml /opt/openfactory/intake/intake-config.yaml
ENV PATH="/opt/openfactory/bin:${PATH}"
ENV HOME=/home/bb \
    BB_DATA_DIR=/home/bb/.bb \
    BB_APP_URL=http://bb:38886 \
    BB_SERVER_URL=http://127.0.0.1:38886 \
    BB_TELEMETRY=false \
    GOCACHE=/tmp/go-build \
    GOPATH=/tmp/go
USER 10001:10001
WORKDIR /workspaces
EXPOSE 38886
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/bb-entrypoint"]
CMD ["node", "/opt/openfactory/bin/bb-start.mjs", "bb-app", "start", "--bundled", "--no-in-app-updates", "--data-dir", "/home/bb/.bb", "--server-bind-host", "0.0.0.0", "--server-port", "38886"]
