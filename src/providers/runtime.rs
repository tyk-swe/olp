use crate::crypto::envelope::EncryptedSecret;
use crate::ids::ProviderId;
use crate::providers::error::Error;
use crate::providers::record_validation::stored_version;
use crate::runtime::snapshot::Snapshot;
use sqlx::FromRow;
use uuid::Uuid;

#[derive(Debug, Clone)]
pub struct RuntimeProvider {
    pub provider_id: ProviderId,
    pub provider_revision_id: Uuid,
    pub configuration: crate::providers::configuration::ProviderConfiguration,

    pub credential_id: Option<Uuid>,
    pub credential_version: Option<u32>,
    pub encrypted_credential: Option<EncryptedSecret>,
}

/// Loads the release-exact connector configuration and credential named by
/// a verified runtime sidecar. Mutable configuration drafts are deliberately not
/// consulted, so testing a replacement endpoint or credential cannot alter
/// the transport used by the last activated provider revision.
pub async fn runtime_provider_configurations(
    pool: &sqlx::PgPool,
    snapshot: &Snapshot,
) -> Result<Vec<RuntimeProvider>, Error> {
    let mut records = Vec::with_capacity(snapshot.providers.len());
    for runtime_provider in snapshot.providers.values() {
        let expected_credential = runtime_provider
            .active_credential
            .map(|credential| credential.as_uuid());
        let published_slots = snapshot.routing.credentials.get(&runtime_provider.id);
        let use_default = published_slots.is_none_or(|slots| {
            slots
                .iter()
                .any(|slot| slot.enabled && slot.credential_version_id == expected_credential)
        });
        let row = sqlx::query_as::<_, RuntimeProviderRow>(
            "SELECT rpc.provider_id AS id, rpc.provider_revision_id, rpc.kind, rpc.endpoint, rpc.cloud_region, \
                        rpc.cloud_project, rpc.deployment, rpc.api_version, rpc.auth_mode, rpc.options, \
                        cv.id AS \"credential_id\", cv.version AS \"credential_version\", \
                        cv.ciphertext AS \"ciphertext\", cv.nonce AS \"nonce\", \
                        cv.master_key_version AS \"master_key_version\" \
                 FROM runtime_generation_provider_configs rpc \
                 JOIN providers p ON p.id = rpc.provider_id \
                 LEFT JOIN provider_credential_versions cv \
                   ON cv.id = rpc.active_credential_version_id AND cv.revoked_at IS NULL AND $4 \
                 WHERE rpc.provider_id = $1 AND rpc.runtime_generation_id = $3 \
                   AND rpc.active_credential_version_id IS NOT DISTINCT FROM $2 \
                   AND p.active_revision_id IS NOT NULL \
                   AND p.state <> 'disabled'::provider_state \
                   AND (NOT $4 OR rpc.active_credential_version_id IS NULL OR cv.id IS NOT NULL)",
        )
    .bind(runtime_provider.id.as_uuid())
    .bind(expected_credential)
    .bind(snapshot.generation.id.as_uuid())
    .bind(use_default)
            .fetch_optional(pool)
            .await?
            .ok_or(Error::InvalidCredential)?;
        if row.configuration.kind != runtime_provider.kind
            || runtime_provider.revision_id != row.provider_revision_id
        {
            return Err(Error::InvalidCredential);
        }
        if use_default {
            records.push(runtime_provider_configuration_from_row(row)?);
        }
        let slots: Vec<_> = published_slots
            .into_iter()
            .flatten()
            .filter(|slot| slot.enabled && slot.credential_version_id != expected_credential)
            .collect();
        if slots.is_empty() {
            continue;
        }
        let slot_ids: Vec<Uuid> = slots.iter().map(|slot| slot.id).collect();
        let mut rows: std::collections::BTreeMap<Uuid, RuntimeProviderRow> = sqlx::query_as::<_, RuntimeProviderRow>(
            "SELECT pr.provider_id AS id, pr.id AS provider_revision_id, pr.kind, pr.endpoint, pr.cloud_region, pr.cloud_project, pr.deployment, pr.api_version, pr.auth_mode, pr.options, cv.id AS credential_id, cv.version AS credential_version, cv.ciphertext, cv.nonce, cv.master_key_version FROM provider_revisions pr JOIN provider_revision_credentials pc ON pc.provider_revision_id = pr.id JOIN provider_credential_versions cv ON cv.id = pc.credential_version_id AND cv.provider_id = pr.provider_id WHERE pr.id = $1 AND pc.slot_id = ANY($2) AND cv.revoked_at IS NULL")
            .bind(runtime_provider.revision_id).bind(&slot_ids).fetch_all(pool).await?
            .into_iter()
            .filter_map(|row| row.credential_id.map(|credential| (credential, row)))
            .collect();
        for slot in slots {
            // The release binds each slot to one credential version; a slot whose
            // version is missing or revoked cannot serve this generation.
            let row = slot
                .credential_version_id
                .and_then(|version| rows.remove(&version))
                .ok_or(Error::InvalidCredential)?;
            records.push(runtime_provider_configuration_from_row(row)?);
        }
    }
    Ok(records)
}

