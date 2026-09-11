use crate::providers::{error::Error, pool::CredentialSlot};
use sqlx::{Postgres, Transaction};
use uuid::Uuid;

/// Validate the selected secret against each enabled model contract. The same
/// bounded checks serve explicit slots and the legacy default-credential probe.
pub(crate) async fn validate_model_access(
    pool: &sqlx::PgPool,
    provider: Uuid,
    allowed_models: &[String],
    connector: &crate::providers::connectors::ProviderConnector,
    certified_only: bool,
) -> Result<usize, Error> {
    use futures::{StreamExt, TryStreamExt};
    let tuples: Vec<(String, String, String, String)> = sqlx::query_as(
        "SELECT pm.upstream_model,mc.operation,mc.surface,mc.mode
         FROM provider_models pm JOIN model_capabilities mc ON mc.provider_model_id=pm.id
         WHERE pm.provider_id=$1 AND pm.enabled
           AND (cardinality($2::text[])=0 OR pm.upstream_model=ANY($2))
           AND (NOT $3 OR mc.source='certified')
         ORDER BY pm.id,mc.operation,mc.surface,mc.mode",
    )
    .bind(provider)
    .bind(allowed_models)
    .bind(certified_only)
    .fetch_all(pool)
    .await?;
    let count = tuples.len();
    futures::stream::iter(
        tuples
            .into_iter()
            .map(|(model, operation, surface, mode)| async move {
                let capability = crate::providers::openai::certification::CompatibleCapability {
                    operation: operation
                        .parse()
                        .map_err(|_| Error::Invalid("Unknown model operation".into()))?,
                    surface: surface
                        .parse()
                        .map_err(|_| Error::Invalid("Unknown model surface".into()))?,
                    mode: mode
                        .parse()
                        .map_err(|_| Error::Invalid("Unknown model transport mode".into()))?,
                };
                connector
                    .certify_capability(&model, capability)
                    .await
                    .map_err(|error| {
                        Error::Invalid(format!("{model} ({operation}/{surface}/{mode}): {error}"))
                    })
            }),
    )
    .buffer_unordered(8)
    .try_collect::<Vec<_>>()
    .await?;
    Ok(count)
}

/// `CredentialSlot` JSON projection of a `provider_credential_slots s` row.
/// A macro so `concat!` can splice it into static SQL.
macro_rules! slot_json {
    () => {
        "(to_jsonb(s) - \
            ARRAY['provider_id','is_default','selected_version_id','validated_at','created_at']) || \
            jsonb_build_object('credential_version_id', s.selected_version_id)"
    };
}

pub async fn list(pool: &sqlx::PgPool, provider: Uuid) -> Result<Vec<CredentialSlot>, Error> {
    Ok(
        sqlx::query_scalar::<_, sqlx::types::Json<CredentialSlot>>(concat!(
            "SELECT ",
            slot_json!(),
            " FROM provider_credential_slots s WHERE provider_id = $1 ORDER BY priority, name, id"
        ))
        .bind(provider)
        .fetch_all(pool)
        .await?
        .into_iter()
        .map(|s| s.0)
        .collect(),
    )
}

pub async fn snapshot(
    transaction: &mut Transaction<'_, Postgres>,
    provider: Uuid,
    revision: Uuid,
) -> Result<(), Error> {
    let invalid: bool = sqlx::query_scalar(
        "SELECT EXISTS(SELECT 1 FROM provider_credential_slots s JOIN providers p ON p.id = \
            s.provider_id LEFT JOIN provider_credential_versions cv ON cv.id = s.selected_version_id \
            WHERE s.provider_id = $1 AND s.enabled AND NOT s.is_default AND (cv.id IS NULL OR \
            cv.revoked_at IS NOT NULL OR s.validated_at IS NULL))",
    )
    .bind(provider)
    .fetch_one(&mut **transaction)
    .await?;
    if invalid {
        return Err(Error::Invalid("Validate every enabled credential slot against the completed provider draft before activation".into()));
    }
    sqlx::query(concat!(
        "INSERT INTO provider_revision_credentials(provider_revision_id, slot_id, \
            credential_version_id, configuration) SELECT $2, s.id, s.selected_version_id, ",
        slot_json!(),
        " FROM provider_credential_slots s LEFT JOIN provider_credential_versions cv ON cv.id = \
            s.selected_version_id WHERE s.provider_id = $1 AND s.enabled AND \
            ((cv.id IS NOT NULL AND cv.revoked_at IS NULL) OR \
             (s.is_default AND s.selected_version_id IS NULL AND EXISTS(SELECT 1 FROM \
                provider_revisions pr WHERE pr.id=$2 AND pr.auth_mode IN ('none','adc','default_chain'))))"
    ))
        .bind(provider)
        .bind(revision)
        .execute(&mut **transaction)
        .await?;
    Ok(())
}

