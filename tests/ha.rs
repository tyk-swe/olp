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

use std::time::Duration;
use std::time::Instant;

use futures::StreamExt as _;
use olp::limits::admission::LimitError;
use olp::limits::admission::LimitRequest;
use olp::limits::distributed::DistributedLimiter;
use olp::observability::workers::WorkerTask;
use redis::AsyncCommands as _;
use serde_json::Value;
use serde_json::json;
use sqlx::Connection as _;

use harness::GatewayProcess;
use harness::SharedValkey;
use world::IssuedKey;
use world::Management;
use world::OPENAI_ROUTE;
use world::World;

#[macro_export]
macro_rules! require {
    ($condition:expr, $($message:tt)*) => {
        if !$condition {
            return Err(format!($($message)*));
        }
    };
}

#[path = "ha/authority.rs"]
mod authority;
#[path = "ha/convergence.rs"]
mod convergence;
#[path = "ha/shared_valkey.rs"]
mod shared_valkey;
#[path = "ha/worker_recovery.rs"]
mod worker_recovery;

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "high-availability; run through scripts/integration.sh"]
async fn two_gateways_converge_and_degrade_safely() -> Result<(), String> {
    let (world, gateway) = world::fixture::bootstrap_ha().await?;
    let result = async {
        convergence::exercise(&world, &gateway).await?;
        authority::exercise(&world, &gateway).await
    }
    .await;
    let logs = world.shutdown().await;
    result.map_err(|error| format!("{error}\nserver logs:\n{logs}"))
}

#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
#[ignore = "shared-Valkey qualification; run through scripts/integration.sh"]
async fn worker_ha_shared_valkey_installations_are_isolated() -> Result<(), String> {
    let valkey = SharedValkey::reserve().await?;
    let installation_a = match world::fixture::bootstrap_sharing_valkey(valkey.url()).await {
        Ok(world) => world,
        Err(error) => {
            valkey.release().await;
            return Err(error);
        }
    };
    let installation_b = match world::fixture::bootstrap_sharing_valkey(valkey.url()).await {
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
#[ignore = "worker HA qualification; run through scripts/integration.sh"]
async fn worker_ha_three_workers_recover_owned_metadata_and_outbox_work() -> Result<(), String> {
    let (world, workers) = world::fixture::bootstrap_worker_ha().await?;
    let result = worker_recovery::prove_three_worker_recovery(&world, &workers).await;
    let logs = world.shutdown().await;
    result.map_err(|error| format!("{error}\nworker HA process logs:\n{logs}"))
}
