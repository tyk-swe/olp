#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

usage() { echo 'usage: code-mode-qualification.sh [cli|process|all] | install [codex|claude-code|opencode]' >&2; exit 2; }
mode=${1:-cli}
case "$mode" in cli|process|all) (( $# <= 1 )) || usage ;; install) (( $# <= 2 )) || usage ;; *) usage ;; esac
case "$(uname -sm)" in
  'Linux x86_64') arch=x64 ;;
  'Linux aarch64') arch=arm64 ;;
  *) echo 'Qualification installer supports Linux amd64/arm64 only.' >&2; exit 2 ;;
esac

# install_package downloads one official npm package archive into the ignored
# cache, verifies its pinned SHA-512 before extracting anything, and leaves it
# extracted under $cache/package.
install_package() {
  local name=$1 url=$2 digest=$3
  cache="$PWD/.local/codecli/$name"
  mkdir -p "$cache"
  local archive="$cache/package.tgz"
  if [[ ! -f "$archive" ]]; then
    curl --fail --location --proto '=https' --tlsv1.2 "$url" -o "$archive.part"
    mv "$archive.part" "$archive"
  fi
  local actual
  actual=$(openssl dgst -sha512 -binary "$archive" | openssl base64 -A)
  if [[ "$actual" != "$digest" ]]; then
    echo "Official $name archive integrity mismatch; refusing to execute." >&2
    exit 1
  fi
  tar -xzf "$archive" -C "$cache"
}

install_codex() {
  local version=0.160.0 target digest
  case $arch in
    x64) target=x86_64-unknown-linux-musl
      digest='KI/73OqGrHmR18s7ya7E1NqV6rT0y3lxr0s8S1qR2m6zU6QRF/HlR529jALLh5vjdUnsRT4Ahoxt0axb4kY99g==' ;;
    arm64) target=aarch64-unknown-linux-musl
      digest='VnVdsS06YlDsL8OwaJjQ3xdqdJNG4i+eBJBV3LytddknzSaF/urijLzVg3ptkwvoj8C7Gn7iWpVE+y8WEaPiCg==' ;;
  esac
  install_package "$version-linux-$arch" "https://registry.npmjs.org/@openai/codex/-/codex-$version-linux-$arch.tgz" "$digest"
  OLP_CODEX_BINARY="$cache/package/vendor/$target/bin/codex"
  [[ "$("$OLP_CODEX_BINARY" --version)" == "codex-cli $version" ]]
}

install_claude_code() {
  local version=2.1.286 digest
  case $arch in
    x64) digest='PnVL8ZCEev3IexLeOc9/QRfqRDUZRgnsaMLdpjlBN0VaieeTrf7mZ2JJi29ue2KAqHvq6SsOadU5G/shnFE4MA==' ;;
    arm64) digest='dHQ/ObyC4jaGHVk+gFE8P8PALMIVLhQSyhiC3y3kkuLp0H0MHgLxHt/srQ3zJs892ClDbtl1swxv4bw1ioruZA==' ;;
  esac
  install_package "claude-code-$version-linux-$arch" "https://registry.npmjs.org/@anthropic-ai/claude-code-linux-$arch/-/claude-code-linux-$arch-$version.tgz" "$digest"
  OLP_CLAUDE_CODE_BINARY="$cache/package/claude"
  [[ "$(HOME="$cache" "$OLP_CLAUDE_CODE_BINARY" --version)" == "$version (Claude Code)" ]]
}

install_opencode() {
  local version=1.18.34 digest
  case $arch in
    x64) digest='RTAMjCve4euxP2QKLuvRmdoW5J5DQK1DiZqt+7slfixyjAEi79QC2Df2oYKogibaAI4IEU8uzenoJeEl3k+UEw==' ;;
    arm64) digest='ZSjqcH0MEbAzLEmkRC9Aop1PbWZxe2NuKONAE/x8MRQdjKdOepX7M50UpjbsNoiLMkP584Z98Mg4a42fJhouvA==' ;;
  esac
  install_package "opencode-$version-linux-$arch" "https://registry.npmjs.org/opencode-linux-$arch/-/opencode-linux-$arch-$version.tgz" "$digest"
  OLP_OPENCODE_BINARY="$cache/package/bin/opencode"
  [[ "$(HOME="$cache" "$OLP_OPENCODE_BINARY" --version)" == "$version" ]]
}

if [[ "$mode" == install ]]; then
  case "${2:-codex}" in
    codex) install_codex; printf '%s\n' "$OLP_CODEX_BINARY" ;;
    claude-code) install_claude_code; printf '%s\n' "$OLP_CLAUDE_CODE_BINARY" ;;
    opencode) install_opencode; printf '%s\n' "$OLP_OPENCODE_BINARY" ;;
    *) usage ;;
  esac
  exit
fi
install_codex
install_claude_code
install_opencode
export OLP_CODEX_BINARY OLP_CLAUDE_CODE_BINARY OLP_OPENCODE_BINARY
if [[ "$mode" == cli || "$mode" == all ]]; then
  go test -mod=readonly -race -tags=codecli -count=1 -timeout=10m -v ./tests/codecli
fi
if [[ "$mode" == process || "$mode" == all ]]; then
  : "${OLP_TEST_DATABASE_URL:?Provision integration PostgreSQL first}"
  : "${OLP_TEST_VALKEY_URL:?Provision integration Valkey first}"
  : "${OLP_TEST_BINARY:?Build the integrated OLP binary first}"
  go test -mod=readonly -race -tags=integration,codecli -count=1 -timeout=20m -v \
    -run '^TestCodeQualification' ./tests/integration
fi
