# syntax=docker/dockerfile:1
FROM golang:1.27.1-trixie@sha256:9baa6b4187bbb98d240372a8a235ac0bb6b5ddd52bba1431dc2f7c0705862728 AS go-build
ARG TARGETARCH
ARG OLP_VERSION
ARG BUILDKIT_SBOM_SCAN_STAGE=true
WORKDIR /src
ENV CGO_ENABLED=1 GOTOOLCHAIN=local
SHELL ["/bin/bash", "-euo", "pipefail", "-c"]
# GLIDE links the archive shipped in its pinned Go module; no Rust tools exist.
RUN test "$(go env GOARCH)" = "$TARGETARCH" && ! command -v cargo && ! command -v rustc
COPY go.mod go.sum package.json ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY openapi ./openapi
COPY deploy/native /out/native
RUN version=$(sed -n 's/  "version": "\([^" ]*\)",/\1/p' package.json) && \
    test -n "$version" && test "${OLP_VERSION:-$version}" = "$version" && \
    mkdir -p /out && go build -mod=readonly -trimpath \
      -ldflags="-s -w -X github.com/tyk-swe/olp/internal/process.Version=$version" -o /out/olp ./cmd/olp
RUN go version -m /out/olp > /out/go-build-info.txt && ldd /out/olp > /out/native-link.txt
RUN glide_dir="$(go list -m -f '{{.Dir}}' github.com/valkey-io/valkey-glide/go/v2)" && \
    cp "$glide_dir/LICENSE" /out/GLIDE-LICENSE && \
    cp "$glide_dir/THIRD_PARTY_LICENSES_GO" /out/GLIDE-THIRD-PARTY-LICENSES

FROM node:26.8.2-alpine@sha256:ef24c5053d50fdc3e4e56eb4e7ddb7861874ab0fdc797046ba897581deb8e868 AS console-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ENV CI=true
WORKDIR /src
RUN npm install --global pnpm@11.24.0
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
COPY console/package.json ./console/package.json
COPY tests/sdk-smoke/package.json ./tests/sdk-smoke/package.json
RUN --mount=type=cache,target=/pnpm/store pnpm install --frozen-lockfile --store-dir /pnpm/store
COPY console ./console
COPY openapi/management.json ./openapi/management.json
RUN pnpm --dir console api:generate && pnpm --dir console build

FROM gcr.io/distroless/cc-debian13:nonroot@sha256:54df941ed0d06a1bd95ef5e0ce391fd8d9f94b64782dc9a60062727849ee3f97
ARG OLP_VERSION=3.0.0
LABEL org.opencontainers.image.source="https://github.com/tyk-swe/olp" \
      org.opencontainers.image.title="OpenLLMProxy" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="$OLP_VERSION" \
      io.openllmproxy.storage="olp-go-v1" \
      io.openllmproxy.fresh-install="required-for-rust-replacement"
COPY --from=go-build /out/olp /usr/local/bin/olp
COPY --from=go-build /out/*LICENSE* /out/*txt /usr/share/doc/openllmproxy/
COPY --from=go-build /out/native/ /usr/share/doc/openllmproxy/native/
COPY --from=console-build /src/console/build /opt/olp/console
COPY LICENSE /usr/share/doc/openllmproxy/
ENV OLP_CONSOLE_DIR=/opt/olp/console OLP_LISTEN_ADDR=0.0.0.0:8080 OLP_OBSERVABILITY_LISTEN_ADDR=0.0.0.0:9090
EXPOSE 8080 9090
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/olp"]
CMD ["all"]