pub async fn media_job_runtime_provider_configuration(
    pool: &sqlx::PgPool,
    snapshot: &Snapshot,
    provider_id: ProviderId,
    provider_revision_id: Uuid,
    credential_version_id: Option<Uuid>,
) -> Result<RuntimeProvider, Error> {
    let runtime_provider = snapshot
        .providers
        .get(&provider_id)
        .ok_or(Error::InvalidCredential)?;
    let expected_credential = runtime_provider
        .active_credential
        .map(|credential| credential.as_uuid());
    let row = sqlx::query_as::<_, RuntimeProviderRow>(
            "SELECT rpc.provider_id AS id, rpc.provider_revision_id, rpc.kind, rpc.endpoint, rpc.cloud_region, \
                    rpc.cloud_project, rpc.deployment, rpc.api_version, rpc.auth_mode, rpc.options, \
                    cv.id AS credential_id, cv.version AS credential_version, \
                    cv.ciphertext, cv.nonce, cv.master_key_version \
             FROM runtime_generation_provider_configs rpc \
             LEFT JOIN provider_credential_versions cv \
               ON cv.id = COALESCE($5::uuid,rpc.active_credential_version_id) \
             WHERE rpc.provider_id = $1 AND rpc.runtime_generation_id = $2 \
               AND rpc.provider_revision_id = $3 \
               AND rpc.active_credential_version_id IS NOT DISTINCT FROM $4 \
               AND ($5::uuid IS NULL OR EXISTS(SELECT 1 FROM provider_revision_credentials pc WHERE pc.provider_revision_id=rpc.provider_revision_id AND pc.credential_version_id=$5)) \
               AND (rpc.active_credential_version_id IS NULL OR cv.id IS NOT NULL)",
        )
        .bind(provider_id.as_uuid())
        .bind(snapshot.generation.id.as_uuid())
        .bind(provider_revision_id)
        .bind(expected_credential)
    .bind(credential_version_id)
        .fetch_optional(pool)
        .await?
        .ok_or(Error::InvalidCredential)?;
    let stored_kind = Ok::<_, Error>(row.configuration.kind)?;
    if stored_kind != runtime_provider.kind {
        return Err(Error::InvalidCredential);
    }
    runtime_provider_configuration_from_row(row)
}

