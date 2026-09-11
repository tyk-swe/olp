//! docs/architecture.md: Runtime activation and API-key authority.

use super::*;

pub(crate) async fn exercise(world: &World, gateway: &GatewayProcess) -> Result<(), String> {
    let public = [world.public_origin.as_str(), gateway.public_origin.as_str()];
    let observability = [
        world.observability_base.as_str(),
        gateway.observability_base.as_str(),
    ];
    let http = reqwest::Client::builder()
        .timeout(Duration::from_secs(5))
        .build()
        .map_err(|error| format!("failed to build authority client: {error}"))?;
    for invalid_transport in [false, true] {
        let name = if invalid_transport {
            "unbuildable transport"
        } else {
            "corrupt release"
        };
        let key = convergence::issue_key(
            world,
            &http,
            public[1],
            name,
            json!({
                "scopes": ["models_read"], "allowed_routes": [OPENAI_ROUTE]
            }),
        )
        .await?;
        for origin in observability {
            await_generation(&http, origin, key.generation).await?;
            await_desired_generation(&http, origin, key.generation).await?;
        }
        let rejected = publish_rejected_authority(world, &key, invalid_transport).await?;
        convergence::await_keys(&http, &public, &key, 401, Duration::from_millis(5_500)).await?;
        for (public, observability) in public.iter().zip(observability) {
            await_desired_generation(&http, observability, rejected).await?;
            let installed = generation(&http, observability).await?;
            crate::require!(
                installed == key.generation && installed < rejected,
                "{name}: authority changed only after installing rejected generation {rejected}; installed {installed}"
            );
            crate::require!(
                convergence::gateway_status(&http, public, &world.api_key, None).await? == 200,
                "{name}: refreshing key authority interrupted retained routing"
            );
        }
    }
    Ok(())
}

async fn generation(http: &reqwest::Client, origin: &str) -> Result<i64, String> {
    let response = http
        .get(format!("{origin}/health/ready"))
        .send()
        .await
        .map_err(|error| format!("authority readiness request failed: {error}"))?;
    crate::require!(
        response.status().is_success(),
        "authority readiness was unavailable"
    );
    let body = response
        .json::<Value>()
        .await
        .map_err(|error| format!("authority readiness response was invalid: {error}"))?;
    body["generation"]
        .as_i64()
        .ok_or_else(|| "authority readiness had no generation".into())
}

/// Polls `probe` until it reports the awaited state, failing with `failure`
/// after ten seconds.
async fn await_state<F, Fut>(mut probe: F, failure: impl Fn() -> String) -> Result<(), String>
where
    F: FnMut() -> Fut,
    Fut: Future<Output = Result<bool, String>>,
{
    let deadline = Instant::now() + Duration::from_secs(10);
    loop {
        if probe().await? {
            return Ok(());
        }
        crate::require!(Instant::now() < deadline, "{}", failure());
        tokio::time::sleep(Duration::from_millis(50)).await;
    }
}

async fn await_generation(
    http: &reqwest::Client,
    origin: &str,
    expected: i64,
) -> Result<(), String> {
    await_state(
        || async { Ok(generation(http, origin).await? == expected) },
        || format!("{origin} did not install fixture generation {expected}"),
    )
    .await
}

async fn await_desired_generation(
    http: &reqwest::Client,
    origin: &str,
    expected: i64,
) -> Result<(), String> {
    let expected_metric = format!("olp_runtime_desired_generation {expected}");
    await_state(
        || async {
            let body = http
                .get(format!("{origin}/metrics"))
                .send()
                .await
                .and_then(reqwest::Response::error_for_status)
                .map_err(|error| format!("authority metrics request failed: {error}"))?
                .text()
                .await
                .map_err(|error| format!("authority metrics response was invalid: {error}"))?;
            Ok(body.lines().any(|line| line == expected_metric))
        },
        || format!("{origin} did not report desired generation {expected}"),
    )
    .await
}

