#!/usr/bin/env bash
set -euo pipefail

major=${1:-}
if [[ ! $major =~ ^[1-9][0-9]*$ ]]; then
  echo "usage: $0 POSTGRESQL_MAJOR_VERSION" >&2
  exit 64
fi

for command in apt-get awk curl dpkg gpg grep sudo tee; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

: "${GITHUB_PATH:?GITHUB_PATH is required when installing PostgreSQL client tools in CI}"

key_dir=/usr/share/postgresql-common/pgdg
key_file=$key_dir/apt.postgresql.org.asc
source_file=/etc/apt/sources.list.d/pgdg.sources
expected_fingerprint=B97B0AFCAA1A47F044F244A07FCC7D46ACCC4CF8

downloaded_key=$(mktemp)
cleanup() {
  rm -f -- "$downloaded_key"
}
trap cleanup EXIT

curl --fail --location --proto '=https' --tlsv1.2 --retry 3 \
  --silent --show-error --output "$downloaded_key" \
  https://www.postgresql.org/media/keys/ACCC4CF8.asc

fingerprint=$(gpg --batch --show-keys --with-colons "$downloaded_key" \
  | awk -F: '$1 == "fpr" { print $10; exit }')
if [[ $fingerprint != "$expected_fingerprint" ]]; then
  echo "unexpected PostgreSQL Apt signing-key fingerprint: ${fingerprint:-missing}" >&2
  exit 1
fi
sudo install -d -m 0755 "$key_dir"
sudo install -m 0644 "$downloaded_key" "$key_file"

# Use the deb822 format documented by the PostgreSQL Apt repository. Keeping
# this in one script prevents the HA, integration, and upgrade jobs drifting.
# shellcheck disable=SC1091
. /etc/os-release
: "${VERSION_CODENAME:?Ubuntu VERSION_CODENAME is unavailable}"
architecture=$(dpkg --print-architecture)

sudo tee "$source_file" >/dev/null <<EOF_SOURCES
Types: deb
URIs: https://apt.postgresql.org/pub/repos/apt
Suites: ${VERSION_CODENAME}-pgdg
Components: main
Architectures: ${architecture}
Signed-By: ${key_file}
EOF_SOURCES

sudo apt-get -o Acquire::Retries=3 update
sudo apt-get -o Acquire::Retries=3 install --yes --no-install-recommends \
  "postgresql-client-$major"

bin_dir="/usr/lib/postgresql/$major/bin"
for tool in psql createdb dropdb pg_dump pg_restore; do
  [[ -x "$bin_dir/$tool" ]] || {
    echo "PostgreSQL $major client installation is missing $bin_dir/$tool" >&2
    exit 1
  }
  "$bin_dir/$tool" --version | grep -Eq "\(PostgreSQL\) ${major}([.]|$)"
done

printf '%s\n' "$bin_dir" >> "$GITHUB_PATH"
echo "PostgreSQL $major client tools installed from the PGDG repository"