pub(crate) async fn put(
    transaction: &mut Transaction<'_, Postgres>,
    provider: Uuid,
    slot: &CredentialSlot,
) -> Result<(), Error> {
    slot.validate().map_err(Error::Invalid)?;
    if let Some(version) = slot.credential_version_id {
        let owned: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM provider_credential_versions WHERE id = $1 AND provider_id = \
            $2 AND slot_id = $3 AND (revoked_at IS NULL OR NOT $4))").bind(version).bind(provider).bind(slot.id).bind(slot.enabled).fetch_one(&mut **transaction).await?;
        if !owned {
            return Err(Error::InvalidCredential);
        }
    }
    let key_count: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM api_keys WHERE id = ANY($1) AND revoked_at IS NULL",
    )
    .bind(&slot.allowed_api_keys)
    .fetch_one(&mut **transaction)
    .await?;
    if key_count as usize
        != slot
            .allowed_api_keys
            .iter()
            .collect::<std::collections::BTreeSet<_>>()
            .len()
    {
        return Err(Error::Invalid("Unknown or revoked gateway API key".into()));
    }
    write_slot_columns(transaction, provider, slot).await
}

/// Persist every editable slot column and invalidate its validation stamp.
async fn write_slot_columns(
    transaction: &mut Transaction<'_, Postgres>,
    provider: Uuid,
    slot: &CredentialSlot,
) -> Result<(), Error> {
    sqlx::query(
        "UPDATE provider_credential_slots SET name=$3, enabled=$4, priority=$5, weight=$6, \
            selected_version_id=$7, allowed_models=$8, allowed_routes=$9, allowed_api_keys=$10, \
            requests_per_minute=$11, tokens_per_minute=$12, max_concurrency=$13, validated_at=NULL \
            WHERE provider_id=$1 AND id=$2",
    )
    .bind(provider)
    .bind(slot.id)
    .bind(&slot.name)
    .bind(slot.enabled)
    .bind(i32::from(slot.priority))
    .bind(slot.weight as i32)
    .bind(slot.credential_version_id)
    .bind(&slot.allowed_models)
    .bind(&slot.allowed_routes)
    .bind(&slot.allowed_api_keys)
    .bind(slot.requests_per_minute.map(|n| n as i32))
    .bind(slot.tokens_per_minute.map(|n| n as i64))
    .bind(slot.max_concurrency.map(|n| n as i32))
    .execute(&mut **transaction)
    .await?;
    Ok(())
}

pub(crate) async fn connector(
    state: &crate::http::control::state::ManagementState,
    provider_id: Uuid,
    slot: &CredentialSlot,
) -> Result<crate::providers::connectors::ProviderConnector, crate::http::problem::Problem> {
    use crate::http::{control::error_mapping::map_configuration, problem::Problem};
    let mut provider =
        crate::providers::repository::get_provider(&state.request_boundary.pool, provider_id)
            .await
            .map_err(map_configuration)?;
    let (id, version, ciphertext, nonce, key_version): (Uuid, i32, Vec<u8>, Vec<u8>, i32) =
        sqlx::query_as(
            "SELECT id, version, ciphertext, nonce, master_key_version FROM \
                provider_credential_versions WHERE id=$1 AND provider_id=$2 AND slot_id=$3 AND \
                revoked_at IS NULL",
        )
        .bind(slot.credential_version_id)
        .bind(provider_id)
        .bind(slot.id)
        .fetch_optional(&state.request_boundary.pool)
        .await
        .map_err(|_| Problem::internal())?
        .ok_or_else(|| {
            Problem::field_validation("credential", "Credential is missing or revoked")
        })?;
    let key = state
        .master_key
        .as_ref()
        .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
    let encrypted = crate::crypto::envelope::EncryptedSecret {
        key_version: key_version as u32,
        ciphertext,
        nonce: nonce.try_into().map_err(|_| Problem::internal())?,
    };
    let plaintext = key
        .open(
            &encrypted,
            &crate::crypto::aad::credential(provider_id, id, version as u32),
        )
        .map_err(|_| Problem::internal())?;
    if let Some(model) = slot.allowed_models.first() {
        provider.configuration.probe_model = Some(model.clone());
    }
    let credential = crate::providers::connectors::input::provider_credential(
        &provider.configuration,
        Some(&plaintext),
    )
    .map_err(|e| Problem::field_validation("credential", e.to_string()))?;
    crate::providers::connectors::create(
        provider.configuration,
        credential,
        &state.provider_egress_policy,
        state.provider_response_limits,
    )
    .await
    .map_err(|e| Problem::field_validation("credential", e.to_string()))
}

