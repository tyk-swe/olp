#!/usr/bin/env bash
set -euo pipefail

# Backup / restore / upgrade rehearsal driven by CI's upgrade-rehearsal job
# and reproducible locally against a disposable PostgreSQL 18 + Valkey.
# The caller has already built the candidate binary and migrated the current-release
# database (OLP_DATABASE_URL).
#
# Required environment:
#   OLP_REHEARSAL_SERVER_URL  server URL without a database name, e.g.
#                             postgres://olp:olp@localhost:5432
#   OLP_DATABASE_URL          migrated current-release database
#   OLP_VALKEY_URL            Valkey for the doctor smoke
#   OLP_BIN                   path to the olp binary
# Optional:
#   OLP_PREVIOUS_BIN          explicit path to pre-built previous olp binary
#   OLP_REHEARSAL_SCRATCH_DIR scratch directory (default: mktemp -d)
#   OLP_EXPECTED_PG_BINDIR    assert pg_dump/pg_restore resolve from here
#                             (CI uses it to prove the PGDG install won)

: "${OLP_REHEARSAL_SERVER_URL:?set OLP_REHEARSAL_SERVER_URL without a trailing database name}"
: "${OLP_DATABASE_URL:?set OLP_DATABASE_URL to the migrated current-release database}"
: "${OLP_VALKEY_URL:?set OLP_VALKEY_URL for the doctor smoke}"
: "${OLP_BIN:?set OLP_BIN to the olp binary}"

script_dir=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd "$script_dir/../.." && pwd)
# shellcheck source=scripts/lib/cargo-target-dir.sh
source "$repo_root/scripts/lib/cargo-target-dir.sh"

scratch_dir=${OLP_REHEARSAL_SCRATCH_DIR:-$(mktemp -d)}

if [[ -n ${OLP_EXPECTED_PG_BINDIR:-} ]]; then
  [[ $(command -v pg_dump) == "$OLP_EXPECTED_PG_BINDIR/pg_dump" ]]
  [[ $(command -v pg_restore) == "$OLP_EXPECTED_PG_BINDIR/pg_restore" ]]
fi
pg_dump --version | grep -Eq '^pg_dump \(PostgreSQL\) 18([.]|$)'
pg_restore --version | grep -Eq '^pg_restore \(PostgreSQL\) 18([.]|$)'

for db in olp_restore olp_legacy_restore olp_upgrade olp_previous; do
  psql "$OLP_REHEARSAL_SERVER_URL/postgres" -v ON_ERROR_STOP=1 \
    -c "DROP DATABASE IF EXISTS $db"
  psql "$OLP_REHEARSAL_SERVER_URL/postgres" -v ON_ERROR_STOP=1 \
    -c "CREATE DATABASE $db"
done

backup=$(./scripts/backup.sh "$scratch_dir/olp-backups")
./scripts/backup-manifest.sh validate "$backup" v2 >/dev/null
OLP_RESTORE_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_restore" \
  ./scripts/restore-rehearsal.sh "$backup"
./scripts/backup-manifest.sh convert-v2-to-v1 "$backup"
./scripts/backup-manifest.sh validate "$backup" v1 >/dev/null
OLP_RESTORE_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_legacy_restore" \
  ./scripts/restore-rehearsal.sh "$backup"

metadata_file="$repo_root/release-metadata.env"
[[ -f $metadata_file ]] || {
  echo "required release metadata file is missing: $metadata_file" >&2
  exit 1
}

previous_version=''
previous_migration=''
previous_commit=''
previous_image_digest=''
metadata_assignments=0

