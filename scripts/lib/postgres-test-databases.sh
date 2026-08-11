# shellcheck shell=bash
# Shared PostgreSQL cleanup primitives for local and end-to-end test harnesses.

postgres_test_run_token() {
  local digest
  digest=$(printf '%s' \
    "${GITHUB_RUN_ID:-}_${GITHUB_RUN_ATTEMPT:-}_$$_${RANDOM}_$(date +%s%N)" \
    | sha256sum) || return 1
  printf '%.10s\n' "$digest"
}

postgres_test_sweep_databases() {
  local admin_url=$1
  local database_prefix=$2
  local suffix_kind=$3
  local suite_label=$4
  local suffix_pattern

  if [[ ! $database_prefix =~ ^[a-z0-9_]+_$ ]]; then
    echo "refusing unsafe PostgreSQL test database prefix: $database_prefix" >&2
    return 2
  fi

  case "$suffix_kind" in
    lower-identifier)
      suffix_pattern='[a-z0-9_]+'
      ;;
    lower-hex)
      suffix_pattern='[a-f0-9]+'
      ;;
    *)
      echo "unsupported PostgreSQL test database suffix kind: $suffix_kind" >&2
      return 2
      ;;
  esac

  local database_pattern="^${database_prefix}${suffix_pattern}$"
  local leftovers database
  # The server-side regex is string-anchored (^/$ do not match around
  # embedded newlines in PostgreSQL), so newline-framed output cannot be used
  # to smuggle a second database name into the DROP loop. The per-line check
  # below independently protects the quoted identifier.
  if ! leftovers=$(timeout --kill-after=5s 30s \
    psql "$admin_url" --no-psqlrc --tuples-only --no-align \
    --command "SELECT datname FROM pg_database
               WHERE datname ~ '${database_pattern}'"); then
    echo "failed to list $suite_label databases" >&2
    return 1
  fi

  while IFS= read -r database; do
    [[ -n $database ]] || continue
    if [[ ! $database =~ $database_pattern ]]; then
      echo "refusing to drop suspicious $suite_label database name: $database" >&2
      return 1
    fi
    echo "dropping leftover $suite_label database $database"
    timeout --kill-after=5s 30s \
      psql "$admin_url" --no-psqlrc --quiet \
      --command "DROP DATABASE IF EXISTS \"$database\" WITH (FORCE)" >/dev/null || return 1
  done <<<"$leftovers"
}
