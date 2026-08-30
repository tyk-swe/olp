# syntax=docker/dockerfile:1.12@sha256:93bfd3b68c109427185cd78b4779fc82b484b0b7618e36d0f104d4d801e66d25
FROM node:24-alpine@sha256:a0b9bf06e4e6193cf7a0f58816cc935ff8c2a908f81e6f1a95432d679c54fbfd AS console-build
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
FROM rust:1.97.1-bookworm@sha256:77fac8b98f9f46062bb680b6d25d5bcaabfc400143952ebc572e924bcbedc3fa AS chef
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

FROM gcr.io/distroless/cc-debian12:nonroot@sha256:fccdbb0a547c14e23fcf4ce8ad62ca5d43b4faae8d22cd292f490fef9946c96e
ARG OLP_VERSION=2.2.0
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
