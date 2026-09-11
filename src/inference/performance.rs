//! Shared, content-free performance snapshots. A request reads one immutable view.
use crate::protocols::canonical::identity::{OperationKind, TransportMode};
use serde::Serialize;
use std::collections::BTreeMap;
use std::sync::{Arc, LazyLock, RwLock};
use std::time::{Duration, Instant};
use utoipa::ToSchema;

type Key = (uuid::Uuid, OperationKind, TransportMode);
#[derive(Clone, Debug, Serialize, ToSchema)]
pub struct Measurement {
    pub latency_ms: u64,
    pub output_tokens_per_second: Option<u64>,
    pub samples: u64,
    pub observed_at: chrono::DateTime<chrono::Utc>,
}
#[derive(Default)]
pub struct Snapshot {
    refreshed: Option<Instant>,
    values: BTreeMap<Key, BTreeMap<String, Measurement>>,
}
static SNAPSHOT: LazyLock<RwLock<Arc<Snapshot>>> =
    LazyLock::new(|| RwLock::new(Arc::new(Snapshot::default())));

/// The current view, or an empty one once the last refresh is stale. Staleness
/// is judged once per caller; the SQL refresh already drops idle destinations.
pub fn snapshot() -> Arc<Snapshot> {
    let view = Arc::clone(
        &SNAPSHOT
            .read()
            .unwrap_or_else(std::sync::PoisonError::into_inner),
    );
    if view.fresh() {
        view
    } else {
        Arc::new(Snapshot::default())
    }
}
impl Snapshot {
    fn fresh(&self) -> bool {
        self.refreshed
            .is_some_and(|at| at.elapsed() < Duration::from_secs(60))
    }

    pub fn get(
        &self,
        provider: uuid::Uuid,
        model: &str,
        operation: OperationKind,
        mode: TransportMode,
    ) -> Option<&Measurement> {
        self.values.get(&(provider, operation, mode))?.get(model)
    }
}

/// All gateways aggregate persisted attempts over the same five-minute window.
/// Insufficient/stale evidence is unknown. Failed refreshes expire naturally.
pub async fn supervise(pool: sqlx::PgPool, mut shutdown: tokio::sync::watch::Receiver<bool>) {
    let mut interval = tokio::time::interval(Duration::from_secs(10));
    loop {
        tokio::select! {
            result = shutdown.changed() => { if result.is_err() || *shutdown.borrow() { break; } }
            _ = interval.tick() => {
                if let Err(error) = refresh(&pool).await {
                    tracing::debug!(%error, "performance snapshot refresh unavailable");
                }
            }
        }
    }
}

pub(crate) async fn refresh(pool: &sqlx::PgPool) -> Result<(), sqlx::Error> {
    type Row = (
        uuid::Uuid,
        String,
        String,
        String,
        i64,
        Option<i64>,
        i64,
        chrono::DateTime<chrono::Utc>,
    );
    let rows = sqlx::query_as::<_, Row>(
        "WITH measurements AS (
           SELECT a.provider_id, a.upstream_model, r.operation, a.routing->>'mode' AS mode,
                  CASE WHEN a.routing->>'mode' = 'streaming' THEN (a.routing->>'first_output_ms')::bigint ELSE a.latency_ms END AS latency,
                  CASE WHEN a.routing->>'mode' = 'streaming' AND r.operation='generation' AND f.usage_complete
                       AND (a.routing->>'streamed_output_tokens')::bigint > 0 AND a.latency_ms > (a.routing->>'first_output_ms')::bigint
                       THEN (a.routing->>'streamed_output_tokens')::bigint * 1000 / (a.latency_ms - (a.routing->>'first_output_ms')::bigint) END AS throughput,
                  a.completed_at
           FROM attempts a JOIN requests r ON r.id=a.request_id AND r.started_at=a.request_started_at
           LEFT JOIN attempt_usage_facts f ON f.attempt_id=a.id
           WHERE a.started_at > now()-interval '5 minutes' AND a.status_code BETWEEN 200 AND 299 AND a.routing->>'mode' IS NOT NULL
         ) SELECT provider_id,upstream_model,operation,mode,
             percentile_disc(0.5) WITHIN GROUP(ORDER BY latency)::bigint,
             CASE WHEN count(throughput)>=20 THEN percentile_disc(0.5) WITHIN GROUP(ORDER BY throughput)::bigint END,
             count(latency),max(completed_at)
           FROM measurements WHERE latency IS NOT NULL GROUP BY provider_id,upstream_model,operation,mode
           HAVING count(latency)>=20 AND max(completed_at)>now()-interval '60 seconds'
           ORDER BY provider_id,upstream_model,operation,mode LIMIT 10000"
    ).fetch_all(pool).await?;
    let mut values: BTreeMap<Key, BTreeMap<String, Measurement>> = BTreeMap::new();
    for (provider, model, operation, mode, latency, throughput, samples, observed_at) in rows {
        let (Ok(operation), Ok(mode)) = (operation.parse(), mode.parse()) else {
            continue;
        };
        let (Ok(latency_ms), Ok(samples)) = (latency.try_into(), samples.try_into()) else {
            continue;
        };
        values
            .entry((provider, operation, mode))
            .or_default()
            .insert(
                model,
                Measurement {
                    latency_ms,
                    output_tokens_per_second: throughput.and_then(|n| n.try_into().ok()),
                    samples,
                    observed_at,
                },
            );
    }
    *SNAPSHOT
        .write()
        .unwrap_or_else(std::sync::PoisonError::into_inner) = Arc::new(Snapshot {
        refreshed: Some(Instant::now()),
        values,
    });
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn stale_and_other_mode_measurements_are_unknown() {
        let id = uuid::Uuid::new_v4();
        let mut view = Snapshot {
            refreshed: Some(Instant::now()),
            values: BTreeMap::from([(
                (id, OperationKind::Generation, TransportMode::Streaming),
                BTreeMap::from([(
                    "model".to_owned(),
                    Measurement {
                        latency_ms: 10,
                        output_tokens_per_second: Some(20),
                        samples: 20,
                        observed_at: chrono::Utc::now(),
                    },
                )]),
            )]),
        };
        assert!(
            view.get(
                id,
                "model",
                OperationKind::Generation,
                TransportMode::Streaming
            )
            .is_some()
        );
        assert!(
            view.get(id, "model", OperationKind::Generation, TransportMode::Unary)
                .is_none()
        );
        assert!(view.fresh());
        view.refreshed = Some(Instant::now() - Duration::from_secs(61));
        assert!(!view.fresh());
    }
}