async fn publish_rejected_authority(
    world: &World,
    key: &IssuedKey,
    invalid_transport: bool,
) -> Result<i64, String> {
    let mut connection = sqlx::postgres::PgConnection::connect(&world.database_url)
        .await
        .map_err(|error| format!("authority fixture connection failed: {error}"))?;
    let mut transaction = connection
        .begin()
        .await
        .map_err(|error| format!("authority fixture transaction failed: {error}"))?;
    sqlx::query("SELECT pg_advisory_xact_lock($1)")
        .bind(olp::runtime::publication::compiler::PUBLICATION_LOCK_ID)
        .execute(&mut *transaction)
        .await
        .map_err(|error| format!("authority fixture publication lock failed: {error}"))?;
    let sequence =
        clone_rejected_release(&mut transaction, key.generation, invalid_transport).await?;
    let key_id = uuid::Uuid::parse_str(&key.id)
        .map_err(|error| format!("authority fixture key ID was invalid: {error}"))?;
    sqlx::query("UPDATE api_keys SET revoked_at = now() WHERE id = $1")
        .bind(key_id)
        .execute(&mut *transaction)
        .await
        .map_err(|error| format!("authority fixture revocation failed: {error}"))?;
    transaction
        .commit()
        .await
        .map_err(|error| format!("authority fixture commit failed: {error}"))?;
    Ok(sequence)
}

async fn clone_rejected_release(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    source_sequence: i64,
    invalid_transport: bool,
) -> Result<i64, String> {
    let (source_id, payload, actor): (uuid::Uuid, Vec<u8>, uuid::Uuid) = sqlx::query_as(
        "SELECT id, compiled_release, created_by FROM runtime_generations WHERE sequence = $1",
    )
    .bind(source_sequence)
    .fetch_one(&mut **transaction)
    .await
    .map_err(|error| format!("authority fixture source release read failed: {error}"))?;
    let mut snapshot = olp::runtime::snapshot::Snapshot::from_persisted_slice(&payload)
        .map_err(|error| format!("authority fixture source release was invalid: {error}"))?;
    let id = uuid::Uuid::now_v7();
    let sequence: i64 = sqlx::query_scalar(
        "INSERT INTO runtime_generations (id, compiled_release, release_sha256, created_by) \
         VALUES ($1, $2, $3, $4) RETURNING sequence",
    )
    .bind(id)
    .bind(payload)
    .bind(vec![0_u8; 32])
    .bind(actor)
    .fetch_one(&mut **transaction)
    .await
    .map_err(|error| format!("authority fixture release insertion failed: {error}"))?;
    snapshot.generation.id = olp::ids::RuntimeGenerationId::from_uuid(id);
    snapshot.generation.activated_at = chrono::Utc::now();
    snapshot.generation.ordinal = u64::try_from(sequence)
        .map_err(|error| format!("authority fixture sequence was invalid: {error}"))?;
    let payload = snapshot
        .to_persisted_vec()
        .map_err(|error| format!("authority fixture serialization failed: {error}"))?;
    sqlx::query(
        "UPDATE runtime_generations SET compiled_release = $1, \
         release_sha256 = CASE WHEN $2 THEN sha256($1) ELSE decode(repeat('00', 32), 'hex') END \
         WHERE id = $3",
    )
    .bind(payload)
    .bind(invalid_transport)
    .bind(id)
    .execute(&mut **transaction)
    .await
    .map_err(|error| format!("authority fixture release finalization failed: {error}"))?;
    if invalid_transport {
        copy_unbuildable_transports(transaction, source_id, id).await?;
    }
    Ok(sequence)
}

async fn copy_unbuildable_transports(
    transaction: &mut sqlx::Transaction<'_, sqlx::Postgres>,
    source_id: uuid::Uuid,
    candidate_id: uuid::Uuid,
) -> Result<(), String> {
    sqlx::query(
        "INSERT INTO runtime_generation_provider_configs \
         (runtime_generation_id, provider_id, kind, endpoint, cloud_region, cloud_project, \
          deployment, api_version, auth_mode, active_credential_version_id, provider_revision_id) \
         SELECT $1, provider_id, kind, 'not-a-url', cloud_region, cloud_project, deployment, \
                api_version, auth_mode, active_credential_version_id, provider_revision_id \
         FROM runtime_generation_provider_configs WHERE runtime_generation_id = $2",
    )
    .bind(candidate_id)
    .bind(source_id)
    .execute(&mut **transaction)
    .await
    .map_err(|error| format!("authority fixture sidecar copy failed: {error}"))?;
    Ok(())
}
