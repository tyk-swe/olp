use crate::{
    configuration::{error::Error, validation::stored_version},
    error::Error as PersistenceError,
    security::envelope::EncryptedSecret,
    store::Store,
};
use olp_engine::domain::{
    ids::ProviderId,
    provider::ProviderAuthMode,
    routing::{provider::ProviderKind, snapshot::Snapshot},
};
use sqlx::FromRow;
use uuid::Uuid;

#[derive(Debug, Clone)]
pub struct RuntimeProvider {
    pub provider_id: ProviderId,
    pub provider_revision_id: Option<Uuid>,
    pub kind: ProviderKind,
    pub endpoint: Option<String>,
    pub cloud_region: Option<String>,
    pub cloud_project: Option<String>,
    pub deployment: Option<String>,
    pub api_version: Option<String>,
    pub auth_mode: ProviderAuthMode,
    pub credential_id: Option<Uuid>,
    pub credential_version: Option<u32>,
    pub encrypted_credential: Option<EncryptedSecret>,
}

impl Store {
    /// Loads the release-exact connector configuration and credential named by
    /// a verified runtime sidecar. Mutable configuration drafts are deliberately not
    /// consulted, so testing a replacement endpoint or credential cannot alter
    /// the transport used by the last activated provider revision.
    pub async fn runtime_provider_configurations(
        &self,
        snapshot: &Snapshot,
    ) -> Result<Vec<RuntimeProvider>, Error> {
        let mut records = Vec::with_capacity(snapshot.providers.len());
        for runtime_provider in snapshot.providers.values() {
            let expected_credential = runtime_provider
                .active_credential
                .map(|credential| credential.as_uuid());
            let row = sqlx::query_as!(
                RuntimeProviderRow,
                "SELECT rpc.provider_id AS id, rpc.provider_revision_id, rpc.kind, rpc.endpoint, rpc.cloud_region, \
                        rpc.cloud_project, rpc.deployment, rpc.api_version, rpc.auth_mode, \
                        cv.id AS \"credential_id?\", cv.version AS \"credential_version?\", \
                        cv.ciphertext AS \"ciphertext?\", cv.nonce AS \"nonce?\", \
                        cv.master_key_version AS \"master_key_version?\" \
                 FROM runtime_generation_provider_configs rpc \
                 JOIN providers p ON p.id = rpc.provider_id \
                 LEFT JOIN provider_credential_versions cv \
                   ON cv.id = rpc.active_credential_version_id AND cv.revoked_at IS NULL \
                 WHERE rpc.provider_id = $1 AND rpc.runtime_generation_id = $3 \
                   AND rpc.active_credential_version_id IS NOT DISTINCT FROM $2 \
                   AND p.active_revision_id IS NOT NULL \
                   AND p.state <> 'disabled'::provider_state \
                   AND (rpc.active_credential_version_id IS NULL OR cv.id IS NOT NULL)",
                runtime_provider.id.as_uuid(),
                expected_credential,
                snapshot.generation.id.as_uuid()
            )
            .fetch_optional(self.pool())
            .await?
            .ok_or(Error::InvalidCredential)?;
            let stored_kind = parse_provider_kind(row.kind.as_str())?;
            if stored_kind != runtime_provider.kind
                || runtime_provider
                    .revision_id
                    .is_some_and(|revision| Some(revision) != row.provider_revision_id)
            {
                return Err(Error::InvalidCredential);
            }
            records.push(runtime_provider_configuration_from_row(row)?);
        }
        Ok(records)
    }

    pub async fn media_job_runtime_provider_configuration(
        &self,
        snapshot: &Snapshot,
        provider_id: ProviderId,
        provider_revision_id: Uuid,
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
                    rpc.cloud_project, rpc.deployment, rpc.api_version, rpc.auth_mode, \
                    cv.id AS credential_id, cv.version AS credential_version, \
                    cv.ciphertext, cv.nonce, cv.master_key_version \
             FROM runtime_generation_provider_configs rpc \
             LEFT JOIN provider_credential_versions cv \
               ON cv.id = rpc.active_credential_version_id \
             WHERE rpc.provider_id = $1 AND rpc.runtime_generation_id = $2 \
               AND rpc.provider_revision_id = $3 \
               AND rpc.active_credential_version_id IS NOT DISTINCT FROM $4 \
               AND (rpc.active_credential_version_id IS NULL OR cv.id IS NOT NULL)",
        )
        .bind(provider_id.as_uuid())
        .bind(snapshot.generation.id.as_uuid())
        .bind(provider_revision_id)
        .bind(expected_credential)
        .fetch_optional(self.pool())
        .await?
        .ok_or(Error::InvalidCredential)?;
        let stored_kind = parse_provider_kind(&row.kind)?;
        if stored_kind != runtime_provider.kind {
            return Err(Error::InvalidCredential);
        }
        runtime_provider_configuration_from_row(row)
    }

    pub async fn runtime_provider_authority_is_current(
        &self,
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
                 AND historical.auth_mode IS NOT DISTINCT FROM current.auth_mode \
                 AND historical.active_credential_version_id \
                     IS NOT DISTINCT FROM current.credential_version_id \
             )",
        )
        .bind(runtime_generation_id)
        .bind(provider_id)
        .bind(provider_revision_id)
        .fetch_one(self.pool())
        .await?)
    }
}

#[derive(Debug, FromRow)]
struct RuntimeProviderRow {
    id: Uuid,
    provider_revision_id: Option<Uuid>,
    kind: String,
    endpoint: Option<String>,
    cloud_region: Option<String>,
    cloud_project: Option<String>,
    deployment: Option<String>,
    api_version: Option<String>,
    auth_mode: String,
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
        kind: parse_provider_kind(row.kind.as_str())?,
        endpoint: row.endpoint,
        cloud_region: row.cloud_region,
        cloud_project: row.cloud_project,
        deployment: row.deployment,
        api_version: row.api_version,
        auth_mode: row.auth_mode.parse().map_err(|_| {
            PersistenceError::InvalidStoredValue("runtime provider authentication mode")
        })?,
        credential_id,
        credential_version,
        encrypted_credential: encrypted,
    })
}

fn parse_provider_kind(value: &str) -> Result<ProviderKind, Error> {
    value.parse().map_err(|_| Error::InvalidCredential)
}
