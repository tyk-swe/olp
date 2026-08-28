#!/usr/bin/env bash
set -euo pipefail

# Runs the Rust-hosted console integration against the prebuilt console
# bundle and olp binary. The secrets the gateway needs are generated here as
# throwaway files; their paths (and the database/Valkey URLs) come from the
# OLP_CONSOLE_E2E_* environment the caller sets.

: "${OLP_CONSOLE_E2E_MASTER_KEY_FILE:?set OLP_CONSOLE_E2E_MASTER_KEY_FILE}"
: "${OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE:?set OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE}"
: "${OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE:?set OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE}"

umask 077
openssl rand -base64 32 > "$OLP_CONSOLE_E2E_MASTER_KEY_FILE"
openssl rand -base64 32 > "$OLP_CONSOLE_E2E_AUTH_HMAC_KEY_FILE"
printf '%s\n' 'AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=' \
  > "$OLP_CONSOLE_E2E_BOOTSTRAP_TOKEN_FILE"
pnpm --dir console test:integration:prebuilt