/// Restore slot policy while keeping current secret versions; historical secrets
/// never become active merely because configuration is restored.
pub(crate) async fn restore(
    tx: &mut Transaction<'_, Postgres>,
    provider: Uuid,
    revision: Uuid,
) -> Result<(), Error> {
    let historical = sqlx::query_scalar::<_, sqlx::types::Json<CredentialSlot>>(
        "SELECT configuration FROM provider_revision_credentials WHERE provider_revision_id=$1",
    )
    .bind(revision)
    .fetch_all(&mut **tx)
    .await?;
    sqlx::query(
        "UPDATE provider_credential_slots SET enabled=false,validated_at=NULL WHERE provider_id=$1",
    )
    .bind(provider)
    .execute(&mut **tx)
    .await?;
    for slot in historical {
        let mut slot = slot.0;
        // The slot keeps its current name and secret version; only the
        // historical policy columns are restored. A slot deleted since the
        // revision was published stays deleted.
        let Some((name, version)) = sqlx::query_as::<_, (String, Option<Uuid>)>(
            "SELECT name, selected_version_id FROM provider_credential_slots \
             WHERE id=$1 AND provider_id=$2",
        )
        .bind(slot.id)
        .bind(provider)
        .fetch_optional(&mut **tx)
        .await?
        else {
            continue;
        };
        slot.name = name;
        slot.credential_version_id = version;
        // A revoked gateway key in historical restrictions cannot grant access;
        // preserve its ID rather than failing an otherwise reviewable restore.
        write_slot_columns(tx, provider, &slot).await?;
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::providers::connector::Timeouts;
    use crate::providers::mock_server::{MockResponse, response, spawn_sequence, status_response};
    use crate::providers::openai::{ApiKey, ConnectorConfig};
    use std::sync::Arc;

    #[tokio::test]
    #[ignore = "requires PostgreSQL via make integration"]
    async fn model_access_checks_all_enabled_models_and_honors_slot_restrictions() {
        let db = crate::test_support::TestDb::create_migrated("credential_model_access").await;
        let pool = db.pool(2).await;
        let actor = Uuid::now_v7();
        let provider = Uuid::now_v7();
        sqlx::query("INSERT INTO users(id,email,display_name,role) VALUES($1,'access@pool.test','Owner','owner')")
            .bind(actor).execute(&pool).await.unwrap();
        sqlx::query("INSERT INTO providers(id,name,kind,auth_mode,etag,created_by) VALUES($1,'access','openai','api_key',$2,$3)")
            .bind(provider).bind(Uuid::now_v7()).bind(actor).execute(&pool).await.unwrap();
        for (name, enabled, certified) in [
            ("first", true, true),
            ("second", true, true),
            ("disabled", false, true),
            ("unreviewed-seed", true, false),
        ] {
            let model = Uuid::now_v7();
            sqlx::query("INSERT INTO provider_models(id,provider_id,upstream_model,display_name,enabled) VALUES($1,$2,$3,$3,$4)")
                .bind(model).bind(provider).bind(name).bind(enabled).execute(&pool).await.unwrap();
            sqlx::query("INSERT INTO model_capabilities(provider_model_id,operation,surface,mode,source,certified_at) VALUES($1,'embeddings','openai','unary',CASE WHEN $2 THEN 'certified' ELSE 'declared' END,CASE WHEN $2 THEN now() END)")
                .bind(model).bind(certified).execute(&pool).await.unwrap();
        }
        let success = || {
            MockResponse::immediate(response(
                "application/json",
                r#"{"object":"list","model":"first","data":[{"object":"embedding","index":0,"embedding":[0.1]}],"usage":{"prompt_tokens":1,"total_tokens":1}}"#,
            ))
        };
        let (endpoint, requests) = spawn_sequence(
            "/v1/",
            vec![
                success(),
                MockResponse::immediate(status_response(
                    "403 Forbidden",
                    "application/json",
                    r#"{"error":{"message":"model denied"}}"#,
                )),
            ],
        )
        .await;
        let connector = |endpoint: &str| {
            crate::providers::connectors::ProviderConnector::OpenAi(Arc::new(
                crate::providers::openai::transport::Connector::new(
                    ConnectorConfig::for_local_test(endpoint, Timeouts::default()),
                    ApiKey::new("model-access-secret").unwrap(),
                ),
            ))
        };
        assert!(
            validate_model_access(&pool, provider, &[], &connector(&endpoint), true)
                .await
                .is_err()
        );
        let requests = requests.await.unwrap();
        assert_eq!(requests.len(), 2);
        let mut models = requests
            .iter()
            .map(|request| {
                let offset =
                    crate::providers::mock_server::find_bytes(request, b"\r\n\r\n").unwrap() + 4;
                let body: serde_json::Value = serde_json::from_slice(&request[offset..]).unwrap();
                body["model"].as_str().unwrap().to_owned()
            })
            .collect::<Vec<_>>();
        models.sort();
        assert_eq!(models, ["first", "second"]);
        let (endpoint, requests) = spawn_sequence("/v1/", vec![success()]).await;
        assert_eq!(
            validate_model_access(
                &pool,
                provider,
                &["first".into()],
                &connector(&endpoint),
                false
            )
            .await
            .unwrap(),
            1
        );
        assert_eq!(requests.await.unwrap().len(), 1);
    }
}
