#!/usr/bin/env bash
set -euo pipefail

umask 077
local_dir="${OLP_LOCAL_DIR:-$PWD/.local/dev}"
mkdir -p "$local_dir"
export OLP_MASTER_KEY_FILE="$local_dir/master-key.json"
export OLP_AUTH_HMAC_KEY_FILE="$local_dir/auth-hmac-key"
export OLP_BOOTSTRAP_TOKEN_FILE="$local_dir/bootstrap-token"
node --input-type=module <<'JS'
import { randomBytes } from 'node:crypto';
import { existsSync, writeFileSync } from 'node:fs';
for (const [name, json] of [
  ['OLP_MASTER_KEY_FILE', true],
  ['OLP_AUTH_HMAC_KEY_FILE', false],
  ['OLP_BOOTSTRAP_TOKEN_FILE', false]
]) {
  const path = process.env[name];
  if (existsSync(path)) continue;
  const key = randomBytes(32).toString('base64');
  const value = json ? JSON.stringify({ active_version: 1, keys: [{ version: 1, key }] }) : key;
  writeFileSync(path, `${value}\n`, { mode: 0o600 });
}
JS
export OLP_DATABASE_URL="${OLP_DATABASE_URL:-postgres://olp:olp-local@127.0.0.1:54320/olp_dev}"
export OLP_TEST_DATABASE_URL_PREFIX="${OLP_TEST_DATABASE_URL_PREFIX:-postgres://olp:olp-local@127.0.0.1:54320}"
export OLP_TEST_DATABASE_ADMIN_URL="${OLP_TEST_DATABASE_ADMIN_URL:-$OLP_TEST_DATABASE_URL_PREFIX/postgres}"
export OLP_VALKEY_URL="${OLP_VALKEY_URL:-redis://127.0.0.1:63790/0}"
export OLP_PUBLIC_ORIGIN="${OLP_PUBLIC_ORIGIN:-http://localhost:5173}"
export OLP_LISTEN_ADDR="${OLP_LISTEN_ADDR:-127.0.0.1:8081}"
export OLP_OBSERVABILITY_LISTEN_ADDR="${OLP_OBSERVABILITY_LISTEN_ADDR:-127.0.0.1:9091}"
