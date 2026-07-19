#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'USAGE'
usage: scripts/check-migration-contract.sh [--self-test-rules]

Validate every SQL migration after the frozen M0 cutoff against its required
compatibility sidecar and the expand-migrate-contract policy. The optional
self-test proves that every frozen unsafe-SQL example is detected by its rule.
USAGE
}

mode=validate
if [[ $# -eq 1 && $1 == "--self-test-rules" ]]; then
  mode=self-test-rules
elif [[ $# -ne 0 ]]; then
  usage >&2
  exit 2
fi

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
contract="$root/docs/enterprise/contracts/compatibility.json"
migration_dir="$root/crates/storage/migrations"
fixture_dir="$root/tests/migration-fixtures"
sql_comment_normalizer="$root/scripts/normalize-sql-comments.pl"
readonly frozen_grandfathered_through=27

for command in find jq perl rg sha256sum sort; do
  command -v "$command" >/dev/null || {
    echo "required command is unavailable: $command" >&2
    exit 1
  }
done

jq -e . "$contract" >/dev/null || {
  echo "compatibility contract is not valid JSON: $contract" >&2
  exit 1
}
[[ -f $sql_comment_normalizer ]] || {
  echo "SQL comment normalizer is missing: $sql_comment_normalizer" >&2
  exit 1
}

regex_matches() {
  local label=$1 pattern=$2 input=$3 status
  rg --quiet --ignore-case --multiline --multiline-dotall --pcre2 "$pattern" <<<"$input" && return 0
  status=$?
  if (( status == 1 )); then
    return 1
  fi
  echo "unsafe-SQL rule '$label' is not a valid PCRE2 expression" >&2
  exit 1
}

sanitize_sql_comments() {
  perl "$sql_comment_normalizer" "$@"
}

if [[ $mode == self-test-rules ]]; then
  while IFS=$'\t' read -r expected_rule sql; do
    pattern=$(jq -er --arg id "$expected_rule" '
      [.migration_contract.unsafe_sql_rules[] | select(.id == $id) | .pattern]
      | select(length == 1) | .[0]
    ' "$contract") || {
      echo "unsafe-SQL self-test references a missing or duplicate rule: $expected_rule" >&2
      exit 1
    }
    regex_matches "$expected_rule" "$pattern" "$sql" || {
      echo "unsafe-SQL rule '$expected_rule' did not detect: $sql" >&2
      exit 1
    }
  done <<'CASES'
drop-object	DROP VIEW legacy_route_view;
drop-object	DROP MATERIALIZED VIEW legacy_usage_view;
drop-object	DROP PROCEDURE rewrite_scope();
drop-object	DROP ROUTINE rewrite_scope;
drop-object	DROP EVENT TRIGGER ddl_guard;
immediate-constraint	ALTER TABLE children ADD CONSTRAINT children_parent_fk FOREIGN KEY (parent_id) REFERENCES parents(id);
immediate-constraint	ALTER TABLE requests ADD CONSTRAINT c1 CHECK (x > 0), ADD CONSTRAINT c2 CHECK (y > 0) NOT VALID;
immediate-constraint	ALTER TABLE requests ADD CONSTRAINT c1 CHECK (x > 0) NOT VALID, ADD CONSTRAINT c2 CHECK (y > 0);
column-default	ALTER TABLE requests ADD COLUMN created_at timestamptz DEFAULT clock_timestamp();
drop-column-default	ALTER TABLE requests ALTER COLUMN state DROP DEFAULT;
rename-object	ALTER TABLE requests RENAME COLUMN old_scope TO scope_id;
alter-column-type	ALTER TABLE requests ALTER COLUMN scope_id TYPE text;
destructive-data-change	DELETE FROM requests;
row-security-change	ALTER TABLE requests ENABLE ROW LEVEL SECURITY;
row-security-change	ALTER TABLE requests DISABLE ROW LEVEL SECURITY;
row-security-change	CREATE POLICY tenant_only ON requests USING (organization_id = current_setting('olp.organization_id')::uuid);
unbounded-sql-backfill	UPDATE ONLY requests SET organization_id = '00000000-0000-0000-0000-000000000001';
privilege-change	REVOKE INSERT ON requests FROM olp_gateway;
set-not-null	ALTER TABLE requests ADD COLUMN organization_id uuid NOT NULL;
blocking-index-build	CREATE INDEX requests_org_idx ON requests (organization_id);
unique-index-behavior	CREATE UNIQUE INDEX CONCURRENTLY requests_external_id_uq ON requests (external_id);
enum-expansion	ALTER TYPE request_state ADD VALUE 'new_state';
behavioral-database-fence	CREATE TRIGGER requests_scope BEFORE INSERT ON requests EXECUTE FUNCTION set_scope();
behavioral-database-fence	CREATE OR REPLACE PROCEDURE rewrite_scope() LANGUAGE SQL AS 'SELECT 1';
behavioral-database-fence	CREATE EVENT TRIGGER ddl_guard ON ddl_command_start EXECUTE FUNCTION guard_ddl();
behavioral-database-fence	CREATE CONSTRAINT TRIGGER scope_guard AFTER INSERT ON requests DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION enforce_scope();
behavioral-database-fence	CREATE OR REPLACE TRIGGER scope_guard BEFORE INSERT ON requests FOR EACH ROW EXECUTE FUNCTION enforce_scope();
behavioral-database-fence	CREATE RULE request_guard AS ON UPDATE TO requests DO NOTHING;
behavioral-database-fence	ALTER EVENT TRIGGER ddl_guard DISABLE;
behavioral-database-fence	ALTER TABLE requests DISABLE TRIGGER scope_guard;
replace-view	CREATE OR REPLACE VIEW current_requests AS SELECT id FROM requests;
constraint-validation	ALTER TABLE requests VALIDATE CONSTRAINT requests_org_fk;
CASES

  comment_spaced_sql=$(sanitize_sql_comments <<<'DROP/**/TABLE requests;')
  drop_pattern=$(jq -er '.migration_contract.unsafe_sql_rules[] | select(.id == "drop-object") | .pattern' "$contract")
  regex_matches "drop-object-comment-separated" "$drop_pattern" "$comment_spaced_sql" || {
    echo "unsafe-SQL sanitizer hid a comment-separated DROP TABLE" >&2
    exit 1
  }

  nested_comment_sql=$(sanitize_sql_comments <<<'DROP/* outer /* inner */ tail */TABLE requests;')
  regex_matches "drop-object-nested-comment-separated" "$drop_pattern" "$nested_comment_sql" || {
    echo "unsafe-SQL sanitizer hid a nested-comment-separated DROP TABLE" >&2
    exit 1
  }
  quoted_comment_sql=$(sanitize_sql_comments <<<'SELECT '\''-- string data'\''; DROP TABLE requests;')
  regex_matches "drop-object-after-quoted-comment-marker" "$drop_pattern" "$quoted_comment_sql" || {
    echo "unsafe-SQL sanitizer treated a quoted comment marker as a comment" >&2
    exit 1
  }
  # The PostgreSQL dollar-quote marker must reach the normalizer literally.
  # shellcheck disable=SC2016
  dollar_quoted_comment_sql=$(sanitize_sql_comments <<<'SELECT $body$/* string data */$body$; DROP TABLE requests;')
  regex_matches "drop-object-after-dollar-quoted-comment-marker" "$drop_pattern" "$dollar_quoted_comment_sql" || {
    echo "unsafe-SQL sanitizer treated a dollar-quoted marker as a comment" >&2
    exit 1
  }
  escape_quoted_comment_sql=$(sanitize_sql_comments <<'SQL'
SELECT E'escaped\'quote -- string data'; DROP TABLE requests;
SQL
)
  regex_matches "drop-object-after-escape-quoted-comment-marker" "$drop_pattern" "$escape_quoted_comment_sql" || {
    echo "unsafe-SQL sanitizer mishandled a PostgreSQL E string" >&2
    exit 1
  }
  if sanitize_sql_comments <<<'SELECT 1 /* unmatched' >/dev/null 2>&1; then
    echo "unsafe-SQL sanitizer accepted an unmatched block-comment delimiter" >&2
    exit 1
  fi

  safe_expand='ALTER TABLE requests ADD COLUMN organization_id uuid;'
  while IFS= read -r rule_id; do
    pattern=$(jq -er --arg id "$rule_id" '.migration_contract.unsafe_sql_rules[] | select(.id == $id) | .pattern' "$contract")
    if regex_matches "$rule_id" "$pattern" "$safe_expand"; then
      echo "unsafe-SQL rules rejected the additive nullable-column control case" >&2
      exit 1
    fi
  done < <(jq -r '.migration_contract.unsafe_sql_rules[].id' "$contract")

  safe_not_valid='ALTER TABLE requests ADD CONSTRAINT requests_org_fk FOREIGN KEY (organization_id) REFERENCES organizations(id) NOT VALID;'
  immediate_constraint_pattern=$(jq -er '.migration_contract.unsafe_sql_rules[] | select(.id == "immediate-constraint") | .pattern' "$contract")
  if regex_matches "immediate-constraint" "$immediate_constraint_pattern" "$safe_not_valid"; then
    echo "immediate-constraint rule rejected the NOT VALID control case" >&2
    exit 1
  fi
  safe_multiple_not_valid='ALTER TABLE requests ADD CONSTRAINT c1 CHECK (x > 0) NOT VALID, ADD CONSTRAINT c2 CHECK (y > 0) NOT VALID;'
  if regex_matches "immediate-constraint" "$immediate_constraint_pattern" "$safe_multiple_not_valid"; then
    echo "immediate-constraint rule rejected multiple NOT VALID control cases" >&2
    exit 1
  fi

  echo "migration unsafe-SQL rule self-test passed"
  exit 0
fi

representative_manifest_relative=$(jq -er '.migration_contract.representative_fixture_manifest | select(type == "string" and length > 0)' "$contract")
representative_manifest="$root/$representative_manifest_relative"
jq -e '
  .schema_version == 1 and
  .contract_id == "olp.enterprise.compatibility.v1" and
  .qualification_status == "specification_only" and
  .source_release.immutable_identity_available == false and
  .artifact.generated_test_secrets_only == true and
  .artifact.contains_request_or_response_content == false and
  (.required_populations | type == "array" and length > 0) and
  (.provisional_current_evidence | type == "array" and length > 0) and
  (.qualification_requirements | type == "array" and length > 0)
' "$representative_manifest" >/dev/null || {
  echo "representative 2.x fixture manifest is missing or overstates qualification: $representative_manifest" >&2
  exit 1
}
while IFS= read -r evidence; do
  [[ -e $root/$evidence ]] || {
    echo "provisional compatibility evidence does not exist: $evidence" >&2
    exit 1
  }
done < <(jq -r '.provisional_current_evidence[].path' "$representative_manifest")

cutoff=$(jq -er '.migration_contract.grandfathered_through | select(type == "number" and floor == .)' "$contract")
if (( cutoff != frozen_grandfathered_through )); then
  echo "migration grandfather cutoff is frozen at $frozen_grandfathered_through; found $cutoff" >&2
  echo "changing it requires a superseding ADR and checker change" >&2
  exit 1
fi

mapfile -t migration_files < <(find "$migration_dir" -maxdepth 1 -type f -name '*.sql' -print | LC_ALL=C sort)
(( ${#migration_files[@]} > 0 )) || {
  echo "no SQL migrations found in $migration_dir" >&2
  exit 1
}

previous=0
for migration_file in "${migration_files[@]}"; do
  basename=${migration_file##*/}
  [[ $basename =~ ^([0-9]{4})_[a-z0-9_]+[.]sql$ ]] || {
    echo "migration filename violates the frozen pattern: $basename" >&2
    exit 1
  }
  version_token=${BASH_REMATCH[1]}
  version=$((10#$version_token))
  (( version == previous + 1 )) || {
    echo "migration sequence must be contiguous: expected $((previous + 1)), found $version ($basename)" >&2
    exit 1
  }
  previous=$version

  if (( version <= cutoff )); then
    expected_sha=$(jq -er --arg file "$basename" '.migration_contract.grandfathered_migration_sha256[$file] | select(type == "string" and test("^[a-f0-9]{64}$"))' "$contract") || {
      echo "grandfathered migration hash is missing or invalid: $basename" >&2
      exit 1
    }
    read -r actual_sha _ < <(sha256sum "$migration_file")
    [[ $actual_sha == "$expected_sha" ]] || {
      echo "grandfathered migration is immutable and its SHA-256 changed: $basename" >&2
      exit 1
    }
    continue
  fi

  stem=${basename%.sql}
  sidecar="$fixture_dir/${stem}.contract.json"
  [[ -f $sidecar ]] || {
    echo "post-M0 migration is missing its compatibility sidecar: $sidecar" >&2
    exit 1
  }
  jq -e . "$sidecar" >/dev/null || {
    echo "migration compatibility sidecar is not valid JSON: $sidecar" >&2
    exit 1
  }

  expected_contract_id="olp.migration.$version_token"
  jq -e --arg contract_id "$expected_contract_id" --arg file "$basename" --argjson version "$version" '
    .schema_version == 1 and
    .contract_id == $contract_id and
    .migration_file == $file and
    .migration_version == $version and
    (.phase == "expand" or .phase == "migrate" or .phase == "contract") and
    (.feature_gate | type == "string" and length > 0) and
    (.n_minus_one | keys | sort == ["control", "gateway", "worker"]) and
    (.rollback_decision | type == "string") and
    (.contract_of | type == "array") and
    (.verification | type == "array" and length > 0 and all(.[]; type == "string" and length > 0)) and
    (.unsafe_sql_exceptions | type == "array")
  ' "$sidecar" >/dev/null || {
    echo "migration compatibility sidecar is incomplete or inconsistent: $sidecar" >&2
    exit 1
  }

  phase=$(jq -r '.phase' "$sidecar")
  feature_gate=$(jq -r '.feature_gate' "$sidecar")
  case "$phase" in
    expand | migrate)
      jq -e '
        .n_minus_one.gateway == "read_write" and
        .n_minus_one.control == "read_write" and
        .n_minus_one.worker == "read_write" and
        .rollback_decision == "binary_rollback_safe" and
        .contract_of == []
      ' "$sidecar" >/dev/null || {
        echo "$phase migration must preserve N-1 read/write and binary rollback: $sidecar" >&2
        exit 1
      }
      ;;
    contract)
      jq -e --argjson version "$version" '
        .n_minus_one.gateway == "unsupported_fail_closed" and
        .n_minus_one.control == "unsupported_fail_closed" and
        .n_minus_one.worker == "unsupported_fail_closed" and
        .rollback_decision == "forward_fix_or_restore" and
        (.contract_of | length > 0 and all(.[];
          type == "number" and floor == . and . > 27 and . < $version)) and
        (.contract_preconditions.feature_enabled_successfully == true) and
        (.contract_preconditions.legacy_only_rows == 0) and
        (.contract_preconditions.n_minus_one_workloads == 0) and
        (.contract_preconditions.legacy_queued_events == 0) and
        (.contract_preconditions.legacy_idempotency_replays == 0) and
        (.contract_preconditions.verification | type == "array" and length > 0 and
          all(.[]; type == "string" and length > 0))
      ' "$sidecar" >/dev/null || {
        echo "contract migration must identify earlier expansions and fail closed for N-1: $sidecar" >&2
        exit 1
      }

      found_expand=false
      while IFS= read -r referenced_version; do
        referenced_token=$(printf '%04d' "$referenced_version")
        mapfile -t referenced_migrations < <(
          find "$migration_dir" -maxdepth 1 -type f -name "${referenced_token}_*.sql" -print
        )
        (( ${#referenced_migrations[@]} == 1 )) || {
          echo "contract migration references a missing or ambiguous migration $referenced_token: $sidecar" >&2
          exit 1
        }
        referenced_stem=${referenced_migrations[0]##*/}
        referenced_stem=${referenced_stem%.sql}
        referenced_sidecar="$fixture_dir/${referenced_stem}.contract.json"
        [[ -f $referenced_sidecar ]] || {
          echo "contract migration references a migration without a sidecar: $referenced_sidecar" >&2
          exit 1
        }
        referenced_phase=$(jq -er '.phase | select(. == "expand" or . == "migrate")' "$referenced_sidecar") || {
          echo "contract_of may reference only expand or migrate phases: $referenced_sidecar" >&2
          exit 1
        }
        jq -e --arg feature_gate "$feature_gate" '.feature_gate == $feature_gate' "$referenced_sidecar" >/dev/null || {
          echo "contract_of migrations must share the contract feature gate: $referenced_sidecar" >&2
          exit 1
        }
        [[ $referenced_phase != expand ]] || found_expand=true
      done < <(jq -r '.contract_of[]' "$sidecar")
      [[ $found_expand == true ]] || {
        echo "contract migration must reference at least one expand migration: $sidecar" >&2
        exit 1
      }
      while IFS= read -r verification; do
        [[ -e $root/$verification ]] || {
          echo "contract precondition evidence does not exist: $verification ($sidecar)" >&2
          exit 1
        }
      done < <(jq -r '.contract_preconditions.verification[]' "$sidecar")
      ;;
  esac

  if [[ $phase == migrate ]]; then
    found_expand=false
    while IFS= read -r candidate; do
      candidate_version=$(jq -er '.migration_version' "$candidate")
      (( candidate_version < version )) || continue
      candidate_phase=$(jq -er '.phase' "$candidate")
      candidate_gate=$(jq -er '.feature_gate' "$candidate")
      if [[ $candidate_phase == expand && $candidate_gate == "$feature_gate" ]]; then
        found_expand=true
        break
      fi
    done < <(find "$fixture_dir" -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]_*.contract.json' -print | LC_ALL=C sort)
    [[ $found_expand == true ]] || {
      echo "migrate phase requires an earlier expand sidecar with the same feature gate: $sidecar" >&2
      exit 1
    }
  fi

  while IFS= read -r verification; do
    [[ -e $root/$verification ]] || {
      echo "migration verification evidence does not exist: $verification ($sidecar)" >&2
      exit 1
    }
  done < <(jq -r '.verification[]' "$sidecar")

  jq -e '
    ([.unsafe_sql_exceptions[].rule_id] | length == (unique | length)) and
    all(.unsafe_sql_exceptions[];
      (.rule_id | type == "string" and length > 0) and
      (.reason | type == "string" and length >= 20) and
      (.approval | type == "string" and test("^XOD-[0-9]+$")))
  ' "$sidecar" >/dev/null || {
    echo "unsafe SQL exceptions require rule_id, a meaningful reason, and XOD approval: $sidecar" >&2
    exit 1
  }

  sanitized_sql=$(sanitize_sql_comments "$migration_file")
  matched_rules=()
  while IFS= read -r rule_id; do
    pattern=$(jq -er --arg id "$rule_id" '.migration_contract.unsafe_sql_rules[] | select(.id == $id) | .pattern' "$contract")
    allowed=$(jq -r --arg id "$rule_id" '.migration_contract.unsafe_sql_rules[] | select(.id == $id) | .allowed_without_exception_in | join(",")' "$contract")
    if ! regex_matches "$rule_id" "$pattern" "$sanitized_sql"; then
      continue
    fi
    matched_rules+=("$rule_id")
    exception_count=$(jq --arg rule "$rule_id" '[.unsafe_sql_exceptions[] | select(.rule_id == $rule)] | length' "$sidecar")
    if [[ ,$allowed, == *,$phase,* ]]; then
      (( exception_count == 0 )) || {
        echo "migration $basename carries an unnecessary exception for allowed rule '$rule_id'" >&2
        exit 1
      }
      continue
    fi
    (( exception_count == 1 )) || {
      echo "migration $basename matches unsafe rule '$rule_id' without one reviewed exception" >&2
      exit 1
    }
  done < <(jq -r '.migration_contract.unsafe_sql_rules[].id' "$contract")

  while IFS= read -r exception_rule; do
    found=false
    for matched_rule in "${matched_rules[@]}"; do
      if [[ $matched_rule == "$exception_rule" ]]; then
        found=true
        break
      fi
    done
    [[ $found == true ]] || {
      echo "migration sidecar contains an unused unsafe SQL exception '$exception_rule': $sidecar" >&2
      exit 1
    }
  done < <(jq -r '.unsafe_sql_exceptions[].rule_id' "$sidecar")
done

(( previous >= cutoff )) || {
  echo "grandfather cutoff $cutoff is newer than the latest migration $previous" >&2
  exit 1
}

grandfathered_hashes=$(jq '.migration_contract.grandfathered_migration_sha256 | length' "$contract")
(( grandfathered_hashes == cutoff )) || {
  echo "expected exactly $cutoff grandfathered migration hashes; found $grandfathered_hashes" >&2
  exit 1
}

if [[ -d $fixture_dir ]]; then
  while IFS= read -r sidecar; do
    stem=${sidecar##*/}
    stem=${stem%.contract.json}
    [[ -f $migration_dir/${stem}.sql ]] || {
      echo "orphan migration compatibility sidecar: $sidecar" >&2
      exit 1
    }
  done < <(find "$fixture_dir" -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]_*.contract.json' -print | LC_ALL=C sort)
fi

echo "migration compatibility contract passed: versions=1-$previous grandfathered=1-$cutoff checked=$((previous - cutoff))"
