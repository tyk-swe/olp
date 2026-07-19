#!/usr/bin/env bash
set -euo pipefail

workspace_root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$workspace_root"

for command in awk find jq mktemp rg rustc sed sha256sum sort unlink; do
  command -v "$command" >/dev/null || {
    echo "reference connector build check requires $command" >&2
    exit 2
  }
done

fixture_directory=tests/reference-connector-v1
fixture_source=$fixture_directory/main.rs
fixture_readme=$fixture_directory/README.md
connector_contract=docs/enterprise/contracts/connector-v1.json
connector_proto=proto/olp/connector/v1/connector.proto
expected_fixture_entries=$'README.md\nmain.rs'
forbidden_fixture_pattern='ProviderKind|olp[-_](domain|core|protocols|providers|storage)|crates/(domain|protocols|providers|storage)|extern[[:space:]]+(crate|"C"|"system")|include(_bytes|_str)?!|#\[(path|link)|CARGO_MANIFEST|std::(env|fs|net|os|path|process)|(^|[^[:alnum:]_])(env|option_env|global_asm|asm)!'

inventory_is_exact() {
  [[ $1 == "$expected_fixture_entries" ]]
}

# Keep the rejection rules executable without changing the reviewed fixture.
inventory_is_exact "$expected_fixture_entries" || {
  echo "reference connector inventory self-test rejected the allowed corpus" >&2
  exit 1
}
for rejected_inventory in \
  $'README.md\nmain.rs\nnested' \
  $'README.md\nmain.rs\nnested/escape.rs' \
  $'README.md\nmain.rs\nsource-link'; do
  if inventory_is_exact "$rejected_inventory"; then
    echo "reference connector inventory self-test accepted an extra entry" >&2
    exit 1
  fi
done
for rejected_source in \
  'use olp_domain::ProviderKind;' \
  '#[path = "../../crates/domain/src/ports.rs"] mod ports;' \
  'const BYTES: &[u8] = include_bytes!("outside.bin");' \
  'use std::process::Command;'; do
  rg --text --quiet -- "$forbidden_fixture_pattern" <<<"$rejected_source" || {
    echo "reference connector content self-test missed a forbidden escape hatch" >&2
    exit 1
  }
done
if rg --text --quiet -- "$forbidden_fixture_pattern" \
  <<<'use std::fmt::{self, Display};'; then
  echo "reference connector content self-test rejected standard-library-only source" >&2
  exit 1
fi

[[ -d $fixture_directory && ! -L $fixture_directory ]] || {
  echo "reference connector fixture directory is missing or is a symlink" >&2
  exit 1
}
for fixture_entry in "$fixture_readme" "$fixture_source"; do
  [[ -f $fixture_entry && ! -L $fixture_entry ]] || {
    echo "reference connector fixture entry is missing, not regular, or is a symlink: $fixture_entry" >&2
    exit 1
  }
done

fixture_entries=$(find "$fixture_directory" -mindepth 1 -printf '%P\n' | LC_ALL=C sort)
inventory_is_exact "$fixture_entries" || {
  echo "reference connector fixture must contain exactly regular README.md and main.rs entries" >&2
  exit 1
}

if rg --text -n -- "$forbidden_fixture_pattern" "$fixture_readme" "$fixture_source"; then
  echo "reference connector fixture contains a dependency or filesystem/process escape hatch" >&2
  exit 1
fi

declared_fixture_source=$(jq -er '.reference_connector_gate.m0_contract_build_proof.source' "$connector_contract")
declared_fixture_source_sha256=$(jq -er '.reference_connector_gate.m0_contract_build_proof.source_sha256' "$connector_contract")
actual_fixture_source_sha256=$(sha256sum "$fixture_source" | sed -E 's/[[:space:]].*$//')
[[ $declared_fixture_source == "$fixture_source" \
  && $declared_fixture_source_sha256 =~ ^[0-9a-f]{64}$ \
  && $declared_fixture_source_sha256 == "$actual_fixture_source_sha256" ]] || {
  echo "reference connector fixture source path or digest is stale" >&2
  exit 1
}

fixture_binary=$(mktemp /tmp/olp-reference-connector-v1.XXXXXX)
[[ $fixture_binary == /tmp/olp-reference-connector-v1.* ]] || {
  echo "mktemp returned an unexpected reference connector path" >&2
  exit 1
}
cleanup() {
  unlink -- "$fixture_binary" 2>/dev/null || true
}
trap cleanup EXIT

rustc --edition=2021 -F warnings -C debuginfo=0 "$fixture_source" -o "$fixture_binary"
fixture_output=$("$fixture_binary")

declared_proto_sha256=$(jq -er '.wire_protocol.protobuf_idl.sha256' "$connector_contract")
actual_proto_sha256=$(sha256sum "$connector_proto" | sed -E 's/[[:space:]].*$//')
fixture_proto_sha256=$(sed -nE 's/^proto_sha256=([0-9a-f]{64})$/\1/p' <<<"$fixture_output")
[[ $fixture_proto_sha256 == "$declared_proto_sha256" && $fixture_proto_sha256 == "$actual_proto_sha256" ]] || {
  echo "reference connector fixture does not pin the current Connector v1 protobuf" >&2
  exit 1
}

declared_rpc_methods=$(jq -r '.wire_protocol.protobuf_idl.methods[]' "$connector_contract")
proto_rpc_methods=$(sed -nE 's/^[[:space:]]*rpc[[:space:]]+([A-Za-z][A-Za-z0-9]*)\(.*/\1/p' "$connector_proto")
fixture_rpc_methods=$(sed -nE 's/^rpc=([A-Za-z][A-Za-z0-9]*)$/\1/p' <<<"$fixture_output")
[[ $fixture_rpc_methods == "$declared_rpc_methods" && $fixture_rpc_methods == "$proto_rpc_methods" ]] || {
  echo "reference connector fixture does not cover every frozen Connector v1 RPC" >&2
  diff -u <(printf '%s\n' "$proto_rpc_methods") <(printf '%s\n' "$fixture_rpc_methods") >&2 || true
  exit 1
}

fixture_type_id=$(sed -nE 's/^type_id=(.+)$/\1/p' <<<"$fixture_output")
jq --exit-status --arg type_id "$fixture_type_id" '
  .provider_type_id.pattern as $pattern
  | ($type_id | test($pattern))
  and $type_id == "com.example.external-fixture"
' "$connector_contract" >/dev/null || {
  echo "reference connector fixture does not bind the reviewed open string type_id" >&2
  exit 1
}

echo "reference connector contract/build proof passed: type_id=$fixture_type_id rpcs=8"
