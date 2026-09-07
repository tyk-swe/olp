use crate::usage::queue::*;

#[derive(Clone, Copy, Debug)]
pub struct RequestMetadataConsumerTestPolicy {
    pub batch_size: usize,
    pub block_interval: Duration,
    pub active_recovery_block_interval: Duration,
    pub own_pending_interval: Duration,
    pub reclaim_idle: Duration,
    pub recovery_interval: Duration,
    pub health_interval: Duration,
}

impl Default for RequestMetadataConsumerTestPolicy {
    fn default() -> Self {
        let short = Duration::from_millis(10);
        Self {
            batch_size: 10,
            block_interval: short,
            active_recovery_block_interval: Duration::from_millis(1),
            own_pending_interval: short,
            reclaim_idle: Duration::ZERO,
            recovery_interval: short,
            health_interval: short,
        }
    }
}

pub async fn run_request_metadata_consumer(
    pool: &PgPool,
    valkey_url: &str,
    stream: &str,
    consumer: &str,
    limits_namespace: &str,
    shutdown: watch::Receiver<bool>,
    policy: RequestMetadataConsumerTestPolicy,
) -> Result<(), Error> {
    run_request_metadata_consumer_with_policy(
        pool,
        valkey_url,
        stream,
        consumer,
        limits_namespace,
        shutdown,
        ConsumerPolicy {
            batch_size: policy.batch_size,
            block_interval: policy.block_interval,
            active_recovery_block_interval: policy.active_recovery_block_interval,
            own_pending_interval: policy.own_pending_interval,
            reclaim_idle: policy.reclaim_idle,
            recovery_interval: policy.recovery_interval,
            health_interval: policy.health_interval,
        },
    )
    .await
}
