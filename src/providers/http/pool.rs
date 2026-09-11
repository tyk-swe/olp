use crate::access::{
    permissions::require_permission,
    policy::Permission,
    principal::{MutationPrincipal, ReadPrincipal},
};
use crate::http::{
    control::{
        error_mapping::map_configuration,
        idempotency::{MutationReply, ReplayableMutation},
        preconditions::{if_match, with_etag},
        provenance::Provenance,
        secrets::WriteOnlySecret,
        state::ManagementState,
    },
    problem::Problem,
};
use crate::providers::pool::CredentialSlot;
use axum::{
    Json,
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

#[derive(Serialize, ToSchema)]
pub(crate) struct SlotList {
    pub health: std::collections::BTreeMap<Uuid, SlotHealth>,
    pub connection_usage: Option<crate::limits::admission::ProviderQuotaUsage>,
    pub items: Vec<CredentialSlot>,
    pub etag: Uuid,
}
#[derive(Serialize, ToSchema)]
pub(crate) struct SlotHealth {
    pub validated_at: Option<chrono::DateTime<chrono::Utc>>,
    pub active_credential_version_id: Option<Uuid>,
    pub cooling_down: Option<bool>,
    pub usage: Option<crate::limits::admission::ProviderQuotaUsage>,
}
async fn slot_list(
    state: &ManagementState,
    provider: Uuid,
    etag: Uuid,
) -> Result<SlotList, Problem> {
    let items = crate::providers::pool_store::list(&state.request_boundary.pool, provider)
        .await
        .map_err(map_configuration)?;
    let rows = sqlx::query_as::<_, (Uuid, Option<chrono::DateTime<chrono::Utc>>, Option<Uuid>)>(
        "SELECT s.id,CASE WHEN s.is_default AND s.enabled AND p.last_probe_status='succeeded' \
            THEN p.last_probe_at WHEN NOT s.is_default THEN s.validated_at \
            END,pc.credential_version_id FROM provider_credential_slots s JOIN providers p ON \
            p.id=s.provider_id LEFT JOIN provider_revision_credentials pc ON \
            pc.provider_revision_id=p.active_revision_id AND pc.slot_id=s.id WHERE s.provider_id=$1",
    )
    .bind(provider)
    .fetch_all(&state.request_boundary.pool)
    .await
    .map_err(|_| Problem::internal())?;
    let backend = state.request_boundary.inference.limiter.current();
    let backend = backend.as_deref();
    let items_ref = &items;
    let slot_health = rows.into_iter().map(
        |(id, validated_at, active_credential_version_id)| async move {
            let (cooling_down, usage) = match backend {
                Some(backend) => {
                    let selected = items_ref
                        .iter()
                        .find(|s| s.id == id)
                        .and_then(|s| s.credential_version_id);
                    futures::join!(
                        crate::providers::pool_transport::is_cooling(
                            backend,
                            provider,
                            id,
                            active_credential_version_id.or(selected),
                        ),
                        async {
                            backend
                                .provider_usage(
                                    id,
                                    &crate::providers::pool_transport::slot_lookup(id),
                                )
                                .await
                                .ok()
                                .flatten()
                        }
                    )
                }
                None => (None, None),
            };
            (
                id,
                SlotHealth {
                    validated_at,
                    active_credential_version_id,
                    cooling_down,
                    usage,
                },
            )
        },
    );
    let connection_usage = async {
        match backend {
            Some(backend) => backend
                .provider_usage(
                    provider,
                    &crate::providers::pool_transport::connection_lookup(provider),
                )
                .await
                .ok()
                .flatten(),
            None => None,
        }
    };
    let (health, connection_usage) =
        futures::join!(futures::future::join_all(slot_health), connection_usage);
    let health = health.into_iter().collect();
    Ok(SlotList {
        items,
        health,
        connection_usage,
        etag,
    })
}

#[derive(Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub(crate) struct SlotWrite {
    pub slot: CredentialSlot,
    #[schema(value_type=Option<String>, write_only)]
    pub credential: Option<WriteOnlySecret>,
}

#[utoipa::path(get, path="/api/v3/provider-vendors", tag="providers", responses((status=200,body=Vec<crate::providers::catalog::Vendor>)), security(("sessionCookie"=[])))]
pub(crate) async fn provider_vendors(
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Json<Vec<crate::providers::catalog::Vendor>>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    Ok(Json(crate::providers::catalog::vendors().to_vec()))
}

