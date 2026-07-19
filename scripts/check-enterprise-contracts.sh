#!/usr/bin/env bash
set -euo pipefail

workspace_root="$(cd "$(dirname "$0")/.." && pwd -P)"
cd "$workspace_root"

for command in awk find jq rg sed sha256sum sort tr wc; do
  command -v "$command" >/dev/null || {
    echo "enterprise contract check requires $command" >&2
    exit 2
  }
done

required_files=(
  docs/enterprise/README.md
  docs/enterprise/approvals.md
  docs/enterprise/adr/0001-enterprise-resource-hierarchy.md
  docs/enterprise/adr/0002-request-context-and-runtime-authority.md
  docs/enterprise/adr/0003-extension-trust-boundaries.md
  docs/enterprise/adr/0004-compatibility-and-migrations.md
  docs/enterprise/contracts/resource-ownership.json
  docs/enterprise/contracts/connector-v1.json
  docs/enterprise/contracts/policy-v1.json
  docs/enterprise/contracts/policy/policy-program-v1.schema.json
  docs/enterprise/contracts/policy/policy-v1-golden.json
  docs/enterprise/contracts/compatibility.json
  docs/enterprise/contracts/capacity-envelope.json
  docs/enterprise/contracts/enterprise-beta-scorecard.json
  docs/enterprise/contracts/repository-paths.json
  docs/enterprise/contracts/threat-register.json
  docs/enterprise/contracts/baselines/management-v1.0.0.json
  docs/enterprise/threat-model.md
  proto/olp/connector/v1/connector.proto
  scripts/check-reference-connector-v1.sh
  scripts/run-capacity-profile.sh
  scripts/test-capacity-profile-plan.sh
  scripts/check-openapi-compatibility.sh
  scripts/check-policy-v1-golden.jq
  scripts/openapi-compatibility.jq
  scripts/test-openapi-compatibility.sh
  scripts/normalize-sql-comments.pl
  scripts/test-migration-contract.sh
  scripts/decide-upgrade-recovery.sh
  scripts/test-upgrade-recovery-decision.sh
  tests/migration-fixtures/representative-2x.fixture-manifest.json
  tests/reference-connector-v1/README.md
  tests/reference-connector-v1/main.rs
)

for required_file in "${required_files[@]}"; do
  [[ -f $required_file ]] || {
    echo "required enterprise contract is missing: $required_file" >&2
    exit 1
  }
done

