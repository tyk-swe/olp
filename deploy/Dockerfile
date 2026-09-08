# syntax=docker/dockerfile:1
FROM rust:1.98-trixie@sha256:620dbcd124499c59e2406d3741574b5c5838cf9eb9656f0c3a03948f79b02959 AS rust-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
WORKDIR /src
COPY Cargo.toml Cargo.lock rust-toolchain.toml rustfmt.toml ./
COPY src ./src
COPY migrations ./migrations
COPY examples ./examples
COPY benches ./benches
COPY tests ./tests
RUN --mount=type=cache,target=/usr/local/cargo/registry \
    --mount=type=cache,target=/usr/local/cargo/git \
    cargo build --locked --release --bin olp --example export_openapi
RUN mkdir -p openapi && target/release/examples/export_openapi > openapi/management.json

FROM node:26-alpine@sha256:2d984a15c9b54fd0aeb608b8e0d0d83529eb34d2966db27a1fb4f1edc3d298a3 AS console-build
ARG BUILDKIT_SBOM_SCAN_STAGE=true
ENV CI=true
WORKDIR /src
RUN npm install --global pnpm@11.24.0
COPY package.json pnpm-workspace.yaml pnpm-lock.yaml ./
COPY console/package.json ./console/package.json
COPY tests/sdk-smoke/package.json ./tests/sdk-smoke/package.json
RUN --mount=type=cache,target=/pnpm/store \
    pnpm install --frozen-lockfile --store-dir /pnpm/store
COPY console ./console
COPY --from=rust-build /src/openapi ./openapi
RUN pnpm --dir console api:generate && pnpm --dir console build

FROM gcr.io/distroless/cc-debian13:nonroot@sha256:c31ff9abcb1910f3ab25c7957bdaf0bfe12a01eb546e8df2282f1c8f682b606c
ARG OLP_VERSION=3.0.0
LABEL org.opencontainers.image.source="https://github.com/tyk-swe/olp" \
      org.opencontainers.image.title="OpenLLMProxy" \
      org.opencontainers.image.description="Self-hosted AI gateway and control plane" \
      org.opencontainers.image.licenses="AGPL-3.0-only" \
      org.opencontainers.image.version="$OLP_VERSION"
COPY --from=rust-build /src/target/release/olp /usr/local/bin/olp
COPY --from=console-build /src/console/build /opt/olp/console
COPY LICENSE /usr/share/doc/openllmproxy/
ENV OLP_CONSOLE_DIR=/opt/olp/console
EXPOSE 8080 9090
USER 65532:65532
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/olp"]
CMD ["all"]