#[utoipa::path(get, path="/api/v3/providers/{provider_id}/credential-slots", tag="providers", params(("provider_id"=Uuid,Path)), responses((status=200,body=SlotList)), security(("sessionCookie"=[])))]
pub(crate) async fn credential_slots(
    State(state): State<ManagementState>,
    Path(provider): Path<Uuid>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let record = crate::providers::repository::get_provider(&state.request_boundary.pool, provider)
        .await
        .map_err(map_configuration)?;
    with_etag(
        Json(slot_list(&state, provider, record.etag).await?),
        record.etag,
    )
}

#[utoipa::path(put,path="/api/v3/providers/{provider_id}/credential-slots/{slot_id}",tag="providers",params(("provider_id"=Uuid,Path),("slot_id"=Uuid,Path)),request_body=SlotWrite,responses((status=200,body=SlotList)),security(("sessionCookie"=[],"csrfToken"=[])))]
pub(crate) async fn put_credential_slot(
    State(state): State<ManagementState>,
    Path((provider, id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    Provenance(provenance): Provenance,
    MutationPrincipal(principal): MutationPrincipal,
    Json(mut request): Json<SlotWrite>,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let expected = if_match(&headers)?;
    request.slot.id = id;
    request
        .slot
        .validate()
        .map_err(|e| Problem::field_validation("slot", e))?;
    let digest = request
        .credential
        .as_ref()
        .map(|s| crate::database::idempotency::secret_digest(s.expose().as_bytes()));
    let fingerprint = serde_json::json!({"provider":provider,"etag":expected,"slot":request.slot,"credential_digest":digest});
    ReplayableMutation::new(
        &state,
        principal.user_id,
        "provider.credential_slot.put",
        &headers,
        &fingerprint,
    )?
    .run(|_| async {
        let pool = &state.request_boundary.pool;
        let record = crate::providers::repository::get_provider(pool, provider)
            .await.map_err(map_configuration)?;
        if let Some(secret) = &request.credential {
            if secret.expose().is_empty() || secret.expose().len() > 8192 {
                return Err(Problem::field_validation("credential", "Provide at most 8 KiB"));
            }
            crate::providers::http::credentials::validate_rotated_credential(&record, secret.expose())
                .map_err(|e| Problem::field_validation("credential", e))?;
        }
        let mut tx = pool.begin().await.map_err(|_| Problem::internal())?;
        let locked = crate::providers::queries::lock_provider(&mut tx, provider)
            .await.map_err(|_| Problem::internal())?
            .ok_or_else(|| map_configuration(crate::providers::error::Error::NotFound))?;
        if locked.etag != expected {
            return Err(map_configuration(crate::providers::error::Error::PreconditionFailed));
        }
        if locked.state == "disabled" {
            return Err(Problem::conflict("provider_disabled", "Restore the provider before editing credentials"));
        }
        let count: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM provider_credential_slots WHERE provider_id=$1 AND id<>$2",
        ).bind(provider).bind(id).fetch_one(&mut *tx).await.map_err(|_| Problem::internal())?;
        if count >= 64 {
            return Err(Problem::field_validation("slot", "At most 64 credential slots are supported per connection"));
        }
        sqlx::query(
            "INSERT INTO provider_credential_slots(id,provider_id,name)
             VALUES($1,$2,$3) ON CONFLICT(id) DO NOTHING",
        ).bind(id).bind(provider).bind(&request.slot.name).execute(&mut *tx).await
            .map_err(|_| Problem::conflict("slot_conflict", "Credential slot name or identity already exists"))?;
        let existing: Option<Option<Uuid>> = sqlx::query_scalar(
            "SELECT selected_version_id FROM provider_credential_slots WHERE id=$1 AND provider_id=$2",
        ).bind(id).bind(provider).fetch_optional(&mut *tx).await.map_err(|_| Problem::internal())?;
        let Some(existing) = existing else {
            return Err(Problem::conflict("slot_conflict", "Credential slot belongs to another connection"));
        };
        request.slot.credential_version_id = existing;
        if let Some(secret) = &request.credential {
            let version: i32 = sqlx::query_scalar(
                "SELECT COALESCE(max(version),0)+1 FROM provider_credential_versions WHERE provider_id=$1",
            ).bind(provider).fetch_one(&mut *tx).await.map_err(|_| Problem::internal())?;
            let version_id = Uuid::now_v7();
            let master = state.master_key.as_ref()
                .ok_or_else(|| Problem::service_unavailable("master_key_not_configured"))?;
            let encrypted = master.seal(
                secret.expose().as_bytes(),
                &crate::crypto::aad::credential(provider, version_id, version as u32),
            ).map_err(|_| Problem::internal())?;
            sqlx::query(
                "INSERT INTO provider_credential_versions
                 (id,provider_id,slot_id,version,ciphertext,nonce,master_key_version,created_by)
                 VALUES($1,$2,$3,$4,$5,$6,$7,$8)",
            ).bind(version_id).bind(provider).bind(id).bind(version)
                .bind(encrypted.ciphertext).bind(encrypted.nonce.to_vec())
                .bind(encrypted.key_version as i32).bind(principal.user_id)
                .execute(&mut *tx).await.map_err(|_| Problem::internal())?;
            request.slot.credential_version_id = Some(version_id);
        }
        crate::providers::pool_store::put(&mut tx, provider, &request.slot)
            .await.map_err(map_configuration)?;
        let etag = Uuid::now_v7();
        sqlx::query(
            "UPDATE providers SET state='draft',etag=$2,last_probe_at=NULL,last_probe_status=NULL,
             active_credential_version_id=CASE WHEN id=$3 THEN $4
                 ELSE active_credential_version_id END WHERE id=$1",
        ).bind(provider).bind(etag).bind(id).bind(request.slot.credential_version_id)
            .execute(&mut *tx).await.map_err(|_| Problem::internal())?;
        crate::access::audit_events::record_success(
            &mut *tx, &provenance, principal.user_id, "provider.credential_slot.put", "provider", provider,
        ).await.map_err(|_| Problem::internal())?;
        tx.commit().await.map_err(|_| Problem::internal())?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: slot_list(&state, provider, etag).await?,
            etag: Some(etag),
            location: None,
        })
    }).await
}