pub async fn runtime_provider_authority_is_current(
    pool: &sqlx::PgPool,
    runtime_generation_id: Uuid,
    provider_id: Uuid,
    provider_revision_id: Uuid,
) -> Result<bool, Error> {
    Ok(sqlx::query_scalar::<_, bool>(
        "SELECT EXISTS ( \
               SELECT 1 FROM runtime_generation_provider_configs historical \
               JOIN providers provider ON provider.id = historical.provider_id \
               JOIN provider_revisions current ON current.id = provider.active_revision_id \
               WHERE historical.runtime_generation_id = $1 \
                 AND historical.provider_id = $2 \
                 AND historical.provider_revision_id = $3 \
                 AND provider.state <> 'disabled'::provider_state \
                 AND historical.kind IS NOT DISTINCT FROM current.kind \
                 AND historical.endpoint IS NOT DISTINCT FROM current.endpoint \
                 AND historical.cloud_region IS NOT DISTINCT FROM current.cloud_region \
                 AND historical.cloud_project IS NOT DISTINCT FROM current.cloud_project \
                 AND historical.deployment IS NOT DISTINCT FROM current.deployment \
                 AND historical.api_version IS NOT DISTINCT FROM current.api_version \
                 AND ($4::jsonb || historical.options) - 'limits' \
                     IS NOT DISTINCT FROM ($4::jsonb || current.options) - 'limits' \
                 AND historical.auth_mode IS NOT DISTINCT FROM current.auth_mode \
                 AND historical.active_credential_version_id \
                     IS NOT DISTINCT FROM current.credential_version_id \
             )",
    )
    .bind(runtime_generation_id)
    .bind(provider_id)
    .bind(provider_revision_id)
    .bind(sqlx::types::Json(
        crate::providers::options::ConnectionOptions::default(),
    ))
    .fetch_one(pool)
    .await?)
}

#[derive(Debug, FromRow)]
struct RuntimeProviderRow {
    id: Uuid,
    provider_revision_id: Uuid,
    #[sqlx(flatten)]
    configuration: crate::providers::configuration::ProviderConfiguration,

    credential_id: Option<Uuid>,
    credential_version: Option<i32>,
    ciphertext: Option<Vec<u8>>,
    nonce: Option<Vec<u8>>,
    master_key_version: Option<i32>,
}

