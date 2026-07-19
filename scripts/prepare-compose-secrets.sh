#!/usr/bin/env bash
set -euo pipefail

# Generate the local Docker Compose secrets exactly once. Existing operator
# supplied material is preserved, including a versioned master-key keyring.
secrets_dir=${OLP_COMPOSE_SECRETS_DIR:-deploy/secrets}
bootstrap_retired_marker="$secrets_dir/.olp_bootstrap_retired"

command -v openssl >/dev/null || {
  echo "openssl is required to prepare Compose secrets" >&2
  exit 1
}

install -d -m 700 "$secrets_dir"
chmod 700 "$secrets_dir"

if [[ -L $bootstrap_retired_marker ]] ||
  [[ -e $bootstrap_retired_marker && ! -f $bootstrap_retired_marker ]]; then
  echo "Compose bootstrap-retirement marker is not a regular file: $bootstrap_retired_marker" >&2
  exit 1
fi

secret_names=(olp_master_key olp_key_hash_key)
if [[ ! -e $bootstrap_retired_marker ]]; then
  secret_names+=(olp_bootstrap_token)
fi

for name in "${secret_names[@]}"; do
  path="$secrets_dir/$name"
  if [[ -L $path ]]; then
    echo "refusing symbolic-link Compose secret: $path" >&2
    exit 1
  fi
  if [[ ! -e $path ]]; then
    temporary=$(mktemp "$secrets_dir/.${name}.XXXXXX") || exit 1
    chmod 600 "$temporary"
    if ! openssl rand -base64 32 > "$temporary" ||
      [[ $(wc -c < "$temporary") -ne 45 ]]; then
      rm -f "$temporary"
      echo "failed to create Compose secret: $path" >&2
      exit 1
    fi
    # A hard link publishes the complete value without replacing a value from
    # a concurrent invocation.
    if ln "$temporary" "$path" 2>/dev/null; then
      echo "generated $path"
    elif [[ -L $path || ! -f $path ]]; then
      rm -f "$temporary"
      echo "failed to create Compose secret: $path" >&2
      exit 1
    fi
    rm -f "$temporary"
  fi
  [[ -f $path ]] || {
    echo "Compose secret is not a regular file: $path" >&2
    exit 1
  }
  chmod 600 "$path"
done

if [[ -e $bootstrap_retired_marker ]]; then
  echo "Compose bootstrap token remains retired in $secrets_dir"
fi

echo "Compose secrets are ready in $secrets_dir"