#[utoipa::path(post,path="/api/v3/providers/{provider_id}/credential-slots/{slot_id}/validate",tag="providers",params(("provider_id"=Uuid,Path),("slot_id"=Uuid,Path)),responses((status=200,body=SlotList)),security(("sessionCookie"=[],"csrfToken"=[])))]
pub(crate) async fn validate_credential_slot(
    State(state): State<ManagementState>,
    Path((provider, id)): Path<(Uuid, Uuid)>,
    headers: HeaderMap,
    Provenance(provenance): Provenance,
    MutationPrincipal(principal): MutationPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ManageProviders)?;
    let expected = if_match(&headers)?;
    let slots = crate::providers::pool_store::list(&state.request_boundary.pool, provider)
        .await
        .map_err(map_configuration)?;
    let slot = slots
        .iter()
        .find(|s| s.id == id)
        .ok_or_else(|| map_configuration(crate::providers::error::Error::NotFound))?;
    let connector = crate::providers::pool_store::connector(&state, provider, slot).await?;
    let checked = crate::providers::pool_store::validate_model_access(
        &state.request_boundary.pool,
        provider,
        &slot.allowed_models,
        &connector,
        false,
    )
    .await
    .map_err(map_configuration)?;
    if checked == 0 {
        return Err(Problem::field_validation(
            "models",
            "Select enabled models before validating this credential",
        ));
    }
    let mut tx = state
        .request_boundary
        .pool
        .begin()
        .await
        .map_err(|_| Problem::internal())?;
    let locked = crate::providers::queries::lock_provider(&mut tx, provider)
        .await
        .map_err(|_| Problem::internal())?
        .ok_or_else(Problem::internal)?;
    if locked.etag != expected {
        return Err(map_configuration(
            crate::providers::error::Error::PreconditionFailed,
        ));
    }
    sqlx::query(
        "UPDATE provider_credential_slots SET validated_at=now() WHERE provider_id=$1 AND id=$2",
    )
    .bind(provider)
    .bind(id)
    .execute(&mut *tx)
    .await
    .map_err(|_| Problem::internal())?;
    crate::access::audit_events::record_success(
        &mut *tx,
        &provenance,
        principal.user_id,
        "provider.credential_slot.validate",
        "provider",
        provider,
    )
    .await
    .map_err(|_| Problem::internal())?;
    tx.commit().await.map_err(|_| Problem::internal())?;
    if let Some(backend) = state.request_boundary.inference.limiter.current() {
        crate::providers::pool_transport::clear_cooldowns(
            backend.as_ref(),
            provider,
            id,
            slot.credential_version_id,
        )
        .await;
    }
    with_etag(Json(slot_list(&state, provider, expected).await?), expected)
}