fn runtime_provider_configuration_from_row(
    row: RuntimeProviderRow,
) -> Result<RuntimeProvider, Error> {
    let credential_id: Option<Uuid> = row.credential_id;
    let credential_version = row.credential_version.map(stored_version).transpose()?;
    let nonce = row.nonce;
    let ciphertext = row.ciphertext;
    let key_version = row.master_key_version.map(stored_version).transpose()?;
    let encrypted = match (nonce, ciphertext, key_version) {
        (Some(nonce), Some(ciphertext), Some(key_version)) => Some(EncryptedSecret {
            key_version,
            nonce: nonce.try_into().map_err(|_| Error::InvalidCredential)?,
            ciphertext,
        }),
        (None, None, None) => None,
        _ => return Err(Error::InvalidCredential),
    };
    if credential_id.is_some() != credential_version.is_some()
        || credential_id.is_some() != encrypted.is_some()
    {
        return Err(Error::InvalidCredential);
    }
    Ok(RuntimeProvider {
        provider_id: ProviderId::from_uuid(row.id),
        provider_revision_id: row.provider_revision_id,
        configuration: row.configuration,

        credential_id,
        credential_version,
        encrypted_credential: encrypted,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::crypto::{aad::credential, envelope::MasterKey};
    use crate::database::idempotency::{Outcome, Replayable, Response, fingerprint};
    use crate::providers::{
        configuration::ProviderConfiguration, lifecycle::NewProviderDraft,
        runtime_model::ProviderKind, types::ProviderAuthMode,
    };

    #[tokio::test]
    #[ignore = "requires PostgreSQL via make integration"]
    async fn published_named_slots_do_not_load_the_disabled_default_secret() {
        let db = crate::test_support::TestDb::create_migrated("disabled_default_runtime").await;
        let pool = db.pool(3).await;
        let actor = Uuid::now_v7();
        sqlx::query("INSERT INTO users(id,email,display_name,role) VALUES($1,'runtime-pool@test.example','Owner','owner')")
            .bind(actor).execute(&pool).await.unwrap();
        let master = MasterKey::new(1, [47; 32]);
        let id = Uuid::now_v7();
        let default_secret = Uuid::now_v7();
        let model = Uuid::now_v7();
        let mut configuration = ProviderConfiguration::new(ProviderKind::OpenAiCompatible);
        configuration.endpoint = Some("https://api.example.test/v1".into());
        configuration.auth_mode = ProviderAuthMode::Headers;
        configuration
            .options
            .credential_headers
            .insert("authorization".into());
        let created = crate::providers::lifecycle::create_provider_draft(
            &pool,
            &Default::default(),
            NewProviderDraft {
                provider_id: id,
                credential_id: Some(default_secret),
                model_id: Some(model),
                name: "named-slot-runtime".into(),
                configuration: configuration.clone(),
                connector_ready: true,
                credential: Some(
                    master
                        .seal(
                            br#"{"authorization":"Bearer old"}"#,
                            &credential(id, default_secret, 1),
                        )
                        .unwrap(),
                ),
                model: Some("test-model".into()),
                display_name: Some("Test model".into()),
                model_enabled: true,
                surface: Some("openai".parse().unwrap()),
                actor,
                idempotency_key: "named-slot-runtime-create".into(),
            },
            Replayable::new(fingerprint(&"named-slot-runtime-create").unwrap(), &master),
            |_| Response::new(201, None, None, Vec::new()),
        )
        .await
        .unwrap();
        let Outcome::Executed { value: created, .. } = created else {
            panic!("Fixture creation must execute");
        };
        assert!(matches!(
            crate::providers::credentials::revoke_provider_credential(
                &pool,
                &Default::default(),
                id,
                default_secret,
                created.etag,
                actor,
                "revoke-enabled-default",
            )
            .await,
            Err(Error::InUse)
        ));
        configuration.options.credential_headers =
            std::collections::BTreeSet::from(["x-api-key".into()]);
        sqlx::query("UPDATE providers SET options=$2 WHERE id=$1")
            .bind(id)
            .bind(sqlx::types::Json(&configuration.options))
            .execute(&pool)
            .await
            .unwrap();
        sqlx::query("UPDATE provider_credential_slots SET enabled=false WHERE provider_id=$1")
            .bind(id)
            .execute(&pool)
            .await
            .unwrap();
        let slot = Uuid::now_v7();
        let named_secret = Uuid::now_v7();
        sqlx::query("INSERT INTO provider_credential_slots(id,provider_id,name) VALUES($1,$2,'Enabled account')")
            .bind(slot).bind(id).execute(&pool).await.unwrap();
        let encrypted = master
            .seal(br#"{"x-api-key":"new"}"#, &credential(id, named_secret, 2))
            .unwrap();
        sqlx::query("INSERT INTO provider_credential_versions(id,provider_id,slot_id,version,ciphertext,nonce,master_key_version,created_by) VALUES($1,$2,$3,2,$4,$5,1,$6)")
            .bind(named_secret).bind(id).bind(slot).bind(encrypted.ciphertext)
            .bind(encrypted.nonce.to_vec()).bind(actor).execute(&pool).await.unwrap();
        sqlx::query("UPDATE model_capabilities SET source='certified',certified_at=now() WHERE provider_model_id=$1")
            .bind(model).execute(&pool).await.unwrap();
        sqlx::query("UPDATE provider_credential_slots SET selected_version_id=$2,validated_at=now() WHERE id=$1")
            .bind(slot).bind(named_secret).execute(&pool).await.unwrap();
        crate::providers::repository::record_provider_probe(
            &pool,
            &Default::default(),
            id,
            created.etag,
            true,
            "Named credential validated",
            actor,
        )
        .await
        .unwrap();
        let activated = crate::providers::lifecycle::activate_provider(
            &pool,
            &Default::default(),
            id,
            created.etag,
            actor,
            "named-slot-runtime-activate",
        )
        .await
        .unwrap();
        let payload: Vec<u8> =
            sqlx::query_scalar("SELECT compiled_release FROM runtime_generations WHERE id=$1")
                .bind(activated.release.generation_id)
                .fetch_one(&pool)
                .await
                .unwrap();
        let mut snapshot = Snapshot::from_persisted_slice(&payload).unwrap();
        assert!(matches!(
            crate::providers::credentials::revoke_provider_credential(
                &pool,
                &Default::default(),
                id,
                named_secret,
                activated.etag,
                actor,
                "revoke-enabled-named-slot",
            )
            .await,
            Err(Error::InUse)
        ));
        let mut etag = activated.etag;
        for revoke_default in [false, true] {
            if revoke_default {
                etag = crate::providers::credentials::revoke_provider_credential(
                    &pool,
                    &Default::default(),
                    id,
                    default_secret,
                    etag,
                    actor,
                    "revoke-disabled-default",
                )
                .await
                .unwrap();
            }
            let credentials =
                crate::providers::credentials::list_provider_credentials(&pool, id, None, 10)
                    .await
                    .unwrap();
            let default = credentials
                .items
                .iter()
                .find(|item| item.id == default_secret)
                .unwrap();
            assert!(!default.active);
            assert!(!default.draft_selected);
            assert_eq!(default.revoked_at.is_some(), revoke_default);
            let named = credentials
                .items
                .iter()
                .find(|item| item.id == named_secret)
                .unwrap();
            assert!(named.active);
            let records = runtime_provider_configurations(&pool, &snapshot)
                .await
                .unwrap();
            assert_eq!(records.len(), 1);
            assert_eq!(records[0].credential_id, Some(named_secret));
            let mut transports = std::collections::BTreeMap::new();
            crate::providers::runtime_config::load_runtime_transports(
                &records,
                Some(&master),
                &snapshot,
                &mut transports,
                &crate::net::egress::EgressPolicy::default(),
                Default::default(),
                &Default::default(),
            )
            .await
            .unwrap();
            assert!(transports.contains_key(&ProviderId::from_uuid(id)));
        }
        // A disabled revoked default must not prevent the next valid pool activation.
        sqlx::query("UPDATE providers SET state='draft' WHERE id=$1")
            .bind(id)
            .execute(&pool)
            .await
            .unwrap();
        crate::providers::repository::record_provider_probe(
            &pool,
            &Default::default(),
            id,
            etag,
            true,
            "Named credential revalidated",
            actor,
        )
        .await
        .unwrap();
        crate::providers::lifecycle::activate_provider(
            &pool,
            &Default::default(),
            id,
            etag,
            actor,
            "reactivate-with-revoked-default",
        )
        .await
        .unwrap();
        let mut disabled = snapshot.clone();
        disabled
            .routing
            .credentials
            .get_mut(&ProviderId::from_uuid(id))
            .unwrap()
            .clear();
        let records = runtime_provider_configurations(&pool, &disabled)
            .await
            .unwrap();
        assert!(records.is_empty());
        let mut transports = std::collections::BTreeMap::new();
        crate::providers::runtime_config::load_runtime_transports(
            &records,
            Some(&master),
            &disabled,
            &mut transports,
            &crate::net::egress::EgressPolicy::default(),
            Default::default(),
            &Default::default(),
        )
        .await
        .unwrap();
        assert!(
            crate::runtime::manager::Manager::empty()
                .install(disabled, transports)
                .unwrap()
        );
        snapshot
            .routing
            .credentials
            .remove(&ProviderId::from_uuid(id));
        assert!(
            runtime_provider_configurations(&pool, &snapshot)
                .await
                .is_err()
        );
        sqlx::query("UPDATE provider_credential_versions SET revoked_at=NULL WHERE id=$1")
            .bind(default_secret)
            .execute(&pool)
            .await
            .unwrap();
        let legacy = runtime_provider_configurations(&pool, &snapshot)
            .await
            .unwrap();
        assert_eq!(legacy.len(), 1);
        assert_eq!(legacy[0].credential_id, Some(default_secret));
    }
}
