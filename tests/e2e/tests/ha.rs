//! Two-gateway convergence and degraded-dependency proof.

#[allow(dead_code)]
#[path = "contract/harness.rs"]
mod harness;
#[allow(dead_code)]
#[path = "contract/mock_upstream.rs"]
mod mock_upstream;
#[allow(dead_code)]
#[path = "contract/otlp.rs"]
mod otlp;
#[allow(dead_code)]
#[path = "contract/world.rs"]
mod world;

use std::time::{Duration, Instant};

use futures::StreamExt as _;
use olp_db::{limits::DistributedLimiter, worker_health::WorkerTask};
use olp_engine::inference::limits::{LimitError, LimitRequest};
use redis::{
    AsyncCommands as _,
    streams::{StreamInfoGroupsReply, StreamPendingCountReply},
};
use serde_json::{Value, json};
use sqlx::Connection as _;

use harness::{GatewayProcess, Server, SharedValkey};
use world::{IssuedKey, Management, OPENAI_ROUTE, World};

#[macro_export]
macro_rules! require {
    ($condition:expr, $($message:tt)*) => {
        if !$condition {
            return Err(format!($($message)*));
        }
    };
}

#[path = "ha/convergence.rs"]
mod convergence;
#[path = "ha/shared_valkey.rs"]
mod shared_valkey;
#[path = "ha/worker_recovery.rs"]
mod worker_recovery;

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "high-availability; run through scripts/run-e2e-tests.sh"]
async fn two_gateways_converge_and_degrade_safely() -> Result<(), String> {
    let (world, gateway) = world::bootstrap_ha().await?;
    let result = convergence::exercise(&world, &gateway).await;
    let logs = world.shutdown().await;
    result.map_err(|error| format!("{error}\nserver logs:\n{logs}"))
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "shared-Valkey qualification; run through scripts/run-e2e-tests.sh"]
async fn worker_ha_shared_valkey_installations_are_isolated() -> Result<(), String> {
    let valkey = SharedValkey::reserve().await?;
    let installation_a = match world::bootstrap_sharing_valkey(valkey.url()).await {
        Ok(world) => world,
        Err(error) => {
            valkey.release().await;
            return Err(error);
        }
    };
    let installation_b = match world::bootstrap_sharing_valkey(valkey.url()).await {
        Ok(world) => world,
        Err(error) => {
            let logs = installation_a.shutdown().await;
            valkey.release().await;
            return Err(format!("{error}\ninstallation A logs:\n{logs}"));
        }
    };

    let result =
        shared_valkey::prove_shared_valkey_isolation(&installation_a, &installation_b).await;
    let logs_a = installation_a.shutdown().await;
    let teardown_result = match &result {
        Ok(keys_b) => shared_valkey::assert_valkey_keys_exist(valkey.url(), keys_b).await,
        Err(_) => Ok(()),
    };
    let logs_b = installation_b.shutdown().await;
    valkey.release().await;

    result.map(|_| ()).and(teardown_result).map_err(|error| {
        format!("{error}\ninstallation A logs:\n{logs_a}\ninstallation B logs:\n{logs_b}")
    })
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "legacy namespace qualification; run through scripts/run-e2e-tests.sh"]
async fn worker_ha_migrate_preserves_legacy_stream_ownership() -> Result<(), String> {
    const LEGACY: &str = "olp:v2:request-metadata";
    const GROUP: &str = "olp:persistence";
    let valkey = SharedValkey::reserve().await?;

    let (server, event_id) =
        match Server::launch_control_from_legacy_request_metadata_upgrade(valkey.url()).await {
            Ok(server) => server,
            Err(error) => {
                valkey.release().await;
                return Err(error);
            }
        };
    let result = async {
        let client = redis::Client::open(valkey.url())
            .map_err(|error| format!("invalid shared Valkey URL: {error}"))?;
        let mut connection = client
            .get_multiplexed_async_connection()
            .await
            .map_err(|error| format!("failed to connect shared Valkey: {error}"))?;
        let target = format!(
            "{}:request-metadata",
            shared_valkey::installation_prefix(&server.database_url).await?
        );
        let legacy_exists: bool = connection
            .exists(LEGACY)
            .await
            .map_err(|error| format!("failed to inspect legacy stream: {error}"))?;
        let target_exists: bool = connection
            .exists(&target)
            .await
            .map_err(|error| format!("failed to inspect namespaced stream: {error}"))?;
        require!(!legacy_exists, "migration left the legacy stream present");
        require!(
            target_exists,
            "migration did not create the namespaced stream"
        );
        let groups: StreamInfoGroupsReply = connection
            .xinfo_groups(&target)
            .await
            .map_err(|error| format!("failed to inspect migrated consumer group: {error}"))?;
        let group = groups
            .groups
            .iter()
            .find(|group| group.name == GROUP)
            .ok_or_else(|| "migration lost the legacy consumer group".to_owned())?;
        require!(group.pending == 1, "migration lost pending-entry state");
        let pending: StreamPendingCountReply = connection
            .xpending_count(&target, GROUP, "-", "+", 10)
            .await
            .map_err(|error| format!("failed to inspect migrated ownership: {error}"))?;
        require!(
            pending
                .ids
                .iter()
                .any(|entry| { entry.id == event_id && entry.consumer == "legacy-owner" }),
            "migration changed pending event identity or ownership"
        );
        Ok::<(), String>(())
    }
    .await;
    let logs = server.shutdown().await;
    valkey.release().await;
    result.map_err(|error| format!("{error}\ncontrol logs:\n{logs}"))
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "worker HA qualification; run through scripts/run-e2e-tests.sh"]
async fn worker_ha_three_workers_recover_owned_metadata_and_outbox_work() -> Result<(), String> {
    let (world, workers) = world::bootstrap_worker_ha().await?;
    let result = worker_recovery::prove_three_worker_recovery(&world, &workers).await;
    let logs = world.shutdown().await;
    result.map_err(|error| format!("{error}\nworker HA process logs:\n{logs}"))
}
