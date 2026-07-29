# syntax=docker/dockerfile:1.12@sha256:93bfd3b68c109427185cd78b4779fc82b484b0b7618e36d0f104d4d801e66d25
FROM node:26-alpine@sha256:e88a35be04478413b7c71c455cd9865de9b9360e1f43456be5951032d7ac1a66 AS console-build
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
# Cache mounts alone cannot do this: `cache-to: type=gha` exports layers,
# not mount contents.
FROM rust:1.97-bookworm@sha256:7d0723df719e7f213b69dc7c8c595985c3f4b060cfbee4f7bc0e347a86fe3b6a AS chef
ENV CARGO_INCREMENTAL=0
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
COPY --from=planner /src/recipe.json recipe.json
RUN --mount=type=cache,id=olp-cargo-registry,target=/usr/local/cargo/registry,sharing=locked \
    cargo chef cook --locked --release --recipe-path recipe.json --package olp
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
    cargo build --locked --release -p olp

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:ce0d66bc0f64aae46e6a03add867b07f42cc7b8799c949c2e898057b7f75a151
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