while IFS= read -r line || [[ -n $line ]]; do
  [[ $line =~ ^[[:space:]]*($|#) ]] && continue
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_VERSION=(.+)$ ]]; then
    [[ -z $previous_version ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_VERSION assignment in $metadata_file" >&2
      exit 1
    }
    previous_version=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION=(.+)$ ]]; then
    [[ -z $previous_migration ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_SCHEMA_MIGRATION assignment in $metadata_file" >&2
      exit 1
    }
    previous_migration=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_COMMIT=(.+)$ ]]; then
    [[ -z $previous_commit ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_COMMIT assignment in $metadata_file" >&2
      exit 1
    }
    previous_commit=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  if [[ $line =~ ^OLP_PREVIOUS_RELEASED_IMAGE_DIGEST=(.+)$ ]]; then
    [[ -z $previous_image_digest ]] || {
      echo "duplicate OLP_PREVIOUS_RELEASED_IMAGE_DIGEST assignment in $metadata_file" >&2
      exit 1
    }
    previous_image_digest=${BASH_REMATCH[1]}
    ((metadata_assignments += 1))
    continue
  fi
  echo "release metadata contains an unsupported line: $line" >&2
  exit 1
done <"$metadata_file"

(( metadata_assignments == 4 )) || {
  echo "release metadata must contain all 4 required assignments" >&2
  exit 1
}

semver='^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$'
[[ $previous_version =~ $semver ]] || {
  echo "previous released version is not semantic: $previous_version" >&2
  exit 1
}
[[ $previous_migration =~ ^[0-9]{4}$ ]] || {
  echo "previous released schema migration must be 4 digits: $previous_migration" >&2
  exit 1
}
[[ $previous_commit =~ ^[0-9a-f]{40}$ ]] || {
  echo "previous released commit must be a 40-character hex SHA: $previous_commit" >&2
  exit 1
}
[[ $previous_image_digest =~ ^sha256:[0-9a-f]{64}$ ]] || {
  echo "previous released image digest must be sha256:<64-hex>: $previous_image_digest" >&2
  exit 1
}

if [[ -n ${OLP_PREVIOUS_BIN:-} ]]; then
  previous_bin="$OLP_PREVIOUS_BIN"
else
  git cat-file -e "$previous_commit^{commit}" || {
    echo "previous released commit $previous_commit does not exist in git history" >&2
    exit 1
  }

  isolated_src="$scratch_dir/n1-src"
  mkdir -p "$isolated_src"
  git archive "$previous_commit" | tar -x -C "$isolated_src"
  sed -i "s/^version = \".*\"/version = \"$previous_version\"/" "$isolated_src/Cargo.toml"
  sed -i -E "s/(path = \"crates\/[^\"]+\", version = \")[^\"]+(\")/\1$previous_version\2/g" "$isolated_src/Cargo.toml"
  for mig in "$isolated_src"/crates/*/migrations/[0-9][0-9][0-9][0-9]_*.sql; do
    [[ -f $mig ]] || continue
    mig_name=${mig##*/}
    mig_num=${mig_name%%_*}
    if (( 10#$mig_num > 10#$previous_migration )); then
      rm -f "$mig"
    fi
  done

  n1_target_dir=$(cargo_target_dir "$repo_root")/previous-bin
  CARGO_TARGET_DIR="$n1_target_dir" cargo build --offline -p olp --manifest-path "$isolated_src/Cargo.toml"
  previous_bin="$n1_target_dir/debug/olp"
fi

[[ -x $previous_bin ]] || {
  echo "previous binary is not executable: $previous_bin" >&2
  exit 1
}

observed_previous_version=$("$previous_bin" --version)
[[ $observed_previous_version == "olp $previous_version" ]] || {
  echo "previous binary version mismatch: observed '$observed_previous_version', expected 'olp $previous_version'" >&2
  exit 1
}

candidate_version=$("$OLP_BIN" --version)
if [[ $candidate_version == "olp $previous_version" ]]; then
  echo "candidate binary unexpectedly matches previous release version $previous_version" >&2
  exit 1
fi

OLP_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_previous" "$previous_bin" migrate
observed_n1_migration=$(psql "$OLP_REHEARSAL_SERVER_URL/olp_previous" -Atc "SELECT coalesce(max(version), 0) FROM _sqlx_migrations WHERE success")
(( observed_n1_migration == 10#$previous_migration )) || {
  echo "previous binary migrated to schema version $observed_n1_migration, expected $previous_migration" >&2
  exit 1
}

psql "$OLP_REHEARSAL_SERVER_URL/olp_previous" -v ON_ERROR_STOP=1 << 'EOF'
DO $$
DECLARE
  owner_id uuid := '018f0000-0000-7000-8000-000000000001';
  provider_id uuid := '018f0000-0000-7000-8000-000000000002';
  provider_model_id uuid := '018f0000-0000-7000-8000-000000000003';
  route_id uuid := '018f0000-0000-7000-8000-000000000004';
  draft_id uuid := '018f0000-0000-7000-8000-000000000005';
  draft_target_id uuid := '018f0000-0000-7000-8000-000000000006';
  route_rev_id uuid := '018f0000-0000-7000-8000-000000000007';
  rev_target_id uuid := '018f0000-0000-7000-8000-000000000008';
  api_key_id uuid := '018f0000-0000-7000-8000-000000000009';
  gen_id uuid := '018f0000-0000-7000-8000-00000000000a';
  prov_rev_id uuid := '018f0000-0000-7000-8000-00000000000b';
  prov_rev_model_id uuid := '018f0000-0000-7000-8000-00000000000c';
  pricing_rev_id uuid := '018f0000-0000-7000-8000-00000000000d';
  req_id uuid := '018f0000-0000-7000-8000-00000000000e';
  fact_id uuid := '018f0000-0000-7000-8000-00000000000f';
  obs_at timestamptz := now() - interval '2 days';
BEGIN
  INSERT INTO installation (organization_name) VALUES ('Upgrade Rehearsal Organization');
  INSERT INTO users (id, email, display_name, role) VALUES (owner_id, 'owner@example.test', 'Owner', 'owner');
  INSERT INTO providers (id, name, kind, state, auth_mode, etag, created_by)
    VALUES (provider_id, 'upgrade-provider', 'open_ai', 'draft', 'api_key', '018f0000-0000-7000-8000-000000000010', owner_id);
  INSERT INTO provider_models (id, provider_id, upstream_model, display_name, enabled)
    VALUES (provider_model_id, provider_id, 'gpt-4o', 'GPT-4o', true);
  INSERT INTO route_drafts (id, slug, state, overall_timeout_ms, max_attempts, etag, created_by)
    VALUES (draft_id, 'upgrade-route', 'validated', 30000, 1, '018f0000-0000-7000-8000-000000000011', owner_id);
  INSERT INTO route_draft_targets (id, route_draft_id, provider_model_id, priority, weight, timeout_ms, position)
    VALUES (draft_target_id, draft_id, provider_model_id, 0, 1, 20000, 0);
  INSERT INTO routes (id, slug, created_by) VALUES (route_id, 'upgrade-route', owner_id);
  INSERT INTO route_revisions (id, route_id, revision, slug, overall_timeout_ms, max_attempts, source_draft_id, activated_by)
    VALUES (route_rev_id, route_id, 1, 'upgrade-route', 30000, 1, draft_id, owner_id);
  INSERT INTO route_revision_targets (id, route_revision_id, provider_model_id, priority, weight, timeout_ms, position)
    VALUES (rev_target_id, route_rev_id, provider_model_id, 0, 1, 20000, 0);
  INSERT INTO api_keys (id, lookup_id, secret_digest, name, created_by)
    VALUES (api_key_id, 'olpv2upgrade21', decode('0707070707070707070707070707070707070707070707070707070707070707', 'hex'), 'upgrade key', owner_id);
  INSERT INTO runtime_generations (id, compiled_release, release_sha256, created_by)
    VALUES (gen_id, E'\\x01', decode('0202020202020202020202020202020202020202020202020202020202020202', 'hex'), owner_id);
  INSERT INTO model_capabilities (provider_model_id, operation, surface, mode, source)
    VALUES (provider_model_id, 'generation', 'open_ai', 'unary', 'declared');
  INSERT INTO provider_revisions (id, provider_id, revision, name, kind, auth_mode, connector_ready, source_etag, activated_by)
    VALUES (prov_rev_id, provider_id, 1, 'upgrade-provider', 'open_ai', 'api_key', true, '018f0000-0000-7000-8000-000000000010', owner_id);
  INSERT INTO provider_revision_models (id, provider_revision_id, source_provider_model_id, upstream_model, display_name, enabled)
    VALUES (prov_rev_model_id, prov_rev_id, provider_model_id, 'gpt-4o', 'GPT-4o', true);
  INSERT INTO provider_revision_capabilities (provider_revision_model_id, operation, surface, mode, source)
    VALUES (prov_rev_model_id, 'generation', 'open_ai', 'unary', 'declared');
  INSERT INTO runtime_generation_provider_configs (runtime_generation_id, provider_id, kind, auth_mode, provider_revision_id)
    VALUES (gen_id, provider_id, 'open_ai', 'api_key', prov_rev_id);
  INSERT INTO pricing_revisions (id, revision, effective_at, created_by)
    VALUES (pricing_rev_id, 1, now(), owner_id);
  INSERT INTO prices (pricing_revision_id, provider_kind, model, operation, input_per_million)
    VALUES (pricing_rev_id, 'open_ai', 'gpt-4o', 'generation', 1);
  INSERT INTO requests (id, runtime_generation_id, api_key_id, route_slug, operation, surface, started_at, completed_at, status_code, total_latency_ms, attempt_count)
    VALUES (req_id, gen_id, api_key_id, 'upgrade-route', 'generation', 'open_ai', obs_at - interval '10 milliseconds', obs_at, 200, 10, 1);
  INSERT INTO usage_request_anchors (request_id, request_started_at)
    VALUES (req_id, obs_at - interval '10 milliseconds');
  INSERT INTO usage_facts (id, request_id, request_started_at, api_key_id, provider_id, route_slug, upstream_model, operation, surface, observed_at, input_tokens, output_tokens, unpriced, usage_complete)
    VALUES (fact_id, req_id, obs_at - interval '10 milliseconds', api_key_id, provider_id, 'upgrade-route', 'gpt-4o', 'generation', 'open_ai', obs_at, 3, 2, true, true);
  INSERT INTO usage_hourly (bucket, route_slug, provider_id, upstream_model, operation, surface, api_key_id, request_count, input_tokens, output_tokens, cached_input_tokens, media_units, unpriced_count, incomplete_count)
    VALUES (date_trunc('hour', obs_at - interval '10 days'), 'retained', provider_id, 'gpt-4o', 'generation', 'open_ai', api_key_id, 4, 12, 8, 0, 0, 4, 0);
  INSERT INTO usage_consumer_health (singleton, pending_events, lag_events, checked_at)
    VALUES (true, 0, 0, now())
    ON CONFLICT (singleton) DO UPDATE SET pending_events = 0, lag_events = 0, checked_at = now();
END $$;
EOF

previous_backup=$(OLP_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_previous" \
  OLP_BACKUP_TRAFFIC_QUIESCED=true \
  ./scripts/backup.sh "$scratch_dir/olp-previous-backups")
./scripts/backup-manifest.sh validate "$previous_backup" v1 >/dev/null

doctor_root=$(mktemp -d)
mkdir -p "$doctor_root/console" "$doctor_root/media"
printf '<!doctype html><html></html>\n' >"$doctor_root/console/index.html"
openssl rand -base64 32 >"$doctor_root/master-key"
openssl rand -base64 32 >"$doctor_root/auth-hmac-key"
chmod 600 "$doctor_root/master-key" "$doctor_root/auth-hmac-key"

OLP_REHEARSAL_DATABASE_URL="$OLP_REHEARSAL_SERVER_URL/olp_upgrade" \
  OLP_REHEARSAL_CONFIRM=destroy-target \
  OLP_REHEARSAL_PREVIOUS_RELEASED_SCHEMA_MIGRATION="$previous_migration" \
  OLP_REHEARSAL_RUN_DOCTOR=true \
  OLP_MASTER_KEY_FILE="$doctor_root/master-key" \
  OLP_AUTH_HMAC_KEY_FILE="$doctor_root/auth-hmac-key" \
  OLP_CONSOLE_DIR="$doctor_root/console" \
  OLP_MEDIA_SPOOL_DIR="$doctor_root/media" \
  ./scripts/upgrade-rehearsal.sh "$previous_backup"

upgraded_kind=$(psql "$OLP_REHEARSAL_SERVER_URL/olp_upgrade" -Atc "SELECT kind FROM providers WHERE id = '018f0000-0000-7000-8000-000000000002'")
[[ $upgraded_kind == "openai" ]] || {
  echo "provider kind was not migrated to openai: observed '$upgraded_kind'" >&2
  exit 1
}

upgraded_attempt_facts=$(psql "$OLP_REHEARSAL_SERVER_URL/olp_upgrade" -Atc "SELECT count(*) FROM attempt_usage_facts WHERE request_id = '018f0000-0000-7000-8000-00000000000e'")
(( upgraded_attempt_facts == 1 )) || {
  echo "attempt_usage_facts count mismatch: observed $upgraded_attempt_facts, expected 1" >&2
  exit 1
}

upgraded_rollups=$(psql "$OLP_REHEARSAL_SERVER_URL/olp_upgrade" -Atc "SELECT coalesce(sum(request_count), 0)::bigint FROM attempt_usage_hourly")
(( upgraded_rollups == 4 )) || {
  echo "attempt_usage_hourly sum mismatch: observed $upgraded_rollups, expected 4" >&2
  exit 1
}

echo "upgrade rehearsal and post-upgrade state verification completed successfully"
