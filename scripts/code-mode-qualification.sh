#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."

mode=${1:-cli}
case "$mode" in cli|process|all|install) ;; *) echo 'usage: code-mode-qualification.sh [cli|process|all|install]' >&2; exit 2;; esac
version=0.160.0
case "$(uname -sm)" in
  'Linux x86_64')
    platform=linux-x64
    target=x86_64-unknown-linux-musl
    digest='KI/73OqGrHmR18s7ya7E1NqV6rT0y3lxr0s8S1qR2m6zU6QRF/HlR529jALLh5vjdUnsRT4Ahoxt0axb4kY99g==' ;;
  'Linux aarch64')
    platform=linux-arm64
    target=aarch64-unknown-linux-musl
    digest='VnVdsS06YlDsL8OwaJjQ3xdqdJNG4i+eBJBV3LytddknzSaF/urijLzVg3ptkwvoj8C7Gn7iWpVE+y8WEaPiCg==' ;;
  *) echo 'Qualification installer supports Linux amd64/arm64 only.' >&2; exit 2 ;;
esac
cache="$PWD/.local/codecli/$version-$platform"
mkdir -p "$cache"
archive="$cache/codex.tgz"
if [[ ! -f "$archive" ]]; then
  curl --fail --location --proto '=https' --tlsv1.2 \
    "https://registry.npmjs.org/@openai/codex/-/codex-$version-$platform.tgz" -o "$archive.part"
  mv "$archive.part" "$archive"
fi
actual=$(openssl dgst -sha512 -binary "$archive" | openssl base64 -A)
if [[ "$actual" != "$digest" ]]; then
  echo 'Official Codex archive integrity mismatch; refusing to execute.' >&2
  exit 1
fi
tar -xzf "$archive" -C "$cache"
export OLP_CODEX_BINARY="$cache/package/vendor/$target/bin/codex"
[[ "$("$OLP_CODEX_BINARY" --version)" == "codex-cli $version" ]]
if [[ "$mode" == install ]]; then printf '%s\n' "$OLP_CODEX_BINARY"; exit; fi
if [[ "$mode" == cli || "$mode" == all ]]; then
  go test -mod=readonly -race -tags=codecli -count=1 -timeout=5m -v ./tests/codecli
fi
if [[ "$mode" == process || "$mode" == all ]]; then
  : "${OLP_TEST_DATABASE_URL:?Provision integration PostgreSQL first}"
  : "${OLP_TEST_VALKEY_URL:?Provision integration Valkey first}"
  : "${OLP_TEST_BINARY:?Build the integrated OLP binary first}"
  go test -mod=readonly -race -tags=integration,codecli -count=1 -timeout=15m -v \
    -run '^TestCodeQualification' ./tests/integration
fi
