# Builds the Linux desktop app and a .deb (see `make package-linux`).
# Output: Skim (binary) and skim_<version>_<arch>.deb.
FROM golang:1.27-trixie AS toolchain
RUN apt-get update && apt-get install -y --no-install-recommends \
      build-essential pkg-config libgtk-3-dev libwebkit2gtk-4.1-dev \
    && rm -rf /var/lib/apt/lists/*
COPY --from=node:24-trixie-slim /usr/local/bin/node /usr/local/bin/node
COPY --from=node:24-trixie-slim /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/wailsapp/wails/v2/cmd/wails@v2.16.0 && \
    go install github.com/goreleaser/nfpm/v2/cmd/nfpm@v2.47.0

FROM toolchain AS build
ARG VERSION
WORKDIR /src
COPY . .
# webkit2_41: Debian 13 / Ubuntu 24.04+ ship WebKitGTK 4.1 only.
RUN --mount=type=cache,target=/root/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build --mount=type=cache,target=/root/.npm \
    cd cmd/skim-desktop && wails build -skipbindings -clean -tags webkit2_41
RUN mkdir /out && cp cmd/skim-desktop/build/bin/Skim /out/Skim && \
    VERSION="$VERSION" ARCH="$(go env GOARCH)" nfpm package -f packaging/linux/nfpm.yaml -p deb -t /out/

FROM scratch
COPY --from=build /out /
