use super::*;
pub(crate) async fn prove_three_worker_recovery(
    world: &World,
    workers: &[harness::WorkerProcess; 3],
) -> Result<(), String> {
    let response = world
        .gateway_post(
            "/openai/v1/chat/completions",
            json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "owned metadata replay"}]
            }),
            &world.api_key,
        )
        .await?;
    crate::require!(
        response.status == 200,
        "request used to create owned metadata failed with {}: {}",
        response.status,
        response.text
    );
    await_file(&workers[0].ownership_marker, Duration::from_secs(30)).await?;
    await_outbox_pending(world, 0, Duration::from_secs(15)).await?;
    world.hard_kill_worker(&workers[0]).await?;

    world.release_worker(&workers[1]).await?;
    await_metadata_recovery(world, &world.api_key_id, Duration::from_secs(45)).await?;

    let crash_second = async {
        await_file(&workers[1].ownership_marker, Duration::from_secs(30)).await?;
        world.hard_kill_worker(&workers[1]).await?;
        world.release_worker(&workers[2]).await?;
        Ok::<(), String>(())
    };
    let (takeover_key, crash_result) = tokio::join!(
        world.issue_key("outbox takeover after hard termination", json!({})),
        crash_second
    );
    crash_result?;
    let takeover_key = takeover_key?;

    await_healthy_recovered_workers(world, Duration::from_secs(30)).await?;
    let replayed_rows = world
        .await_request_rows(&world.api_key_id, &format!("&route={OPENAI_ROUTE}"), 1)
        .await?;
    crate::require!(
        replayed_rows.len() == 1,
        "replay created {} logical request rows instead of one",
        replayed_rows.len()
    );
    crate::require!(
        usage_facts_for_key(&world.database_url, &world.api_key_id).await? == 1,
        "replay did not preserve exactly one logical usage fact"
    );

    let continued = world
        .gateway_post(
            "/openai/v1/chat/completions",
            json!({
                "model": OPENAI_ROUTE,
                "messages": [{"role": "user", "content": "work after recovery"}]
            }),
            &takeover_key.secret,
        )
        .await?;
    crate::require!(
        continued.status == 200,
        "surviving worker topology stopped serving new work: {} {}",
        continued.status,
        continued.text
    );
    world
        .await_request_rows(&takeover_key.id, &format!("&route={OPENAI_ROUTE}"), 1)
        .await?;
    crate::require!(
        usage_facts_for_key(&world.database_url, &takeover_key.id).await? == 1,
        "new work after recovery did not produce exactly one usage fact"
    );
    await_healthy_recovered_workers(world, Duration::from_secs(15)).await
}

pub(crate) async fn await_file(path: &std::path::Path, timeout: Duration) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    loop {
        if path.exists() {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "worker did not reach ownership boundary {} within {timeout:?}",
                path.display()
            ));
        }
        tokio::time::sleep(Duration::from_millis(25)).await;
    }
}

