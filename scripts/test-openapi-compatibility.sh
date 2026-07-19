#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd -P)"
baseline="$root/docs/enterprise/contracts/baselines/management-v1.0.0.json"
comparator="$root/scripts/openapi-compatibility.jq"
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT

command -v jq >/dev/null || {
  echo "OpenAPI compatibility regression test requires jq" >&2
  exit 2
}

compare() {
  local candidate=$1
  jq --null-input \
    --slurpfile baseline "$baseline" \
    --slurpfile current "$candidate" \
    --from-file "$comparator"
}

assert_compatible() {
  local name=$1 filter=$2 candidate result
  candidate="$test_root/$name.json"
  jq "$filter" "$baseline" >"$candidate"
  result=$(compare "$candidate")
  jq --exit-status '.compatible == true and (.violations | length == 0)' \
    <<<"$result" >/dev/null || {
    echo "OpenAPI comparator rejected compatible case '$name':" >&2
    jq -r '.violations[] | "  - \(.)"' <<<"$result" >&2
    exit 1
  }
}

assert_incompatible() {
  local name=$1 filter=$2 candidate result
  candidate="$test_root/$name.json"
  jq "$filter" "$baseline" >"$candidate"
  result=$(compare "$candidate")
  jq --exit-status '.compatible == false and (.violations | length > 0)' \
    <<<"$result" >/dev/null || {
    echo "OpenAPI comparator accepted incompatible case '$name'" >&2
    exit 1
  }
}

assert_compatible additive_optional_surface '
  .info.version = "1.1.0"
  | .components.schemas.ApiKeyCatalogResponse.properties.compatibility_optional = {
      "type": ["string", "null"]
    }
  | .paths["/api/v1/api-keys"].get.parameters += [{
      "in": "query",
      "name": "compatibility_optional",
      "required": false,
      "schema": {"type": "string"}
    }]
'

assert_incompatible removed_path '
  del(.paths["/api/v1/api-keys"])
'

assert_incompatible removed_response '
  del(.paths["/api/v1/api-keys"].get.responses["200"])
'

assert_incompatible removed_property '
  del(.components.schemas.ApiKeyCatalogResponse.properties.id)
'

assert_incompatible new_required_input '
  .paths["/api/v1/api-keys"].get.parameters += [{
    "in": "query",
    "name": "required_scope",
    "required": true,
    "schema": {"type": "string"}
  }]
'

assert_incompatible new_required_request_body '
  .paths["/api/v1/api-keys"].get.requestBody = {
    "required": true,
    "content": {
      "application/json": {
        "schema": {"type": "object"}
      }
    }
  }
'

assert_incompatible security_change '
  .paths["/api/v1/api-keys"].get.security = []
'

assert_incompatible narrowed_existing_schema '
  .components.schemas.ApiKeyCatalogResponse.properties.name.pattern = "^[a-z]+$"
'

echo "OpenAPI compatibility comparator regression tests passed"
