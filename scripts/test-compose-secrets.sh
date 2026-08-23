#!/usr/bin/env bash
# Behavioural checks for prepare-compose-secrets.sh and
# retire-compose-bootstrap-secret.sh against a scratch secrets directory.
set -euo pipefail

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
prepare="$script_dir/prepare-compose-secrets.sh"
retire="$script_dir/retire-compose-bootstrap-secret.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

fail() { echo "$1" >&2; exit 1; }

legacy="$work/legacy"
install -d -m 700 "$legacy"
printf 'legacy-key-fixture\n' > "$legacy/olp_key_hash_key"
chmod 600 "$legacy/olp_key_hash_key"
legacy_checksum=$(sha256sum "$legacy/olp_key_hash_key")
if OLP_COMPOSE_SECRETS_DIR="$legacy" "$prepare" >"$work/legacy-error" 2>&1; then
  fail "Compose secret preparation replaced a legacy authentication HMAC key"
fi
[[ $(sha256sum "$legacy/olp_key_hash_key") == "$legacy_checksum" &&
  ! -e "$legacy/olp_auth_hmac_key" && ! -e "$legacy/olp_master_key" ]] ||
  fail "Compose legacy authentication HMAC key guard changed secret files"
grep -Fq 'move or securely copy the existing bytes' "$work/legacy-error" ||
  fail "Compose legacy authentication HMAC key guard is not actionable"

secrets="$work/secrets"
OLP_COMPOSE_SECRETS_DIR="$secrets" "$prepare" >/dev/null
for secret in olp_master_key olp_auth_hmac_key olp_bootstrap_token; do
  [[ -f "$secrets/$secret" ]] || fail "Compose quick-start did not generate $secret"
  [[ $(stat -c '%a' "$secrets/$secret") == 600 ]] || fail "Compose quick-start did not secure $secret"
done
master_key_checksum=$(sha256sum "$secrets/olp_master_key")
auth_hmac_key_checksum=$(sha256sum "$secrets/olp_auth_hmac_key")
OLP_COMPOSE_SECRETS_DIR="$secrets" "$retire" >/dev/null
[[ ! -e "$secrets/olp_bootstrap_token" && -f "$secrets/.olp_bootstrap_retired" ]] ||
  fail "Compose bootstrap retirement did not remove and retire the token"
OLP_COMPOSE_SECRETS_DIR="$secrets" "$prepare" >/dev/null
[[ ! -e "$secrets/olp_bootstrap_token" ]] ||
  fail "Compose secret preparation recreated a retired bootstrap token"
[[ $(sha256sum "$secrets/olp_master_key") == "$master_key_checksum" &&
  $(sha256sum "$secrets/olp_auth_hmac_key") == "$auth_hmac_key_checksum" ]] ||
  fail "Compose bootstrap retirement changed a long-lived key"

echo "compose secret helpers verified"
