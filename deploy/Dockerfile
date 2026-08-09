# syntax=docker/dockerfile:1.12@sha256:93bfd3b68c109427185cd78b4779fc82b484b0b7618e36d0f104d4d801e66d25
FROM node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43 AS console-build
ENV CI=true
RUN corepack enable
WORKDIR /src/console
COPY console/package.json console/pnpm-lock.yaml console/pnpm-workspace.yaml ./
RUN --mount=type=cache,id=olp-pnpm,target=/pnpm/store,sharing=locked \
    pnpm config set store-dir /pnpm/store && \
    pnpm install --frozen-lockfile
COPY console/ ./
RUN pnpm build

# cargo-chef splits the release build into a dependency layer keyed only on
# the manifests (recipe.json) and a source layer, so BuildKit's gha cache
# reuses the expensive dependency compilation across source-only changes.
# The sccache mount below helps persistent local builders, but cache-to:
# type=gha exports layers rather than cache-mount contents, so cargo-chef
# remains the remote image-build cache.
FROM rust:1.97.1-bookworm@sha256:77fac8b98f9f46062bb680b6d25d5bcaabfc400143952ebc572e924bcbedc3fa AS chef
ARG SCCACHE_VERSION=0.17.0
ARG TARGETARCH
ENV CARGO_INCREMENTAL=0 \
    SCCACHE_DIR=/var/cache/sccache
RUN apt-get update && \
    apt-get install --yes --no-install-recommends \
        clang=1:14.0-55.7~deb12u1 \
        lld=1:14.0-55.7~deb12u1 && \
    rm -rf /var/lib/apt/lists/*
RUN case "$TARGETARCH" in \
        amd64) \
            sccache_target="x86_64-unknown-linux-musl"; \
            sccache_sha256="67c4a96dd237c1f518f6b36083f270f9976d516f1e57fce891755ea782e50006" \
            ;; \
        arm64) \
            sccache_target="aarch64-unknown-linux-musl"; \
            sccache_sha256="821a86343191aa1cbab74bd42f9e93c9a63bf85e4742945f40d3ae84193c1c77" \
            ;; \
        *) \
            echo "unsupported Docker architecture for sccache: $TARGETARCH" >&2; \
            exit 1 \
            ;; \
    esac && \
    archive="/tmp/sccache.tar.gz" && \
    checksum_file="/tmp/sccache.sha256" && \
    curl --fail --location --proto '=https' --tlsv1.2 \
        --output "$archive" \
        "https://github.com/mozilla/sccache/releases/download/v${SCCACHE_VERSION}/sccache-v${SCCACHE_VERSION}-${sccache_target}.tar.gz" && \
    printf '%s  %s\n' "$sccache_sha256" "$archive" > "$checksum_file" && \
    sha256sum --check --strict "$checksum_file" && \
    tar --extract --gzip --file "$archive" --directory /usr/local/bin \
        --strip-components=1 \
        "sccache-v${SCCACHE_VERSION}-${sccache_target}/sccache" && \
    rm "$archive" "$checksum_file" && \
    sccache --version
RUN --mount=type=cache,id=olp-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    cargo install cargo-chef --locked --version 0.1.77
WORKDIR /src

FROM chef AS planner
COPY Cargo.toml Cargo.lock rust-toolchain.toml rustfmt.toml ./
COPY .sqlx ./.sqlx
COPY apps ./apps
COPY crates ./crates
COPY tests/conformance ./tests/conformance
COPY tests/e2e ./tests/e2e
RUN cargo chef prepare --recipe-path recipe.json

FROM chef AS rust-build
ENV SQLX_OFFLINE=true
COPY .cargo ./.cargo
COPY --from=planner /src/recipe.json recipe.json
RUN --mount=type=cache,id=olp-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=olp-sccache,target=/var/cache/sccache,sharing=locked \
    sccache --start-server && \
    cargo chef cook --locked --release --recipe-path recipe.json --package olp && \
    sccache --show-stats && \
    sccache --stop-server
COPY Cargo.toml Cargo.lock rust-toolchain.toml rustfmt.toml ./
COPY .sqlx ./.sqlx
COPY apps ./apps
COPY crates ./crates
# Cargo resolves every workspace member even when only `olp` is selected.
# Copy the conformance crate manifest/source required for that resolution, but
# keep SDK, HA, and generated test artifacts out of the production build.
COPY tests/conformance ./tests/conformance
COPY tests/e2e ./tests/e2e
RUN --mount=type=cache,id=olp-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    --mount=type=cache,id=olp-sccache,target=/var/cache/sccache,sharing=locked \
    sccache --start-server && \
    cargo build --locked --release -p olp && \
    sccache --show-stats && \
    sccache --stop-server

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:fccdbb0a547c14e23fcf4ce8ad62ca5d43b4faae8d22cd292f490fef9946c96e
ARG OLP_VERSION=2.0.0
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
USER nonroot:nonroot
STOPSIGNAL SIGTERM
ENTRYPOINT ["/usr/local/bin/olp"]
CMD ["all"]