validate_evidence_locator() {
  local locator=$1 context=$2 allow_planned=$3
  local expected_sha=${4:-}
  local evidence_path fragment pointer actual_sha

  if [[ $locator == planned:* ]]; then
    [[ $allow_planned == true ]] || {
      echo "$context uses a planned locator where immutable evidence is required: $locator" >&2
      return 1
    }
    return 0
  fi

  if [[ $locator == https://* ]]; then
    # External evidence is deliberately not fetched in CI. Its digest and
    # accountable review record are mandatory in the scorecard.
    return 0
  fi

  evidence_path=${locator%%#*}
  [[ $evidence_path == "$locator" ]] && fragment= || fragment=${locator#*#}
  [[ -n $evidence_path && $evidence_path != /* && $evidence_path != *".."* ]] || {
    echo "$context has an invalid repository locator: $locator" >&2
    return 1
  }
  [[ -f $evidence_path ]] || {
    echo "$context path is missing: $evidence_path" >&2
    return 1
  }

  if [[ -z $fragment ]]; then
    if [[ -n $expected_sha ]]; then
      read -r actual_sha _ < <(sha256sum "$evidence_path")
      [[ $actual_sha == "$expected_sha" ]] || {
        echo "$context digest does not match repository evidence: $locator" >&2
        return 1
      }
    fi
    return 0
  fi

  [[ $fragment == /* && $evidence_path == *.json ]] || {
    echo "$context must use an RFC 6901 JSON Pointer into a JSON file: $locator" >&2
    return 1
  }
  pointer=${fragment#/}
  jq --exit-status --arg pointer "$pointer" '
    def decode_pointer_token:
      gsub("~1"; "/") | gsub("~0"; "~");
    def resolve_pointer($tokens):
      reduce $tokens[] as $token (.;
        if type == "array" then
          if ($token | test("^(0|[1-9][0-9]*)$")) then .[$token | tonumber]
          else error("invalid array index") end
        elif type == "object" then .[$token]
        else error("pointer traverses a scalar") end);
    ($pointer
      | if length == 0 then [] else split("/") | map(decode_pointer_token) end
    ) as $path
    | resolve_pointer($path) != null
  ' "$evidence_path" >/dev/null || {
    echo "$context JSON Pointer does not resolve: $locator" >&2
    return 1
  }

  if [[ -n $expected_sha ]]; then
    read -r actual_sha _ < <(jq -S -c --arg pointer "$pointer" '
      def decode_pointer_token:
        gsub("~1"; "/") | gsub("~0"; "~");
      def resolve_pointer($tokens):
        reduce $tokens[] as $token (.;
          if type == "array" then
            if ($token | test("^(0|[1-9][0-9]*)$")) then .[$token | tonumber]
            else error("invalid array index") end
          elif type == "object" then .[$token]
          else error("pointer traverses a scalar") end);
      ($pointer
        | if length == 0 then [] else split("/") | map(decode_pointer_token) end
      ) as $path
      | resolve_pointer($path)
    ' "$evidence_path" | sha256sum)
    [[ $actual_sha == "$expected_sha" ]] || {
      echo "$context digest does not match canonical JSON Pointer evidence: $locator" >&2
      return 1
    }
  fi
}

while IFS= read -r -d '' contract; do
  jq --exit-status '
    type == "object"
    and .schema_version == 1
    and .decision_status == "accepted_target"
    and (.approval_status == "approval_pending" or .approval_status == "approved")
    and (.implementation_status | type == "string" and length > 0)
    and (.qualification_status == "not_qualified" or .qualification_status == "not_applicable")
    and (.decision_issue | type == "string" and test("^XOD-[0-9]+$"))
  ' "$contract" >/dev/null || {
    echo "invalid enterprise JSON contract: $contract" >&2
    exit 1
  }
done < <(find docs/enterprise/contracts -maxdepth 1 -type f -name '*.json' -print0 | LC_ALL=C sort -z)

threat_register=docs/enterprise/contracts/threat-register.json
jq --exit-status '
  . as $register
  | .contract_id == "olp.enterprise.threat-register.v1"
  and .decision_issue == "XOD-87"
  and .decision_status == "accepted_target"
  and (.approval_status == "approval_pending" or .approval_status == "approved")
  and .implementation_status == "incomplete"
  and .qualification_status == "not_qualified"
  and .source == "docs/enterprise/threat-model.md"
  and .approval.evidence_source == "docs/enterprise/contracts/enterprise-beta-scorecard.json#/m0_contract_gate/decisions/4"
  and (.threats | type == "array" and length > 0)
  and (.residual_risks | type == "array" and length > 0)
  and ([.threats[].id] | length == (unique | length))
  and ([.residual_risks[].id] | length == (unique | length))
  and all(.threats[];
    (.id | type == "string" and test("^TM-[0-9]{2}$"))
    and .disposition == "mitigate"
    and (.review_gates | type == "array" and length > 0 and length == (unique | length))
    and all(.review_gates[]; test("^EB-[0-9]{2}$")))
  and all(.residual_risks[];
    (.id | type == "string" and test("^AR-[0-9]{2}$"))
    and .approval_status == $register.approval_status
    and (.accountable_authorities | type == "array" and length > 0 and length == (unique | length))
    and all(.accountable_authorities[]; type == "string" and length > 0)
    and (.review_gates | type == "array" and length > 0 and length == (unique | length))
    and all(.review_gates[]; test("^EB-[0-9]{2}$"))
    and (.evidence_requirement_ids | type == "array" and length > 0 and length == (unique | length))
    and all(.evidence_requirement_ids[]; test("^EB-[0-9]{2}-[A-Z]$")))
  and ([.residual_risks[].accountable_authorities[]] | unique | sort)
    == (.approval.required_authorities | unique | sort)
' "$threat_register" >/dev/null || {
  echo "machine-readable threat register is incomplete or overstates approval" >&2
  exit 1
}

declared_threat_ids=$(jq -r '.threats[].id' "$threat_register" | LC_ALL=C sort)
documented_threat_ids=$(rg --only-matching --replace '$1' '^\| (TM-[0-9]{2}) \|' \
  docs/enterprise/threat-model.md | LC_ALL=C sort)
[[ $documented_threat_ids == "$declared_threat_ids" ]] || {
  echo "threat-model TM identifiers disagree with the machine-readable register" >&2
  diff -u <(printf '%s\n' "$declared_threat_ids") <(printf '%s\n' "$documented_threat_ids") >&2 || true
  exit 1
}

declared_risk_ids=$(jq -r '.residual_risks[].id' "$threat_register" | LC_ALL=C sort)
documented_risk_ids=$(rg --only-matching --replace '$1' '^\| (AR-[0-9]{2}) \|' \
  docs/enterprise/threat-model.md | LC_ALL=C sort)
[[ $documented_risk_ids == "$declared_risk_ids" ]] || {
  echo "threat-model AR identifiers disagree with the machine-readable register" >&2
  diff -u <(printf '%s\n' "$declared_risk_ids") <(printf '%s\n' "$documented_risk_ids") >&2 || true
  exit 1
}

# Architecture evidence must describe the packages that Cargo and the boundary
# checker actually enforce. These stale names caused the M0 documentation audit.
if rg -n '`(core|http-api|persistence|connectors/\*)`' docs/architecture.md; then
  echo "docs/architecture.md names a nonexistent production component" >&2
  exit 1
fi
for component in apps/olp crates/domain crates/protocols crates/providers crates/storage; do
  rg -F -q "\`$component\`" docs/architecture.md || {
    echo "docs/architecture.md does not name actual component $component" >&2
    exit 1
  }
done

path_contract=docs/enterprise/contracts/repository-paths.json
jq --exit-status '
  .contract_id == "olp.enterprise.repository-paths.v1"
  and .approval.evidence_source == "docs/enterprise/contracts/enterprise-beta-scorecard.json#/m0_contract_gate/decisions/4"
  and (.paths | type == "array" and length > 0)
  and ([.paths[].path] | length == (unique | length))
  and all(.paths[];
    (.path | type == "string" and length > 0)
    and (.path | startswith("/") | not)
    and (.path | split("/") | index("..") | not)
    and (.kind == "file" or .kind == "directory"))
' "$path_contract" >/dev/null

while IFS=$'\t' read -r repository_path path_kind; do
  case "$path_kind" in
    file)
      [[ -f $repository_path ]] || {
        echo "documented repository file is missing: $repository_path" >&2
        exit 1
      }
      ;;
    directory)
      [[ -d $repository_path ]] || {
        echo "documented repository directory is missing: $repository_path" >&2
        exit 1
      }
      ;;
  esac
done < <(jq -r '.paths[] | [.path, .kind] | @tsv' "$path_contract")

ownership_contract=docs/enterprise/contracts/resource-ownership.json
jq --exit-status '
  .contract_id == "olp.enterprise.resource-ownership.v1"
  and .approval.evidence_source == "docs/enterprise/contracts/enterprise-beta-scorecard.json#/m0_contract_gate/decisions/0"
  and (.migration_inventory.highest_reviewed_migration | type == "string" and test("^[0-9]{4}$"))
  and ((.tables | length) == .migration_inventory.create_table_count)
  and ([.tables[].name] | length == (unique | length))
  and ([.planned_resources[].resource_kind] | length == (unique | length))
  and any(.planned_resources[];
    .resource_kind == "provider_dependency_authority_entry"
    and .authority == "action_scope"
    and .deletion == "credential_revoke"
    and .default_scope_migration == "create_default_provider_grants")
  and (.default_scope_contract as $default
    | $default.uuid_namespace == "persisted installation.id"
    and $default.hierarchy.organization.uuid_v5_name == "olp/default-organization/v1"
    and $default.hierarchy.organization.display_name_source == "installation.organization_name"
    and $default.hierarchy.project.uuid_v5_name == "olp/default-project/v1"
    and $default.hierarchy.project.display_name == "Default"
    and $default.hierarchy.project.normalized_name == "default"
    and $default.hierarchy.environment.uuid_v5_name == "olp/default-environment/v1"
    and $default.hierarchy.environment.display_name == "Default"
    and $default.hierarchy.environment.normalized_name == "default"
    and $default.legacy_role_mapping.role_uuid_namespace == "default organization UUID"
    and $default.legacy_role_mapping.binding_scope
      == "default organization and all of its project/environment descendants"
    and ($default.legacy_role_mapping.roles | keys | sort == ["developer", "operator", "owner", "viewer"])
    and $default.legacy_role_mapping.roles.owner.role_key == "olp.bootstrap.organization.owner.v1"
    and $default.legacy_role_mapping.roles.owner.uuid_v5_name == "olp/bootstrap-role/owner/v1"
    and $default.legacy_role_mapping.roles.owner.permissions == [
      "read_configuration", "manage_providers", "manage_routes", "manage_api_keys",
      "read_team", "manage_team", "manage_sessions", "read_operations",
      "use_playground", "manage_settings", "manage_pricing"
    ]
    and $default.legacy_role_mapping.roles.operator.role_key == "olp.bootstrap.organization.operator.v1"
    and $default.legacy_role_mapping.roles.operator.uuid_v5_name == "olp/bootstrap-role/operator/v1"
    and $default.legacy_role_mapping.roles.operator.permissions == [
      "read_configuration", "manage_providers", "manage_routes", "manage_api_keys",
      "read_team", "read_operations", "use_playground", "manage_settings", "manage_pricing"
    ]
    and $default.legacy_role_mapping.roles.developer.role_key == "olp.bootstrap.organization.developer.v1"
    and $default.legacy_role_mapping.roles.developer.uuid_v5_name == "olp/bootstrap-role/developer/v1"
    and $default.legacy_role_mapping.roles.developer.permissions == [
      "read_configuration", "manage_api_keys", "read_operations", "use_playground"
    ]
    and $default.legacy_role_mapping.roles.viewer.role_key == "olp.bootstrap.organization.viewer.v1"
    and $default.legacy_role_mapping.roles.viewer.uuid_v5_name == "olp/bootstrap-role/viewer/v1"
    and $default.legacy_role_mapping.roles.viewer.permissions == [
      "read_configuration", "read_operations"
    ]
    and $default.legacy_role_mapping.sources == [
      "users.role", "invitations.role", "oidc_configurations.default_role",
      "oidc_email_role_mappings.role", "oidc_group_role_mappings.role"
    ]
    and ($default.legacy_role_mapping.user_state | keys | sort == ["active", "inactive"])
    and ($default.legacy_role_mapping.invitation_state | keys | sort == ["accepted_or_expired", "pending_unexpired"])
    and ($default.settings_key_authority | keys | sort == [
      "retention.audit_days", "retention.requests_days", "retention.usage_days"
    ])
    and all($default.settings_key_authority[];
      .authority == "organization" and .migration_scope == "default organization")
    and $default.unknown_setting_key_behavior
      == "abort migration verification; never guess or copy an unregistered key")
  and (.scope_authorities as $authorities
    | .policy_catalog as $policies
    | all(.tables[];
        ($authorities[.current.authority] != null)
        and (.current.note | type == "string" and length > 0)
        and ($authorities[.target.authority] != null)
        and ($policies.deletion[.target.deletion] != null)
        and ($policies.transfer[.target.transfer] != null)
        and ($policies.retention[.target.retention] != null)
        and ($policies.reference[.target.reference] != null)
        and ($policies.default_scope_migration[.target.default_scope_migration] != null)
        and (.target.scope_columns | type == "array")
        and (.target.uniqueness | type == "array" and length > 0))
    and all(.planned_resources[];
        ($authorities[.authority] != null)
        and (.storage_disposition | type == "string" and length > 0)
        and ($policies.deletion[.deletion] != null)
        and ($policies.transfer[.transfer] != null)
        and ($policies.retention[.retention] != null)
        and ($policies.reference[.reference] != null)
        and ($policies.default_scope_migration[.default_scope_migration] != null)
        and (.scope_columns | type == "array")
        and (.uniqueness | type == "array" and length > 0)))
  and ([
    "policy_set", "policy_program_revision", "policy_binding", "compiled_policy_projection",
    "saml_identity_provider_configuration", "scim_configuration", "scim_credential_revision",
    "scim_resource_version", "approval_policy", "approval_request_and_decision",
    "break_glass_grant", "network_egress_policy", "durable_media_object",
    "audit_or_event_export_checkpoint", "audit_or_event_export_delivery",
    "declarative_document_revision", "declarative_apply_and_drift_observation",
    "support_bundle_request_and_artifact"
  ] - [.planned_resources[].resource_kind] | length == 0)
  and (.contract_invariants | type == "array" and length >= 10)
' "$ownership_contract" >/dev/null || {
  echo "resource ownership contract is incomplete or references an unknown policy" >&2
  exit 1
}

actual_tables=$(rg -o --no-filename --ignore-case \
  '^[[:space:]]*CREATE[[:space:]]+TABLE[[:space:]]+(IF[[:space:]]+NOT[[:space:]]+EXISTS[[:space:]]+)?[a-z_][a-z0-9_]*' \
  crates/storage/migrations --glob '*.sql' \
  | sed -E 's/^[[:space:]]*CREATE[[:space:]]+TABLE[[:space:]]+(IF[[:space:]]+NOT[[:space:]]+EXISTS[[:space:]]+)?//I' \
  | LC_ALL=C sort)
declared_tables=$(jq -r '.tables[].name' "$ownership_contract" | LC_ALL=C sort)
[[ $actual_tables == "$declared_tables" ]] || {
  echo "resource ownership table inventory does not match SQL migrations:" >&2
  diff -u <(printf '%s\n' "$actual_tables") <(printf '%s\n' "$declared_tables") >&2 || true
  exit 1
}

actual_table_count=$(wc -l <<<"$actual_tables" | tr -d '[:space:]')
declared_table_count=$(jq -er '.migration_inventory.create_table_count' "$ownership_contract")
[[ $actual_table_count == "$declared_table_count" ]] || {
  echo "resource ownership table count is stale: actual=$actual_table_count declared=$declared_table_count" >&2
  exit 1
}

mapfile -t reviewed_migrations < <(find crates/storage/migrations -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]_*.sql' -print | LC_ALL=C sort)
(( ${#reviewed_migrations[@]} > 0 )) || {
  echo "resource ownership inventory has no SQL migrations to review" >&2
  exit 1
}
latest_migration=${reviewed_migrations[${#reviewed_migrations[@]} - 1]##*/}
latest_migration=${latest_migration%%_*}
declared_latest_migration=$(jq -er '.migration_inventory.highest_reviewed_migration' "$ownership_contract")
[[ $latest_migration == "$declared_latest_migration" ]] || {
  echo "resource ownership migration inventory is stale: actual=$latest_migration declared=$declared_latest_migration" >&2
  exit 1
}

while IFS=$'\t' read -r table_name introduced_in; do
  migration_path="crates/storage/migrations/$introduced_in"
  [[ -f $migration_path ]] || {
    echo "ownership entry $table_name names a missing migration: $migration_path" >&2
    exit 1
  }
  rg --quiet --ignore-case \
    "^[[:space:]]*CREATE[[:space:]]+TABLE[[:space:]]+(IF[[:space:]]+NOT[[:space:]]+EXISTS[[:space:]]+)?${table_name}([[:space:]]|$)" \
    "$migration_path" || {
      echo "ownership entry $table_name is not created by declared migration $introduced_in" >&2
      exit 1
    }
done < <(jq -r '.tables[] | [.name, .introduced_in] | @tsv' "$ownership_contract")

connector_contract=docs/enterprise/contracts/connector-v1.json
jq --exit-status '
  [
    "type_id",
    "connector_version",
    "artifact_sha256",
    "manifest_sha256",
    "provider_revision_id",
    "provider_grant_id",
    "config_sha256",
    "credential_revision_ids",
    "external_secret_reference_revision_ids",
    "certified_capability_set_sha256",
    "network_policy_id",
    "workload_isolation_policy_id"
  ] as $required_runtime_pins |
  . as $connector |
  .contract_id == "olp.enterprise.connector.v1"
  and .approval.evidence_source == "docs/enterprise/contracts/enterprise-beta-scorecard.json#/m0_contract_gate/decisions/2"
  and .decision_status == "accepted_target"
  and (.approval_status == "approval_pending" or .approval_status == "approved")
  and .implementation_status == "not_implemented"
  and .qualification_status == "not_qualified"
  and .wire_protocol.protocol_major == 1
  and .wire_protocol.encoding == "protobuf"
  and .wire_protocol.rpc == "grpc"
  and .wire_protocol.protobuf_idl.path == "proto/olp/connector/v1/connector.proto"
  and (.wire_protocol.protobuf_idl.sha256 | test("^[0-9a-f]{64}$"))
  and .wire_protocol.protobuf_idl.syntax == "proto3"
  and .wire_protocol.protobuf_idl.package == "olp.connector.v1"
  and .wire_protocol.protobuf_idl.service == "ConnectorService"
  and .wire_protocol.protobuf_idl.methods == ["Handshake", "Configure", "Deconfigure", "DiscoverModels", "CertifyCapability", "CheckHealth", "Execute", "Cancel"]
  and .wire_protocol.message_size_enforcement.maximum_negotiated_message_bytes
    == .resource_bounds.grpc_message_maximum_bytes
  and .wire_protocol.message_size_enforcement.limit_applies_to
    == "serialized_protobuf_message_after_decompression_including_unknown_fields"
  and .wire_protocol.message_size_enforcement.enforced_before_send == true
  and .wire_protocol.message_size_enforcement.enforced_before_application_dispatch == true
  and (.wire_protocol.handshake_negotiation as $handshake
    | $handshake.feature_registry == [
      "FEATURE_SERVER_STREAMING", "FEATURE_REQUEST_MEDIA", "FEATURE_RESPONSE_MEDIA",
      "FEATURE_EXTERNAL_SECRET_REFERENCE", "FEATURE_USAGE_V1"
    ]
    and $handshake.feature_unspecified_allowed == false
    and $handshake.unknown_feature_behavior == "reject_handshake"
    and $handshake.duplicate_feature_behavior == "reject_handshake"
    and $handshake.selected_features_rule
      == "unique_subset_of_HandshakeRequest_supported_features_and_feature_registry"
    and $handshake.required_baseline_selected_features == ["FEATURE_USAGE_V1"]
    and $handshake.feature_gates == {
      "streaming_capability": "FEATURE_SERVER_STREAMING",
      "nonempty_BeginExecution_request_media": "FEATURE_REQUEST_MEDIA",
      "ResponseMediaStart": "FEATURE_RESPONSE_MEDIA",
      "external_secret_reference_revision_deliveries": "FEATURE_EXTERNAL_SECRET_REFERENCE",
      "UsageReport": "FEATURE_USAGE_V1"
    }
    and $handshake.request_maximum_message_bytes.minimum == 1
    and $handshake.request_maximum_message_bytes.maximum
      == $connector.resource_bounds.grpc_message_maximum_bytes
    and $handshake.connector_limit_rule
      == "every_ProtocolLimits_field_is_present_nonzero_and_not_above_its_contract_ceiling"
    and $handshake.effective_limit_rule
      == "validated_connector_advertised_value_without_OLP_increase"
    and $handshake.response_maximum_message_bytes_rule
      == "not_above_HandshakeRequest_maximum_message_bytes_or_contract_ceiling"
    and ($handshake.protocol_limit_ceilings | all(.[]; . > 0))
    and $handshake.protocol_limit_ceilings == {
      "maximum_message_bytes": $connector.resource_bounds.grpc_message_maximum_bytes,
      "maximum_begin_frame_bytes": $connector.execution.maximum_begin_frame_bytes,
      "maximum_unary_result_frame_bytes": $connector.execution.maximum_unary_result_frame_bytes,
      "maximum_stream_event_frame_bytes": $connector.execution.maximum_stream_event_frame_bytes,
      "maximum_media_chunk_frame_bytes": $connector.media.maximum_chunk_frame_bytes,
      "maximum_canonical_output_bytes_per_execution": $connector.execution.maximum_canonical_output_bytes_per_execution,
      "maximum_response_media_bytes_per_execution": $connector.media.response_media_total_bytes_maximum,
      "maximum_events_per_execution": $connector.execution.maximum_events_per_execution,
      "maximum_canonical_request_bytes": $connector.execution.maximum_canonical_request_bytes,
      "maximum_unary_result_bytes": $connector.execution.maximum_unary_result_bytes,
      "maximum_stream_event_bytes": $connector.execution.maximum_stream_event_bytes,
      "maximum_media_chunk_bytes": $connector.media.chunk_maximum_bytes,
      "maximum_request_media_items": $connector.media.request_media_items_maximum,
      "maximum_request_media_bytes_per_execution": $connector.media.request_media_total_bytes_maximum,
      "maximum_response_media_items": $connector.media.response_media_items_maximum,
      "maximum_response_media_item_bytes": $connector.media.response_media_item_bytes_maximum,
      "maximum_request_media_item_bytes": $connector.media.request_media_item_bytes_maximum
    }
    and $handshake.cross_field_rules == [
      "canonical_request_bytes_not_above_begin_frame_bytes_not_above_message_bytes",
      "unary_result_bytes_not_above_unary_result_frame_bytes_not_above_message_bytes",
      "stream_event_bytes_not_above_stream_event_frame_bytes_not_above_message_bytes",
      "media_chunk_bytes_not_above_media_chunk_frame_bytes_not_above_message_bytes",
      "canonical_output_bytes_per_execution_not_below_unary_result_bytes",
      "request_media_bytes_per_execution_not_below_request_media_item_bytes_or_media_chunk_bytes",
      "response_media_bytes_per_execution_not_below_response_media_item_bytes_or_media_chunk_bytes"
    ]
    and $handshake.zero_missing_above_ceiling_or_cross_field_failure_behavior
      == "reject_handshake_without_activation_or_fallback")
  and .wire_protocol.plaintext_tcp_allowed == false
  and .wire_protocol.plaintext_uds_allowed == false
  and .wire_protocol.transports.tcp.mutual_tls_required == true
  and .wire_protocol.transports.unix_domain_socket.mutual_tls_required == true
  and .wire_protocol.transports.unix_domain_socket.peer_credentials_required == true
  and .wire_protocol.transports.unix_domain_socket.socket_mode == "0660"
  and .wire_protocol.workload_identity.required_for_all_transports == true
  and .provider_type_id.pattern == "^(?=.{3,128}$)[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)+$"
  and .provider_type_id.minimum_bytes == 3
  and .provider_type_id.maximum_bytes == 128
  and (.provider_type_id as $provider_type_id
    | $provider_type_id.example | test($provider_type_id.pattern))
  and (["a", "ab", ".a", "a.", "a..b", "A.b", "a_b"]
    | all(.[]; test($connector.provider_type_id.pattern) | not))
  and .wire_value_bounds.measurement == "UTF-8_bytes_before_protobuf_encoding"
  and .wire_value_bounds.validation_point == "before_send_and_before_application_dispatch"
  and .wire_value_bounds.invalid_or_oversized_behavior
    == "reject_frame_as_protocol_error_without_logging_the_value"
  and (.wire_value_bounds.profiles as $profiles
    | ($profiles | length >= 18)
    and all($profiles[];
      (.minimum_bytes | type == "number" and . >= 0)
      and (.maximum_bytes | type == "number" and . > 0)
      and .minimum_bytes <= .maximum_bytes
      and (.empty_allowed | type == "boolean")
      and (.syntax | type == "string" and length > 0))
    and all($connector.wire_value_bounds.protobuf_string_fields[];
      $profiles[.] != null))
  and .wire_value_bounds.profiles.provider_type_id.minimum_bytes
    == .provider_type_id.minimum_bytes
  and .wire_value_bounds.profiles.provider_type_id.maximum_bytes
    == .provider_type_id.maximum_bytes
  and .wire_value_bounds.profiles.secret_slot_name.pattern == .secrets.slot_name_pattern
  and .wire_value_bounds.protobuf_repeated_string_fields == {
    "ConfigureResponse.credential_revision_ids": 32,
    "ConfigureResponse.external_secret_reference_revision_ids": 32
  }
  and .wire_value_bounds.other_repeated_wire_fields == {
    "HandshakeRequest.supported_features": 64,
    "HandshakeResponse.selected_features": 64,
    "ConfigureRequest.credential_revision_deliveries": 32,
    "ConfigureRequest.external_secret_reference_revision_deliveries": 32,
    "BeginExecution.request_media": 32
  }
  and .trust_model.forbidden_extension_models == [
    "rust_cdylib", "rust_dylib", "native_shared_library", "extern_c_provider_abi",
    "libloading", "customer_wasm_in_olp", "customer_script_in_olp",
    "arbitrary_customer_code_in_olp"
  ]
  and (.rpc_operations | map(.name) == ["Handshake", "Configure", "Deconfigure", "DiscoverModels", "CertifyCapability", "CheckHealth", "Execute", "Cancel"])
  and (($required_runtime_pins - .configuration.pinned_values) | length == 0)
  and (($required_runtime_pins - .upgrade_and_rollback.runtime_pin) | length == 0)
  and .secrets.supported_delivery_modes.external_reference.immutable_secret_version_required == true
  and .secrets.supported_delivery_modes.external_reference.mutable_aliases_allowed == false
  and .secrets.retirement.deconfigure_ack_is_zeroization_evidence == false
  and .secrets.retirement.enforcement_after_ack_or_deadline == "revoke_workload_identity_terminate_instance_and_reclaim_memory"
  and (.rpc_operations[] | select(.name == "Deconfigure") | .bounds as $bounds
    | $bounds.drain_ms_maximum == $connector.secrets.retirement.maximum_drain_ms
    and $bounds.emergency_drain_ms == $connector.secrets.retirement.emergency_revocation_drain_ms)
  and (.rpc_operations[] | select(.name == "DiscoverModels") | .bounds as $bounds
    | $bounds.page_limit_minimum == 1
    and $bounds.models_emitted_per_page_maximum == 200
    and $bounds.models_emitted_per_page_maximum <= $bounds.models_emitted_per_discovery_maximum
    and $bounds.models_emitted_per_discovery_maximum == 2000
    and $bounds.pages_requested_per_discovery_maximum
      == ($bounds.models_emitted_per_discovery_maximum + 1)
    and $bounds.items_olp_may_read_across_pages_to_detect_overflow == ($bounds.models_emitted_per_discovery_maximum + 1)
    and $bounds.overflow_item_persisted == false
    and $bounds.pagination.initial_request_cursor == "empty"
    and $bounds.pagination.next_cursor_placement
      == "empty_on_every_nonfinal_response_and_present_only_on_the_final_model_response_when_another_page_exists"
    and $bounds.pagination.empty_page_semantics
      == "stream_ends_without_a_response_and_discovery_is_complete"
    and $bounds.pagination.progress_rule
      == "nonempty_next_cursor_must_differ_from_request_cursor_and_every_prior_cursor_in_this_discovery"
    and $bounds.pagination.cursor_binding == ["configuration_handle", "draft_etag", "manifest_sha256"]
    and $bounds.pagination.cursor_is_opaque_to_olp == true
    and $bounds.pagination.cursor_persisted_or_logged == false
    and $bounds.pagination.cursor_repeat_or_loop_behavior
      == "reject_discovery_without_persisting_partial_inventory"
    and $bounds.pagination.duplicate_model_id_behavior
      == "reject_discovery_without_persisting_partial_inventory")
  and (.manifest.maximum_bytes > 0)
  and (.manifest.configuration_schema_evaluation as $schema
    | $schema.metaschema_uri_must_equal == $connector.manifest.configuration_schema_draft
    and $schema.metaschema_source == "pinned_embedded_copy_no_network_or_filesystem_resolution"
    and $schema.unknown_keywords == "reject"
    and $schema.vocabulary_keyword_allowed_in_connector_schema == false
    and ($schema.implemented_vocabularies | length == 8 and length == (unique | length))
    and ($schema.implemented_vocabularies
      | index("https://json-schema.org/draft/2020-12/vocab/format-assertion") != null)
    and $schema.unsupported_or_custom_vocabulary_behavior == "reject_even_when_declared_optional"
    and $schema.content_keywords_behavior == "annotation_only_never_decode_fetch_or_dereference"
    and $schema.reference_policy.id_keyword_policy
      == "optional_at_root_only_urn_value_no_resolution_or_retrieval_effect"
    and $schema.reference_policy.allowed_ref_form == "same_document_json_pointer_fragment_only"
    and $schema.reference_policy.remote_references_allowed == false
    and $schema.reference_policy.relative_external_references_allowed == false
    and $schema.reference_policy.filesystem_references_allowed == false
    and $schema.reference_policy.anchors_allowed == false
    and $schema.reference_policy.dynamic_ref_allowed == false
    and $schema.reference_policy.dynamic_anchor_allowed == false
    and $schema.reference_policy.recursive_reference_cycles_allowed == false
    and $schema.reference_policy.maximum_reference_edges > 0
    and $schema.reference_policy.maximum_resolved_reference_depth > 0
    and $schema.regex_policy.engine == "RE2_compatible_linear_time"
    and $schema.regex_policy.length_unit == "utf8_bytes"
    and $schema.regex_policy.applies_to_keywords == ["pattern", "patternProperties"]
    and $schema.regex_policy.maximum_patterns > 0
    and $schema.regex_policy.maximum_pattern_bytes_each > 0
    and $schema.regex_policy.maximum_pattern_bytes_total
      >= $schema.regex_policy.maximum_pattern_bytes_each
    and $schema.regex_policy.maximum_compiled_program_instructions_total > 0
    and ($schema.format_policy.allowlist | length > 0 and length == (unique | length))
    and $schema.format_policy.effective_vocabulary
      == "https://json-schema.org/draft/2020-12/vocab/format-assertion"
    and $schema.format_policy.metaschema_format_annotation_is_strengthened_to_assertion == true
    and $schema.compilation_bounds.maximum_schema_syntactic_nesting > 0
    and $schema.compilation_bounds.maximum_schema_syntactic_nesting
      > $connector.configuration.maximum_nesting
    and $schema.compilation_bounds.maximum_schema_nodes > 0
    and $schema.compilation_bounds.maximum_keywords > 0
    and $schema.compilation_bounds.maximum_applicator_branches > 0
    and $schema.compilation_bounds.maximum_compiled_bytes
      > $connector.manifest.field_bounds.config_schema_bytes
    and $schema.compilation_bounds.maximum_wall_time_ms > 0
    and $schema.validation_bounds.maximum_evaluation_steps > 0
    and $schema.validation_bounds.maximum_resolved_reference_visits
      >= $schema.reference_policy.maximum_reference_edges
    and $schema.validation_bounds.maximum_reported_errors > 0
    and $schema.validation_bounds.maximum_wall_time_ms > 0
    and $schema.validation_bounds.unique_items_algorithm
      == "canonical_hash_with_collision_safe_equality")
  and (.configuration.maximum_bytes > 0)
  and .configuration.maximum_total_nodes > 0
  and .configuration.maximum_array_items_total > 0
  and (.execution as $execution
    | $execution.maximum_canonical_request_bytes > 0
    and $execution.maximum_begin_frame_bytes
      == ($execution.maximum_canonical_request_bytes + $execution.begin_frame_envelope_headroom_bytes)
    and $execution.maximum_unary_result_frame_bytes
      == ($execution.maximum_unary_result_bytes + $execution.unary_result_frame_envelope_headroom_bytes)
    and $execution.maximum_stream_event_frame_bytes
      == ($execution.maximum_stream_event_bytes + $execution.stream_event_frame_envelope_headroom_bytes)
    and $execution.maximum_canonical_output_bytes_per_execution
      >= $execution.maximum_unary_result_bytes
    and $execution.maximum_begin_frame_bytes <= $connector.resource_bounds.grpc_message_maximum_bytes
    and $execution.maximum_unary_result_frame_bytes <= $connector.resource_bounds.grpc_message_maximum_bytes
    and $execution.maximum_stream_event_frame_bytes <= $connector.resource_bounds.grpc_message_maximum_bytes
    and $execution.cumulative_output_accounting
      == "sum_raw_canonical_event_and_canonical_result_payload_bytes_before_translation_across_the_execution"
    and ($execution.stream_state_machine.client_states | map(.state))
      == ["await_begin", "input_open", "input_complete", "client_terminal"]
    and ($execution.stream_state_machine.server_states | map(.state))
      == ["await_acceptance", "preaccept_usage_reported", "output_open", "usage_reported", "server_terminal"]
    and ($execution.stream_state_machine.server_states[]
      | select(.state == "await_acceptance")
      | .allowed_frames == ["ExecutionAccepted", "UsageReport", "ExecutionError"]
        and .transition
          == "ExecutionAccepted_to_output_open; UsageReport_to_preaccept_usage_reported; ExecutionError_to_server_terminal")
    and ($execution.stream_state_machine.server_states[]
      | select(.state == "preaccept_usage_reported")
      | .allowed_frames == ["ExecutionError"])
    and ($execution.stream_state_machine.server_states[]
      | select(.state == "output_open")
      | .transition
        == "UsageReport_to_usage_reported; ExecutionError_or_ExecutionDone_to_server_terminal")
    and ($execution.stream_state_machine.server_states[]
      | select(.state == "usage_reported")
      | .allowed_frames == ["ExecutionError", "ExecutionDone"])
    and ($execution.stream_state_machine.rules
      | index("UsageReport_occurs_at_most_once_after_all_output_frames_and_only_a_terminal_frame_may_follow") != null)
    and $execution.terminal_status.allowed_execution_done_statuses == [
      "EXECUTION_TERMINAL_STATUS_SUCCEEDED", "EXECUTION_TERMINAL_STATUS_CANCELLED"
    ]
    and $execution.terminal_status.unspecified_or_unknown_behavior
      == "protocol_error_mark_usage_incomplete_and_discard_uncommitted_output")
  and (.media as $media
    | $media.maximum_chunk_frame_bytes
      == ($media.chunk_maximum_bytes + $media.chunk_frame_envelope_headroom_bytes)
    and $media.maximum_chunk_frame_bytes <= $connector.resource_bounds.grpc_message_maximum_bytes
    and $media.request_media_items_maximum > 0
    and $media.request_media_item_bytes_maximum > 0
    and $media.request_media_item_bytes_maximum <= $media.request_media_total_bytes_maximum
    and $media.request_media_total_bytes_maximum >= $media.chunk_maximum_bytes
    and $media.request_media_accounting
      == "sum_raw_MediaChunk_data_bytes_across_all_request_media_ids_for_the_execution"
    and $media.request_media_overflow_behavior
      == "cancel_stream_before_connector_dispatch_protocol_failure_and_discard_request_media"
    and $media.response_media_items_maximum > 0
    and $media.response_media_total_bytes_maximum >= $media.response_media_item_bytes_maximum
    and $media.response_media_accounting
      == "sum_raw_ResponseMediaChunk_data_bytes_across_all_media_ids_for_the_execution"
    and $media.request_scope_key == ["execution_id", "request", "media_id"]
    and $media.response_scope_key == ["execution_id", "response", "media_id"]
    and $media.cross_direction_media_id_reuse == "reject"
    and $media.response_start_required_fields == [
      "execution_id", "media_id", "declared_content_type",
      "declared_maximum_bytes", "sha256_when_known"
    ]
    and $media.request_lifecycle.initial_sequence == 0
    and $media.request_lifecycle.descriptor_counted_against_item_limit_before_connector_dispatch == true
    and $media.request_lifecycle.declared_maximum_bytes_minimum > 0
    and $media.request_lifecycle.declared_maximum_bytes_maximum
      == $media.request_media_item_bytes_maximum
    and $media.request_lifecycle.declared_maximum_must_fit_remaining_execution_total == true
    and $media.request_lifecycle.declared_capacity_accounting
      == "sum_all_MediaDescriptor_maximum_bytes_must_not_exceed_request_media_total_bytes_maximum"
    and $media.request_lifecycle.zero_byte_media_allowed == true
    and $media.request_lifecycle.terminal_frame == "exactly_one_MediaEnd"
    and $media.response_lifecycle.declaration
      == "exactly_one_ResponseMediaStart_per_fresh_response_media_id"
    and $media.response_lifecycle.start_counted_against_item_limit_before_allocation == true
    and $media.response_lifecycle.declared_maximum_bytes_minimum > 0
    and $media.response_lifecycle.declared_maximum_bytes_maximum
      == $media.response_media_item_bytes_maximum
    and $media.response_lifecycle.declared_maximum_must_fit_remaining_execution_total == true
    and $media.response_lifecycle.declared_capacity_accounting
      == "sum_all_ResponseMediaStart_declared_maximum_bytes_must_not_exceed_response_media_total_bytes_maximum"
    and $media.response_lifecycle.initial_sequence == 0
    and $media.response_lifecycle.sequence_rule == "strictly_contiguous_per_media_id"
    and $media.response_lifecycle.terminal_frame == "exactly_one_ResponseMediaEnd"
    and $media.response_lifecycle.chunk_or_end_before_start_or_frame_after_end == "protocol_error")
  and (.rpc_operations[] | select(.name == "Execute")
    | .server_frames == [
      "ExecutionAccepted", "CanonicalEvent", "CanonicalResult",
      "ResponseMediaStart", "ResponseMediaChunk", "ResponseMediaEnd",
      "UsageReport", "ExecutionError", "ExecutionDone"
    ])
  and (.execution.stream_state_machine.rules
    | index("response_media_ids_are_fresh_and_declared_only_by_exactly_one_ResponseMediaStart") != null)
  and (.execution.stream_state_machine.rules
    | index("request_and_response_media_identifier_namespaces_are_distinct_and_cross_direction_reuse_is_rejected") != null)
  and .health.freshness_clock == "OLP_monotonic_clock"
  and .health.freshness_age_start == "OLP_monotonic_completion_of_last_valid_CheckHealth_response"
  and .health.connector_observed_at_unix_ms_is_freshness_authority == false
  and .health.invalid_observed_at_behavior == "discard_timestamp_without_extending_health_eligibility"
  and .health.retry_after_ms_minimum == 0
  and .health.retry_after_ms_maximum <= .health.failure_grace_ms
  and .health.retry_after_ms_behavior == "clamp_to_contract_and_never_extend_stale_health_deadline"
  and .health.unspecified_or_unknown_status_behavior == "reject_health_response_as_protocol_error"
  and (.rpc_operations[] | select(.name == "CheckHealth")
    | .allowed_statuses == ["ready", "degraded", "unavailable"])
  and .usage.silent_zero_allowed == false
  and .usage.terminal_report_count_minimum == 0
  and .usage.terminal_report_count_maximum == 1
  and .usage.successful_execution_expected_report_count == 1
  and .usage.missing_terminal_report_is_protocol_error == false
  and .reference_connector_gate.must_import_only_published_connector_sdk == true
  and .reference_connector_gate.may_depend_on_olp_domain_crate == false
  and .reference_connector_gate.requires_new_ProviderKind_variant == false
  and .reference_connector_gate.requires_olp_rebuild == false
  and (.reference_connector_gate.m0_contract_build_proof as $proof
    | $proof.status == "contract_build_proof_only"
    and $proof.source == "tests/reference-connector-v1/main.rs"
    and ($proof.source_sha256 | test("^[0-9a-f]{64}$"))
    and $proof.checker == "scripts/check-reference-connector-v1.sh"
    and $proof.language == "standalone_rustc_edition_2021"
    and $proof.dependencies == "rust_standard_library_only"
    and $proof.type_id == "com.example.external-fixture"
    and ($proof.type_id | test($connector.provider_type_id.pattern))
    and $proof.proto_sha256 == $connector.wire_protocol.protobuf_idl.sha256
    and $proof.rpc_methods == $connector.wire_protocol.protobuf_idl.methods
    and $proof.imports_olp_domain == false
    and $proof.uses_ProviderKind == false
    and $proof.requires_olp_rebuild == false
    and $proof.published_sdk_used == false
    and $proof.published_sdk_available == false
    and $proof.grpc_transport_implemented == false
    and $proof.runtime_integration_implemented == false
    and $proof.satisfies_M4_reference_connector_gate == false
    and $proof.qualification_status == "not_qualified")
' "$connector_contract" >/dev/null || {
  echo "connector v1 trust or bounds contract is incomplete" >&2
  exit 1
}
while IFS= read -r connector_anchor; do
  [[ -e $connector_anchor ]] || {
    echo "connector contract code anchor is missing: $connector_anchor" >&2
    exit 1
  }
done < <(jq -r '.current_implementation.anchors[]' "$connector_contract")

connector_idl=$(jq -er '.wire_protocol.protobuf_idl.path' "$connector_contract")
declared_connector_idl_sha256=$(jq -er '.wire_protocol.protobuf_idl.sha256' "$connector_contract")
actual_connector_idl_sha256=$(sha256sum "$connector_idl" | sed -E 's/[[:space:]].*$//')
[[ $actual_connector_idl_sha256 == "$declared_connector_idl_sha256" ]] || {
  echo "connector protobuf IDL digest is stale: actual=$actual_connector_idl_sha256 declared=$declared_connector_idl_sha256" >&2
  exit 1
}
rg --quiet '^syntax = "proto3";$' "$connector_idl"
rg --quiet '^package olp\.connector\.v1;$' "$connector_idl"
rg --quiet '^service ConnectorService \{$' "$connector_idl"
actual_connector_methods=$(sed -nE 's/^[[:space:]]*rpc[[:space:]]+([A-Za-z][A-Za-z0-9]*)\(.*/\1/p' "$connector_idl")
declared_connector_methods=$(jq -r '.wire_protocol.protobuf_idl.methods[]' "$connector_contract")
[[ $actual_connector_methods == "$declared_connector_methods" ]] || {
  echo "connector protobuf RPC methods disagree with the machine contract" >&2
  diff -u <(printf '%s\n' "$declared_connector_methods") <(printf '%s\n' "$actual_connector_methods") >&2 || true
  exit 1
}
[[ $(rg --count '^[[:space:]]+oneof frame \{$' "$connector_idl") == 2 ]] || {
  echo "connector protobuf must define exactly one client and one server frame envelope" >&2
  exit 1
}
actual_connector_features=$(awk '
  /^enum Feature \{/ { in_feature=1; next }
  in_feature && /^}/ { in_feature=0; next }
  in_feature && $1 != "FEATURE_UNSPECIFIED" { print $1 }
' "$connector_idl")
declared_connector_features=$(jq -r '.wire_protocol.handshake_negotiation.feature_registry[]' "$connector_contract")
[[ $actual_connector_features == "$declared_connector_features" ]] || {
  echo "connector feature enum and handshake registry disagree" >&2
  diff -u <(printf '%s\n' "$declared_connector_features") <(printf '%s\n' "$actual_connector_features") >&2 || true
  exit 1
}
actual_connector_limit_fields=$(awk '
  /^message ProtocolLimits \{/ { in_limits=1; next }
  in_limits && /^}/ { in_limits=0; next }
  in_limits { print $2 }
' "$connector_idl" | LC_ALL=C sort)
declared_connector_limit_fields=$(jq -r '.wire_protocol.handshake_negotiation.protocol_limit_ceilings | keys[]' "$connector_contract" | LC_ALL=C sort)
[[ $actual_connector_limit_fields == "$declared_connector_limit_fields" ]] || {
  echo "every ProtocolLimits field must have exactly one contract ceiling" >&2
  diff -u <(printf '%s\n' "$declared_connector_limit_fields") <(printf '%s\n' "$actual_connector_limit_fields") >&2 || true
  exit 1
}
actual_connector_terminal_statuses=$(awk '
  /^enum ExecutionTerminalStatus \{/ { in_status=1; next }
  in_status && /^}/ { in_status=0; next }
  in_status && $1 != "EXECUTION_TERMINAL_STATUS_UNSPECIFIED" { print $1 }
' "$connector_idl")
declared_connector_terminal_statuses=$(jq -r '.execution.terminal_status.allowed_execution_done_statuses[]' "$connector_contract")
[[ $actual_connector_terminal_statuses == "$declared_connector_terminal_statuses" ]] || {
  echo "connector terminal status enum and execution contract disagree" >&2
  diff -u <(printf '%s\n' "$declared_connector_terminal_statuses") <(printf '%s\n' "$actual_connector_terminal_statuses") >&2 || true
  exit 1
}
actual_connector_string_fields=$(awk '
  /^message[[:space:]]+/ { message_name=$2; next }
  message_name != "" && /^}/ { message_name=""; next }
  message_name != "" && $1 == "string" { print message_name "." $2 }
  message_name != "" && $1 == "optional" && $2 == "string" { print message_name "." $3 }
  message_name != "" && $1 == "repeated" && $2 == "string" { print message_name "." $3 }
' "$connector_idl" | LC_ALL=C sort)
declared_connector_string_fields=$(jq -r '.wire_value_bounds.protobuf_string_fields | keys[]' "$connector_contract" | LC_ALL=C sort)
[[ $actual_connector_string_fields == "$declared_connector_string_fields" ]] || {
  echo "every connector protobuf string field must have exactly one declared bound profile" >&2
  diff -u <(printf '%s\n' "$declared_connector_string_fields") <(printf '%s\n' "$actual_connector_string_fields") >&2 || true
  exit 1
}
actual_connector_repeated_string_fields=$(awk '
  /^message[[:space:]]+/ { message_name=$2; next }
  message_name != "" && /^}/ { message_name=""; next }
  message_name != "" && $1 == "repeated" && $2 == "string" { print message_name "." $3 }
' "$connector_idl" | LC_ALL=C sort)
declared_connector_repeated_string_fields=$(jq -r '.wire_value_bounds.protobuf_repeated_string_fields | keys[]' "$connector_contract" | LC_ALL=C sort)
[[ $actual_connector_repeated_string_fields == "$declared_connector_repeated_string_fields" ]] || {
  echo "every repeated connector protobuf string field must have an explicit item count" >&2
  diff -u <(printf '%s\n' "$declared_connector_repeated_string_fields") <(printf '%s\n' "$actual_connector_repeated_string_fields") >&2 || true
  exit 1
}
actual_connector_repeated_fields=$(awk '
  /^message[[:space:]]+/ { message_name=$2; next }
  message_name != "" && /^}/ { message_name=""; next }
  message_name != "" && $1 == "repeated" { print message_name "." $3 }
' "$connector_idl" | LC_ALL=C sort)
declared_connector_repeated_fields=$(jq -r '[
  (.wire_value_bounds.protobuf_repeated_string_fields | keys[]),
  (.wire_value_bounds.other_repeated_wire_fields | keys[])
] | unique[]' "$connector_contract" | LC_ALL=C sort)
[[ $actual_connector_repeated_fields == "$declared_connector_repeated_fields" ]] || {
  echo "every repeated connector protobuf field must have an explicit item count" >&2
  diff -u <(printf '%s\n' "$declared_connector_repeated_fields") <(printf '%s\n' "$actual_connector_repeated_fields") >&2 || true
  exit 1
}

if [[ -x scripts/check-reference-connector-v1.sh ]]; then
  scripts/check-reference-connector-v1.sh
else
  echo "reference connector contract/build checker is missing or not executable" >&2
  exit 1
fi

capacity_contract=docs/enterprise/contracts/capacity-envelope.json
jq --exit-status '
  .contract_id == "olp.enterprise.capacity-envelope.v1"
  and .approval.evidence_source == "docs/enterprise/contracts/enterprise-beta-scorecard.json#/m0_contract_gate/decisions/4"
  and .decision_status == "accepted_target"
  and (.approval_status == "approval_pending" or .approval_status == "approved")
  and .implementation_status == "not_implemented"
  and .qualification_status == "not_qualified"
  and .reference_topology.active_active_regions == false
  and .reference_topology.postgresql.synchronous_commit == "remote_apply"
  and .reference_topology.postgresql.synchronous_standby_quorum == "ANY_1_OF_2"
  and .reference_topology.postgresql.failover_candidate_requirement
    == "replayed_at_or_beyond_last_acknowledged_commit_LSN"
  and .reference_topology.postgresql.ack_timeout_behavior == "fail_mutation_without_acknowledgement"
  and .reference_topology.valkey.persistent_storage_required == true
  and .reference_topology.valkey.acknowledged_usage_fsync == "primary_and_both_replicas_before_acknowledgement"
  and .reference_topology.valkey.acknowledgement_mechanism == "WAITAOF_or_tested_equivalent_durable_quorum_protocol"
  and .reference_topology.valkey.failover_candidate_minimum_offset == "at_or_after_last_acknowledged_usage_offset"
  and .beta_targets.scope_cardinality.organizations_per_installation > 0
  and .beta_targets.scope_cardinality.environments_per_installation > 0
  and .beta_targets.provider_cardinality.provider_connections_per_installation > 0
  and .beta_targets.routing_and_policy_cardinality.routes_per_environment > 0
  and .beta_targets.routing_and_policy_cardinality.compiled_policy_bytes_per_environment_maximum > 0
  and .beta_targets.routing_and_policy_cardinality.compiled_policy_bytes_per_environment_maximum
    <= .beta_targets.runtime_cache.environment_runtime_payload_bytes_maximum
  and .beta_targets.routing_and_policy_cardinality.compiled_policy_deduplication_key == "compiled_program_sha256"
  and .beta_targets.credential_cardinality.active_inference_credentials_per_installation > 0
  and .beta_targets.traffic.concurrent_streams_fleet > 0
  and .beta_targets.traffic.sustained_unary_requests_per_second_fleet > 0
  and ([
    .beta_targets.traffic.maximum_json_request_bytes,
    .beta_targets.traffic.maximum_media_request_bytes,
    .beta_targets.traffic.maximum_stream_event_bytes,
    .beta_targets.traffic.maximum_canonical_output_bytes_per_execution,
    .beta_targets.traffic.maximum_response_media_bytes_per_execution,
    .beta_targets.traffic.maximum_connector_begin_frame_bytes,
    .beta_targets.traffic.maximum_connector_unary_result_frame_bytes,
    .beta_targets.traffic.maximum_connector_stream_event_frame_bytes,
    .beta_targets.traffic.maximum_connector_grpc_message_bytes
  ] | all(.[]; . > 0))
  and .beta_targets.durable_work.usage_stream_backlog_events > 0
  and .slo_targets.gateway_successful_availability_percent == 99.9
  and (.slo_targets as $slo
    | ($slo.measurement_window_days * 24 * 60
       * (100 - $slo.gateway_successful_availability_percent) / 100) as $calculated_budget
    | $slo.monthly_error_budget_minutes_at_99_9_percent >= ($calculated_budget - 0.000001)
    and $slo.monthly_error_budget_minutes_at_99_9_percent <= ($calculated_budget + 0.000001))
  and .slo_targets.cross_scope_security_incidents_allowed == 0
  and .slo_targets.acknowledged_usage_events_lost_allowed == 0
  and .slo_targets.silent_zero_usage_or_price_events_allowed == 0
  and .propagation_targets.admission_authority_freshness.authorities == [
    "credential_and_provider_dependency_security_authority",
    "environment_release_head"
  ]
  and .propagation_targets.admission_authority_freshness.age_start
    == "completion_of_last_successful_authoritative_postgresql_read_and_validation_of_current_head"
  and .propagation_targets.admission_authority_freshness.maximum_age_ms == 6000
  and .propagation_targets.admission_authority_freshness.beyond_maximum
    == "deny_new_public_authentication_before_runtime_selection"
  and .propagation_targets.admission_authority_freshness.cache_miss_during_authority_unavailability == "deny"
  and .propagation_targets.admission_authority_freshness.readiness_requires_within_maximum == true
  and .propagation_targets.provider_dependency_invalidation.healthy_hint_p99_ms == 2000
  and .propagation_targets.provider_dependency_invalidation.missed_hint_hard_maximum_ms == 6000
  and .propagation_targets.provider_dependency_invalidation.waits_for_environment_republication == false
  and .propagation_targets.provider_dependency_invalidation.inflight_committed_request_terminated == false
  and .propagation_targets.environment_activation.unrelated_environment_rebuild_allowed == false
  and .propagation_targets.environment_cold_load.wrong_environment_fallback_allowed == false
  and .recovery_targets.acknowledged_usage.rpo_seconds == 0
  and .recovery_targets.valkey_primary_failover.rpo_seconds == 0
  and .recovery_targets.valkey_primary_failover.required_mode == "supported_HA_AOF_durable_quorum_ack_reference_topology"
  and .recovery_targets.valkey_primary_failover.acknowledged_usage_loss_allowed == 0
  and .recovery_targets.durable_media_job.rpo_seconds == 0
  and .profile_runner_contract.path == "scripts/run-capacity-profile.sh"
  and .profile_runner_contract.present_at_m0 == true
  and .profile_runner_contract.mode_at_m0 == "deterministic_plan_renderer_only"
  and .profile_runner_contract.execute_available_at_m0 == false
  and (.profile_runner_contract.execute_unavailable_until | type == "string" and length > 0)
  and .profile_runner_contract.plan_renderer_generates_load == false
  and .profile_runner_contract.plan_renderer_mutates_state == false
  and .profile_runner_contract.plan_output_is_qualification_evidence == false
  and .profile_runner_contract.contract_input_snapshot == "single_validated_mktemp_copy"
  and .profile_runner_contract.source_change_before_output_completion == "fail_closed"
  and .profile_runner_contract.commands_at_m0_are_normative_invocation_specs_not_execution_evidence == true
  and .profile_runner_contract.unresolved_placeholder_behavior == "reject_before_mutating_or_generating_load"
  and .profile_runner_contract.plan_input_bounds == {
    "seed": {"minimum": 0, "maximum": 4294967295},
    "runs": {"minimum": 1, "maximum": 1000},
    "duration_seconds": {"minimum": 1, "maximum": 2592000},
    "gateway_replicas": {"minimum": 1, "maximum": 1000}
  }
  and (.profile_runner_contract.output_at_m0
    | contains("never an execution or qualification evidence record"))
  and (.profile_runner_contract.future_execution_output | type == "string" and length > 0)
  and (.profiles.scope_cardinality.load.read_mix_percent | [.[]] | add == 100)
  and (.profiles.scope_cardinality.load.mutation_mix_percent | [.[]] | add == 100)
  and (.profiles.gateway_unary.load.operation_mix_percent | [.[]] | add == 100)
  and (.profiles.gateway_unary.load.canonical_request_payload_mix_percent | [.[]] | add == 100)
  and (.profiles.gateway_unary.load.credential_state_mix_percent | [.[]] | add == 100)
  and (.profiles.gateway_unary.load.environment_runtime_cache_hit_percent
    + .profiles.gateway_unary.load.environment_runtime_cold_load_percent == 100)
  and (.profiles.gateway_unary.load.cache_churn_schedule.resident_hot_environments_per_gateway
    + .profiles.gateway_unary.load.cache_churn_schedule.rotating_cold_environments_per_gateway
    == .profiles.gateway_unary.load.active_environments)
  and (.profiles.gateway_unary.load.cache_churn_schedule.hot_requests_per_period
    + .profiles.gateway_unary.load.cache_churn_schedule.cold_requests_per_period
    == .profiles.gateway_unary.load.cache_churn_schedule.request_trace_period)
  and (.profiles.gateway_unary.load.cache_churn_schedule.resident_hot_environments_per_gateway
    == .profiles.gateway_unary.load.cache_churn_schedule.hot_requests_per_period)
  and (.beta_targets.runtime_cache.loaded_environment_runtimes_per_gateway
    > .profiles.gateway_unary.load.cache_churn_schedule.resident_hot_environments_per_gateway)
  and (.beta_targets.runtime_cache.loaded_environment_runtimes_per_gateway
    < .profiles.gateway_unary.load.active_environments)
  and (100 * .profiles.gateway_unary.load.cache_churn_schedule.cold_requests_per_period
    == .profiles.gateway_unary.load.environment_runtime_cold_load_percent
      * .profiles.gateway_unary.load.cache_churn_schedule.request_trace_period)
  and .profiles.gateway_unary.load.requests_per_second_fleet
    == .beta_targets.traffic.sustained_unary_requests_per_second_fleet
  and .profiles.gateway_streaming.load.concurrent_streams_fleet
    == .beta_targets.traffic.concurrent_streams_fleet
  and (.profiles.gateway_streaming.load.concurrent_streams_fleet * 1000
    / .profiles.gateway_streaming.load.event_interval_ms
    == .profiles.gateway_streaming.load.events_per_second_fleet)
  and (.profiles.gateway_streaming.load.events_per_stream
    * .profiles.gateway_streaming.load.event_interval_ms
    == .profiles.gateway_streaming.load.stream_lifetime_ms)
  and .profiles.event_backlog.load.backlog_events_at_worker_resume
    == .beta_targets.durable_work.usage_stream_backlog_events
  and .profiles.recovery.placeholder_bindings["BACKUP.dump"] != null
  and .profiles.recovery.placeholder_bindings.CANDIDATE_BINARY != null
  and (.profiles.recovery.future_execution_commands | type == "array" and length == 3)
  and (.profiles.recovery.future_execution_commands
    | all(.[]; type == "string" and length > 0))
  and .profiles.scope_cardinality.command
    == "scripts/run-capacity-profile.sh scope-cardinality --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719"
  and .profiles.configuration_compile.command
    == "scripts/run-capacity-profile.sh configuration-compile --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719"
  and .profiles.gateway_unary.command
    == "scripts/run-capacity-profile.sh gateway-unary --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --runs 3"
  and .profiles.gateway_streaming.command
    == "scripts/run-capacity-profile.sh gateway-streaming --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --runs 3"
  and .profiles.event_backlog.command
    == "scripts/run-capacity-profile.sh event-backlog --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719"
  and .profiles.runtime_convergence.command
    == "scripts/run-capacity-profile.sh runtime-convergence --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --gateway-replicas 3"
  and .profiles.recovery.command
    == "scripts/run-capacity-profile.sh disaster-recovery --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719"
  and .profiles.slo_soak.command
    == "scripts/run-capacity-profile.sh slo-soak --contract docs/enterprise/contracts/capacity-envelope.json --seed 20260719 --duration 24h"
  and (.profiles | to_entries | map(.value.profile_id) == ["CP-01", "CP-02", "CP-03", "CP-04", "CP-05", "CP-06", "CP-07", "CP-08"])
  and all(.profiles[];
    (.implementation_status | startswith("planned_"))
    and .qualification_status == "not_qualified"
    and (.assertions | type == "array" and length > 0)
    and (.command | type == "string" and length > 0))
  and .release_rule.all_profiles_must_pass == ["CP-01", "CP-02", "CP-03", "CP-04", "CP-05", "CP-06", "CP-07", "CP-08"]
  and .release_rule.waiver_allowed == false
  and (.evidence_record_schema.required_fields | index("capacity_contract_sha256") != null)
  and (.evidence_record_schema.required_fields | index("source_commit_sha") != null)
  and (.evidence_record_schema.forbidden_evidence | index("unreviewed_local_summary") != null)
' "$capacity_contract" >/dev/null || {
  echo "capacity, SLO, propagation, recovery, or evidence contract is incomplete" >&2
  exit 1
}

policy_contract=docs/enterprise/contracts/policy-v1.json
policy_schema=docs/enterprise/contracts/policy/policy-program-v1.schema.json
policy_golden=docs/enterprise/contracts/policy/policy-v1-golden.json
policy_golden_checker=scripts/check-policy-v1-golden.jq
jq --exit-status \
  --slurpfile schema "$policy_schema" \
  --slurpfile golden "$policy_golden" \
  --slurpfile capacity "$capacity_contract" '
  def parameter_is_valid($policy; $action):
    ([$policy.action_semantics.parameter_groups
      | to_entries[]
      | select(.value | index($action.kind) != null)
      | .key]) as $groups
  | (["name", "integer_value", "field_path", "candidate_ids"]
    | map(. as $key | select($action | has($key)))) as $present
    | ($groups | length) == 1
    and (if $groups[0] == "none" then ($present | length) == 0
      else $present == [$groups[0]] end);
  . as $policy
  | $schema[0] as $schema
  | $golden[0] as $golden
  | $capacity[0].beta_targets.routing_and_policy_cardinality as $capacity
  | ([.phases[].actions[]] | unique | sort) as $phase_actions
  | ([.action_semantics.parameter_groups[][]] | unique | sort) as $semantic_actions
  | .contract_id == "olp.enterprise.policy.v1"
  and .approval.evidence_source
    == "docs/enterprise/contracts/enterprise-beta-scorecard.json#/m0_contract_gate/decisions/2"
  and .identity.language_version == "1.0.0"
  and .phase_order == ["credential", "admission", "request_guard", "route", "attempt_result", "reconcile"]
  and (.phases | keys | sort == ($policy.phase_order | sort))
  and .operators.closed_set == ["const", "not", "all", "any", "compare", "present", "in"]
  and .operators.regex_glob_substring_and_customer_functions == "forbidden_in_v1"
  and .evaluation.loops_recursion_network_filesystem_customer_code == "forbidden"
  and .execution_boundary.program_representation == "validated_declarative_data_only"
  and .execution_boundary.execution_authority == "trusted_OLP_policy_evaluator_only"
  and .execution_boundary.customer_provided_executable_payload_allowed == false
  and .execution_boundary.dynamic_code_loading_allowed == false
  and .execution_boundary.forbidden_models == [
    "native_shared_library", "extern_c_abi", "libloading", "customer_wasm",
    "customer_javascript_or_script", "general_purpose_bytecode",
    "network_or_filesystem_extension", "runtime_code_generation"
  ]
  and .evaluation.hard_failure == "deny_or_reject_activation_without_side_effect"
  and .binding_composition.narrower_scope_may_weaken_ancestor_hard_control == false
  and .lifecycle.active_revision_mutation == "forbidden"
  and ($phase_actions == $semantic_actions)
  and ($schema."$defs".action.properties.kind.enum | unique | sort) == $phase_actions
  and ($schema.properties.phase.enum == .phase_order)
  and $schema.additionalProperties == false
  and ([$schema | .. | objects | keys[] | ascii_downcase]
    | any(.[];
      . == "code" or . == "wasm" or . == "script" or . == "module"
      or . == "bytecode" or . == "library" or . == "abi"
      or . == "executable" or . == "source")
    | not)
  and $schema.properties.language_version.const == .identity.language_version
  and ($schema."$defs".operand.oneOf[]
    | select(.properties.decimal != null)
    | .properties.decimal.pattern)
    == "^(?!-0[.]000000000$)-?(?:0|[1-9][0-9]{0,19})[.][0-9]{9}$"
  and $golden.language_version == .identity.language_version
  and $golden.negative_vectors == [{
    "id": "decimal-negative-zero-is-not-canonical",
    "schema_path": "#/$defs/operand/oneOf/2/properties/decimal",
    "value": "-0.000000000",
    "expected": "schema_rejection"
  }]
  and all($golden.vectors[];
    .program as $program
    | ($policy.phases[$program.phase] != null)
    and $program.schema_version == 1
    and $program.language_version == $policy.identity.language_version
    and ([ $program.rules[].actions[], $program.default_actions[] ]) as $actions
    | all($actions[];
        . as $action
        | ($policy.phases[$program.phase].actions | index($action.kind) != null)
        and ($action.enforcement == "hard" or $action.enforcement == "advisory")
        and (if $action.enforcement == "advisory"
          then ($policy.action_semantics.advisory_allowed_actions | index($action.kind) != null)
          else true end)
        and parameter_is_valid($policy; $action)))
  and .bounds.policy_programs_per_organization == $capacity.policy_programs_per_organization
  and .bounds.policy_bindings_per_environment == $capacity.policy_bindings_per_environment
  and .bounds.compiled_policy_bytes_per_program == $capacity.compiled_policy_bytes_per_program
  and .bounds.compiled_policy_bytes_per_environment_maximum
    == $capacity.compiled_policy_bytes_per_environment_maximum
  and .bounds.compiled_policy_deduplication_key == $capacity.compiled_policy_deduplication_key
  and .bounds.policy_nodes_per_program == $capacity.policy_nodes_per_program
  and .bounds.policy_evaluation_steps_per_phase == $capacity.policy_evaluation_steps_per_phase
' "$policy_contract" >/dev/null || {
  echo "policy-v1 contract, schema, golden vectors, or capacity alignment is incomplete" >&2
  exit 1
}

jq --exit-status \
  --slurpfile policy "$policy_contract" \
  --slurpfile schema "$policy_schema" \
  --from-file "$policy_golden_checker" \
  "$policy_golden" >/dev/null || {
  echo "policy-v1 golden programs do not validate or replay to their expected values" >&2
  exit 1
}

while IFS=$'\t' read -r artifact_path expected_sha; do
  validate_evidence_locator "$artifact_path" "policy-v1 pinned artifact" false "$expected_sha"
done < <(jq -r '.artifacts[] | [.path, .sha256] | @tsv' "$policy_contract")

connector_model_maximum=$(jq -er '
  .rpc_operations[]
  | select(.name == "DiscoverModels")
  | .bounds.models_emitted_per_discovery_maximum
' "$connector_contract")
capacity_model_maximum=$(jq -er '.beta_targets.provider_cardinality.models_per_provider_revision' "$capacity_contract")
[[ $connector_model_maximum == "$capacity_model_maximum" ]] || {
  echo "connector discovery and capacity model maxima disagree: connector=$connector_model_maximum capacity=$capacity_model_maximum" >&2
  exit 1
}
connector_traffic_limits=$(jq -r '[
  .execution.maximum_canonical_request_bytes,
  .media.request_media_total_bytes_maximum,
  .execution.maximum_stream_event_bytes,
  .execution.maximum_canonical_output_bytes_per_execution,
  .media.response_media_total_bytes_maximum,
  .execution.maximum_begin_frame_bytes,
  .execution.maximum_unary_result_frame_bytes,
  .execution.maximum_stream_event_frame_bytes,
  .resource_bounds.grpc_message_maximum_bytes
] | @tsv' "$connector_contract")
capacity_traffic_limits=$(jq -r '[
  .beta_targets.traffic.maximum_json_request_bytes,
  .beta_targets.traffic.maximum_media_request_bytes,
  .beta_targets.traffic.maximum_stream_event_bytes,
  .beta_targets.traffic.maximum_canonical_output_bytes_per_execution,
  .beta_targets.traffic.maximum_response_media_bytes_per_execution,
  .beta_targets.traffic.maximum_connector_begin_frame_bytes,
  .beta_targets.traffic.maximum_connector_unary_result_frame_bytes,
  .beta_targets.traffic.maximum_connector_stream_event_frame_bytes,
  .beta_targets.traffic.maximum_connector_grpc_message_bytes
] | @tsv' "$capacity_contract")
[[ $connector_traffic_limits == "$capacity_traffic_limits" ]] || {
  echo "connector execution and capacity traffic maxima disagree" >&2
  exit 1
}
while IFS= read -r capacity_anchor; do
  [[ -e $capacity_anchor ]] || {
    echo "capacity contract code anchor is missing: $capacity_anchor" >&2
    exit 1
  }
done < <(jq -r '.current_2_0_baseline.anchors[]' "$capacity_contract")

compatibility_contract=docs/enterprise/contracts/compatibility.json
jq --exit-status '
  .contract_id == "olp.enterprise.compatibility.v1"
  and .approval.evidence_source == "docs/enterprise/contracts/enterprise-beta-scorecard.json#/m0_contract_gate/decisions/3"
  and .decision_status == "accepted_target"
  and (.approval_status == "approval_pending" or .approval_status == "approved")
  and .qualification_status == "not_qualified"
  and .migration_contract.grandfathered_through == 27
  and (.migration_contract.phases | keys | sort == ["contract", "expand", "migrate"])
  and .mixed_version_matrix.expand.gateway_N_control_N_worker_N == "supported"
  and .mixed_version_matrix.expand["gateway_N-1_control_N_worker_N"] == "supported_default_scope_only"
  and .mixed_version_matrix.migrate["gateway_N_control_N_worker_N-1"] == "supported_default_scope_only"
  and .mixed_version_matrix.contract.gateway_N_control_N_worker_N == "supported"
  and .mixed_version_matrix.contract["any_N-1_component"] == "unsupported_fail_closed"
  and .mixed_version_matrix["new_binary_on_schema_N-1"] == "unsupported_fail_closed"
  and .rollout_failure_decision.restore_in_place == false
  and .default_scope_legacy_contract.existing_resource_ids_change == false
  and .default_scope_legacy_contract.new_required_scope_input_in_v1 == false
  and .idempotency.same_key_different_request == "conflict"
  and .cursors.opaque == true
  and .deprecation.minimum_minor_releases >= 2
  and .deprecation.minimum_days >= 180
' "$compatibility_contract" >/dev/null || {
  echo "compatibility or mixed-version contract is incomplete" >&2
  exit 1
}

openapi_path=$(jq -er '.public_contracts.openapi.path' "$compatibility_contract")
baseline_version=$(jq -er '.public_contracts.openapi.frozen_baseline_version' "$compatibility_contract")
baseline_path=$(jq -er '.public_contracts.openapi.frozen_baseline_path' "$compatibility_contract")
expected_baseline_sha=$(jq -er '.public_contracts.openapi.frozen_baseline_sha256' "$compatibility_contract")
openapi_gate=$(jq -er '.public_contracts.openapi.semantic_gate' "$compatibility_contract")
[[ -f $openapi_path ]] || {
  echo "current OpenAPI contract is missing: $openapi_path" >&2
  exit 1
}
[[ -f $baseline_path ]] || {
  echo "frozen OpenAPI baseline is missing: $baseline_path" >&2
  exit 1
}
[[ -x $openapi_gate ]] || {
  echo "OpenAPI semantic gate is missing or not executable: $openapi_gate" >&2
  exit 1
}
actual_baseline_version=$(jq -er '.info.version' "$baseline_path")
read -r actual_baseline_sha _ < <(sha256sum "$baseline_path")
[[ $actual_baseline_version == "$baseline_version" ]] || {
  echo "OpenAPI baseline version disagrees with the compatibility contract" >&2
  exit 1
}
[[ $actual_baseline_sha == "$expected_baseline_sha" ]] || {
  echo "OpenAPI baseline bytes disagree with the compatibility contract" >&2
  exit 1
}
"$openapi_gate"

upgrade_fixture=tests/migration-fixtures/representative-2x.fixture-manifest.json
jq --exit-status '
  .schema_version == 1
  and .fixture_id == "olp.upgrade.representative-2x.v1"
  and .contract_id == "olp.enterprise.compatibility.v1"
  and .qualification_status == "specification_only"
  and .source_release.highest_migration == 21
  and .source_release.immutable_identity_available == false
  and .source_release.version == null
  and .source_release.binary_or_OCI_sha256 == null
  and .artifact.path == null
  and .artifact.sha256 == null
  and .artifact.generated_test_secrets_only == true
  and .artifact.contains_request_or_response_content == false
  and (.required_populations | type == "array" and length >= 10)
  and (.provisional_current_evidence | type == "array" and length > 0)
  and (.qualification_requirements | type == "array" and length > 0)
' "$upgrade_fixture" >/dev/null || {
  echo "representative 2.x fixture specification is incomplete or overstates qualification" >&2
  exit 1
}
while IFS= read -r provisional_evidence; do
  [[ -e $provisional_evidence ]] || {
    echo "provisional upgrade evidence path is missing: $provisional_evidence" >&2
    exit 1
  }
done < <(jq -r '.provisional_current_evidence[].path' "$upgrade_fixture")

scorecard=docs/enterprise/contracts/enterprise-beta-scorecard.json
jq --exit-status --slurpfile threat_register "$threat_register" '
  def risk_authorities_for_requirement($requirement_id):
    [$threat_register[0].residual_risks[]
      | select(.evidence_requirement_ids | index($requirement_id) != null)
      | .accountable_authorities[]]
    | unique
    | sort;
  ($threat_register[0].threats | map(.id) | sort) as $registered_threats
  | ($threat_register[0].residual_risks | map(.id) | sort) as $registered_risks
  | .approval_model.required_record_types as $required_approval_record_types
  | . as $scorecard
  | .contract_id == "olp.enterprise.beta-scorecard.v1"
  and .decision_status == "accepted_target"
  and (.approval_status == "approval_pending" or .approval_status == "approved")
  and .implementation_status == "not_implemented"
  and .qualification_status == "not_qualified"
  and (.approval_model.required_record_types | sort == ["linear_completion", "repository_review"])
  and (.approval_model.required_record_fields | sort == [
    "accountable_identities", "authorities", "locator", "recorded_at", "type"
  ])
  and (.evidence_model.record_required_fields | sort == [
    "approvals", "locator", "record_id", "recorded_at", "requirement_id",
    "result", "sha256", "source_commit_sha"
  ])
  and .evidence_model.external_locator_verification == "human_verified_against_recorded_sha256_and_approvals"
  and (.evidence_model.repository_digest_rule | type == "string" and length > 0)
  and .m0_contract_gate.status as $m0_status
  | ($m0_status == "approval_pending" or $m0_status == "approved")
  and ($scorecard.m0_contract_gate.decisions | map(.id) == ["M0-01", "M0-02", "M0-03", "M0-04", "M0-05"])
  and ($scorecard.m0_contract_gate.decisions | map(.decision_issue) == ["XOD-83", "XOD-84", "XOD-85", "XOD-86", "XOD-87"])
  and all($scorecard.m0_contract_gate.decisions[];
    . as $decision
    | (.owner | type == "string" and length > 0)
    and (.required_authorities | type == "array" and length > 0)
    and ([.required_authorities[]] | length == (unique | length))
    and (.status == "approval_pending" or .status == "approved")
    and (.approval_evidence | type == "array")
    and (if .status == "approval_pending" then
      (.approval_evidence | length) == 0
    else
      ([.approval_evidence[].type] | sort) == ($required_approval_record_types | sort)
      and all(.approval_evidence[];
        (.type == "linear_completion" or .type == "repository_review")
        and (.locator | type == "string" and length > 0)
        and (.recorded_at | type == "string"
          and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
        and (.authorities | type == "array" and length > 0)
        and ([.authorities[]] | sort == ($decision.required_authorities | sort))
        and (.accountable_identities | type == "array" and length > 0)
        and all(.accountable_identities[]; type == "string" and length > 0)
        and (if .type == "linear_completion" then
          (.locator | startswith("https://linear.app/"))
          and (.locator | contains($decision.decision_issue))
        else
          (.locator | test("^https://github\\.com/[^/]+/[^/]+/(pull|commit)/"))
          and (.commit_sha | type == "string" and test("^([a-f0-9]{40}|[a-f0-9]{64})$"))
        end))
    end))
  and (if $m0_status == "approved" then
    $scorecard.approval_status == "approved"
    and all($scorecard.m0_contract_gate.decisions[]; .status == "approved")
  else
    $scorecard.approval_status == "approval_pending"
    and any($scorecard.m0_contract_gate.decisions[]; .status == "approval_pending")
  end)
  and ($scorecard.beta_gates | map(.id) == ["EB-01", "EB-02", "EB-03", "EB-04", "EB-05", "EB-06", "EB-07", "EB-08"])
  and ([$scorecard.beta_gates[].threats[]] | unique | sort == $registered_threats)
  and all($threat_register[0].threats[];
    . as $threat
    | ([$scorecard.beta_gates[]
        | select(.threats | index($threat.id) != null)
        | .id] | sort) == ($threat.review_gates | sort))
  and ([$scorecard.beta_gates[].residual_risks[]] | unique | sort == $registered_risks)
  and all($threat_register[0].residual_risks[];
    . as $risk
    | ([$scorecard.beta_gates[]
        | select(.residual_risks | index($risk.id) != null)
        | .id] | sort) == ($risk.review_gates | sort)
    and all($risk.evidence_requirement_ids[];
      . as $requirement_id
      | any($scorecard.beta_gates[];
          (.residual_risks | index($risk.id) != null)
          and (.evidence_requirements | map(.id) | index($requirement_id) != null))))
  and ([$scorecard.beta_gates[].evidence_requirements[].id] | length == (unique | length))
  and ([$scorecard.beta_gates[].evidence_records[].record_id] | length == (unique | length))
  and all($scorecard.beta_gates[];
    . as $gate
    | .blocking == true
    and (.owner | type == "string" and length > 0)
    and (.acceptance | type == "string" and length > 0)
    and (.status == "pending" or .status == "passed" or .status == "failed")
    and (.threats | type == "array" and length > 0)
    and (.residual_risks | type == "array" and length > 0 and length == (unique | length))
    and (.evidence_requirements | type == "array" and length > 0)
    and (.evidence_records | type == "array")
    and all(.evidence_requirements[];
      (.id | type == "string" and test("^EB-[0-9]{2}-[A-Z]$"))
      and (.type | IN("automated", "external_review", "rehearsal", "load_profile", "reference_deployment", "signed_acceptance"))
      and (.locator | type == "string" and length > 0)
      and ((risk_authorities_for_requirement(.id)) as $risk_authorities
        | if ($risk_authorities | length) > 0
          then (.required_authorities | unique | sort) == $risk_authorities
          else (.required_authorities? // []) == []
          end))
    and all(.evidence_records[];
      . as $record
      | ($gate.evidence_requirements[] | select(.id == $record.requirement_id)) as $requirement
      | ($record.record_id | type == "string" and length > 0)
      and ($record.requirement_id | type == "string")
      and ($record.result == "passed" or $record.result == "failed")
      and ($record.locator | type == "string" and length > 0 and (startswith("planned:") | not))
      and ($record.sha256 | type == "string" and test("^[a-f0-9]{64}$"))
      and ($record.recorded_at | type == "string"
        and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$"))
      and ($record.source_commit_sha | type == "string" and test("^([a-f0-9]{40}|[a-f0-9]{64})$"))
      and ($record.approvals | type == "array" and length > 0)
      and all($record.approvals[];
        (.authority | type == "string" and length > 0)
        and (.accountable_identity | type == "string" and length > 0)
        and (.recorded_at | type == "string"
          and test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")))
      and (if $record.result == "passed" then
        (($requirement.required_authorities // [])
          - ([$record.approvals[].authority] | unique) | length) == 0
      else true end))
    and (if .status == "passed" then
      all(.evidence_requirements[]; (.locator | startswith("planned:") | not))
      and all(.evidence_records[]; .result == "passed")
      and all(.evidence_requirements[];
        . as $requirement
        | any($gate.evidence_records[]; .requirement_id == $requirement.id and .result == "passed"))
    elif .status == "failed" then
      any(.evidence_records[]; .result == "failed")
    else true end))
' "$scorecard" >/dev/null || {
  echo "enterprise beta scorecard is incomplete or out of order" >&2
  exit 1
}

while IFS= read -r evidence_path; do
  [[ -f $evidence_path ]] || {
    echo "scorecard M0 evidence is missing: $evidence_path" >&2
    exit 1
  }
done < <(jq -r '
  .m0_contract_gate.decisions[]
  | .source, (.machine_contract // empty), (.policy_contract // empty)
' "$scorecard")

validate_decision_authorities() {
  local decision_id=$1 comparison=$2
  shift 2
  local declared required missing

  declared=$(jq -c --arg id "$decision_id" '
    .m0_contract_gate.decisions[]
    | select(.id == $id)
    | .required_authorities
    | unique
    | sort
  ' "$scorecard")
  required=$(jq -c -s '[.[].approval.required_authorities[]] | unique | sort' "$@")
  [[ -n $declared && -n $required ]] || {
    echo "approval authority source is missing for $decision_id" >&2
    exit 1
  }

  if [[ $comparison == exact ]]; then
    [[ $declared == "$required" ]] || {
      echo "$decision_id approval authorities disagree with its source contract: declared=$declared required=$required" >&2
      exit 1
    }
  else
    missing=$(jq -nr --argjson declared "$declared" --argjson required "$required" \
      '$required - $declared | @json')
    [[ $missing == "[]" ]] || {
      echo "$decision_id omits source-contract approval authorities: $missing" >&2
      exit 1
    }
  fi
}

validate_decision_approval_status() {
  local decision_id=$1
  shift
  local decision_status source_status source

  decision_status=$(jq -er --arg id "$decision_id" '
    .m0_contract_gate.decisions[] | select(.id == $id) | .status
  ' "$scorecard")
  for source in "$@"; do
    source_status=$(jq -er '.approval_status' "$source")
    [[ $source_status == "$decision_status" ]] || {
      echo "$decision_id approval status disagrees with $source: decision=$decision_status source=$source_status" >&2
      exit 1
    }
  done
}

validate_decision_authorities M0-01 exact "$ownership_contract"
validate_decision_authorities M0-03 exact "$connector_contract" "$policy_contract"
validate_decision_authorities M0-04 exact "$compatibility_contract"
validate_decision_authorities M0-05 exact "$capacity_contract" "$threat_register" "$path_contract"
validate_decision_approval_status M0-01 "$ownership_contract"
validate_decision_approval_status M0-03 "$connector_contract" "$policy_contract"
validate_decision_approval_status M0-04 "$compatibility_contract"
validate_decision_approval_status M0-05 "$capacity_contract" "$threat_register" "$path_contract"

for approval_source in \
  "$path_contract" \
  "$ownership_contract" \
  "$connector_contract" \
  "$policy_contract" \
  "$compatibility_contract" \
  "$capacity_contract" \
  "$threat_register"; do
  approval_locator=$(jq -er '.approval.evidence_source' "$approval_source")
  validate_evidence_locator "$approval_locator" "approval source $approval_source" false
done

m0_02_authorities=$(jq -c '
  .m0_contract_gate.decisions[]
  | select(.id == "M0-02")
  | .required_authorities
  | unique
  | sort
' "$scorecard")
[[ $m0_02_authorities == '["architecture_domain","gateway_security"]' ]] || {
  echo "M0-02 approval authorities must cover architecture/domain and gateway/security" >&2
  exit 1
}

while IFS=$'\t' read -r gate_id evidence_locator; do
  validate_evidence_locator "$evidence_locator" "scorecard requirement $gate_id" true
done < <(jq -r '.beta_gates[] | .id as $gate | .evidence_requirements[] | [$gate, .locator] | @tsv' "$scorecard")

while IFS=$'\t' read -r gate_id record_id evidence_locator evidence_sha; do
  validate_evidence_locator "$evidence_locator" "scorecard evidence $gate_id/$record_id" false "$evidence_sha"
done < <(jq -r '.beta_gates[] | .id as $gate | .evidence_records[] | [$gate, .record_id, .locator, .sha256] | @tsv' "$scorecard")

if [[ -x scripts/check-migration-contract.sh ]]; then
  scripts/check-migration-contract.sh --self-test-rules
  scripts/check-migration-contract.sh
else
  echo "migration contract checker is missing or not executable" >&2
  exit 1
fi

if [[ -x scripts/test-migration-contract.sh ]]; then
  scripts/test-migration-contract.sh
else
  echo "migration contract integration test is missing or not executable" >&2
  exit 1
fi

if [[ -x scripts/test-upgrade-recovery-decision.sh ]]; then
  scripts/test-upgrade-recovery-decision.sh
else
  echo "upgrade recovery decision test is missing or not executable" >&2
  exit 1
fi

if [[ -x scripts/test-capacity-profile-plan.sh ]]; then
  scripts/test-capacity-profile-plan.sh
else
  echo "capacity profile plan renderer test is missing or not executable" >&2
  exit 1
fi

if [[ -x scripts/test-openapi-compatibility.sh ]]; then
  scripts/test-openapi-compatibility.sh
else
  echo "OpenAPI compatibility regression test is missing or not executable" >&2
  exit 1
fi

echo "enterprise contracts are internally consistent"
