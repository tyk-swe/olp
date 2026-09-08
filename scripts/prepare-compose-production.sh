#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
umask 077
secrets_dir=${OLP_COMPOSE_SECRETS_DIR:-deploy/secrets}
./scripts/prepare-compose-secrets.sh
password_file="$secrets_dir/olp_database_password"
environment_file="$secrets_dir/production.env"
[[ ! -L $password_file && ! -L $environment_file ]] || { echo 'Refusing symbolic-link production secrets.' >&2; exit 1; }
if [[ ! -e $password_file ]]; then
  temporary=$(mktemp "$secrets_dir/.database-password.XXXXXX")
  openssl rand -hex 32 > "$temporary"
  ln "$temporary" "$password_file" 2>/dev/null || [[ -f $password_file ]]
  rm -f "$temporary"
fi
[[ -f $password_file ]] || exit 1
password=$(cat "$password_file")
[[ -n $password && $password != olp && $password != *$'\n'* && $password != *$'\r'* ]] || {
  echo 'Production database password must be nonempty, single-line and different from the demonstration password.' >&2
  exit 1
}
encoded=$(printf '%s' "$password" | jq -sRr @uri)
temporary=$(mktemp "$secrets_dir/.production-env.XXXXXX")
printf 'OLP_DATABASE_URL=postgres://olp:%s@postgres:5432/olp\n' "$encoded" > "$temporary"
chmod 600 "$password_file" "$temporary"
mv "$temporary" "$environment_file"
echo "Production database credentials prepared in $secrets_dir. Use the production overlay and its env file."