pub(crate) async fn await_outbox_pending(
    world: &World,
    expected: i64,
    timeout: Duration,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    let mut database = sqlx::PgConnection::connect(&world.database_url)
        .await
        .map_err(|error| format!("failed to inspect outbox pending state: {error}"))?;
    loop {
        let pending: i64 = sqlx::query_scalar(
            "SELECT count(*)::bigint FROM transactional_outbox WHERE published_at IS NULL",
        )
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read outbox pending state: {error}"))?;
        if pending == expected {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "runtime outbox pending count stayed at {pending}, expected {expected}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
}

pub(crate) async fn await_metadata_recovery(
    world: &World,
    api_key_id: &str,
    timeout: Duration,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    let mut database = sqlx::PgConnection::connect(&world.database_url)
        .await
        .map_err(|error| format!("failed to inspect metadata recovery: {error}"))?;
    loop {
        let (usage_facts, recovered, pending, lag): (i64, i64, Option<i64>, Option<i64>) =
            sqlx::query_as(
                "SELECT \
                   (SELECT count(*)::bigint FROM usage_facts WHERE api_key_id = $1::uuid), \
                   request_metadata_recovered_total, \
                   (SELECT pending_events FROM request_metadata_consumer_health WHERE singleton), \
                   (SELECT lag_events FROM request_metadata_consumer_health WHERE singleton) \
                 FROM async_worker_counters WHERE singleton",
            )
            .bind(api_key_id)
            .fetch_one(&mut database)
            .await
            .map_err(|error| format!("failed to read metadata recovery state: {error}"))?;
        if usage_facts == 1 && recovered >= 1 && pending == Some(0) && lag == Some(0) {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "metadata recovery did not converge: usage={usage_facts}, recovered={recovered}, pending={pending:?}, lag={lag:?}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

pub(crate) async fn await_healthy_recovered_workers(
    world: &World,
    timeout: Duration,
) -> Result<(), String> {
    let deadline = Instant::now() + timeout;
    let mut database = sqlx::PgConnection::connect(&world.database_url)
        .await
        .map_err(|error| format!("failed to inspect worker health: {error}"))?;
    loop {
        let state: (i64, i64, i64, i64, i64, bool, i64, Option<i64>, Option<i64>) = sqlx::query_as(
            "SELECT \
                   (SELECT count(*)::bigint FROM transactional_outbox WHERE published_at IS NULL), \
                   runtime_outbox_repeated_attempts_total, \
                   runtime_outbox_abandoned_ownership_total, \
                   runtime_outbox_abandoned_claims_total, \
                   runtime_outbox_published_total, \
                   (SELECT owner_active FROM runtime_outbox_health WHERE singleton), \
                   (SELECT claimed_rows FROM runtime_outbox_health WHERE singleton), \
                   (SELECT pending_events FROM request_metadata_consumer_health WHERE singleton), \
                   (SELECT lag_events FROM request_metadata_consumer_health WHERE singleton) \
                 FROM async_worker_counters WHERE singleton",
        )
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read worker recovery state: {error}"))?;
        let healthy_tasks: i64 = sqlx::query_scalar(
            "SELECT count(*)::bigint FROM worker_task_health \
             WHERE last_success_at IS NOT NULL AND \
               last_success_at >= clock_timestamp() - \
                 CASE WHEN task IN ('maintenance', 'cost_reconciliation') \
                      THEN interval '180 seconds' ELSE interval '20 seconds' END",
        )
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read worker checkpoints: {error}"))?;
        if state.0 == 0
            && state.1 >= 1
            && state.2 >= 1
            && state.3 >= 1
            && state.4 >= 1
            && state.5
            && state.6 == 0
            && state.7 == Some(0)
            && state.8 == Some(0)
            && healthy_tasks == WorkerTask::ALL.len() as i64
        {
            return Ok(());
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "worker recovery did not converge: state={state:?}, healthy_tasks={healthy_tasks}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

pub(crate) async fn usage_facts_for_key(
    database_url: &str,
    api_key_id: &str,
) -> Result<i64, String> {
    let mut database = sqlx::PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect for usage assertion: {error}"))?;
    sqlx::query_scalar("SELECT count(*)::bigint FROM usage_facts WHERE api_key_id = $1::uuid")
        .bind(api_key_id)
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to count logical usage facts: {error}"))
}

/// The processed counter is flushed after the acknowledgement that empties the
/// stream (`report_processing_activity` in `olp-db`), so an empty stream alone
/// does not prove the counter has caught up; require one repeat observation.
pub(crate) async fn await_metadata_quiescence(
    world: &World,
    timeout: Duration,
) -> Result<i64, String> {
    let deadline = Instant::now() + timeout;
    let mut database = sqlx::PgConnection::connect(&world.database_url)
        .await
        .map_err(|error| format!("failed to inspect metadata quiescence: {error}"))?;
    let mut settled: Option<i64> = None;
    loop {
        let (processed, pending, lag): (i64, Option<i64>, Option<i64>) = sqlx::query_as(
            "SELECT request_metadata_processed_total, \
               (SELECT pending_events FROM request_metadata_consumer_health WHERE singleton), \
               (SELECT lag_events FROM request_metadata_consumer_health WHERE singleton) \
             FROM async_worker_counters WHERE singleton",
        )
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("failed to read metadata consumer health: {error}"))?;
        if pending == Some(0) && lag == Some(0) {
            if settled == Some(processed) {
                return Ok(processed);
            }
            settled = Some(processed);
        } else {
            settled = None;
        }
        if Instant::now() >= deadline {
            return Err(format!(
                "installation did not drain its own request metadata: \
                 processed={processed}, pending={pending:?}, lag={lag:?}"
            ));
        }
        tokio::time::sleep(Duration::from_millis(200)).await;
    }
}

pub(crate) async fn metadata_processed(database_url: &str) -> Result<i64, String> {
    database_scalar(
        database_url,
        "SELECT request_metadata_processed_total FROM async_worker_counters WHERE singleton",
    )
    .await
}

pub(crate) async fn usage_fact_count(database_url: &str) -> Result<i64, String> {
    database_scalar(database_url, "SELECT count(*)::bigint FROM usage_facts").await
}

pub(crate) async fn database_scalar(
    database_url: &str,
    query: &'static str,
) -> Result<i64, String> {
    let mut database = sqlx::PgConnection::connect(database_url)
        .await
        .map_err(|error| format!("failed to connect to installation database: {error}"))?;
    sqlx::query_scalar(query)
        .fetch_one(&mut database)
        .await
        .map_err(|error| format!("database assertion failed: {error}"))
}
