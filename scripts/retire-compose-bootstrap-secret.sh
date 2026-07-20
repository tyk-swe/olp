#!/usr/bin/env bash
set -euo pipefail

# Run only after the initialized `olp` service has been recreated from the
# base Compose file. The marker prevents the preparation helper from silently
# generating a new, meaningless token during later maintenance.
secrets_dir=${OLP_COMPOSE_SECRETS_DIR:-deploy/secrets}
token="$secrets_dir/olp_bootstrap_token"
marker="$secrets_dir/.olp_bootstrap_retired"

[[ -d $secrets_dir ]] || {
  echo "Compose secrets directory does not exist: $secrets_dir" >&2
  exit 1
}
for key in olp_master_key olp_auth_hmac_key; do
  path="$secrets_dir/$key"
  if [[ -L $path || ! -f $path ]]; then
    echo "required long-lived Compose secret is unavailable: $path" >&2
    exit 1
  fi
done
if [[ -L $token ]]; then
  echo "refusing symbolic-link Compose bootstrap token: $token" >&2
  exit 1
fi
if [[ -L $marker ]] || [[ -e $marker && ! -f $marker ]]; then
  echo "Compose bootstrap-retirement marker is not a regular file: $marker" >&2
  exit 1
fi

if [[ ! -e $marker ]]; then
  (umask 077; : > "$marker")
fi
chmod 600 "$marker"
rm -f -- "$token"
echo "retired Compose bootstrap token; use deploy/compose.yaml without the bootstrap overlay"
