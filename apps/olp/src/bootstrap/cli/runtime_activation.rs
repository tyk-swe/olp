use std::{collections::BTreeSet, sync::Arc, time::Duration};

use olp_db::{security::envelope::MasterKey, store::Store, valkey::RuntimeHintSubscriber};
use olp_engine::inference::{circuit::Breaker, runtime::Manager};
use olp_engine::providers::{connector::ResponseLimits, http_egress::EgressPolicy};
use tokio::{sync::watch, task::JoinHandle};
use tracing::{error, info, warn};

use crate::{
    application::transports::TransportRegistry, bootstrap::connectors::load_runtime_transports,
};

use super::AppResult;

pub(super) struct RuntimeHintSource {
    pub(super) url: String,
    pub(super) channel: String,
}

/// Everything a background activation needs, cloned once per supervisor.
#[derive(Clone)]
pub(super) struct RuntimeActivator {
    pub(super) runtime: Arc<Manager>,
    pub(super) store: Store,
    pub(super) transports: TransportRegistry,
    pub(super) circuits: Breaker,
    pub(super) master_key: Option<Arc<MasterKey>>,
    pub(super) egress_policy: Arc<EgressPolicy>,
    pub(super) response_limits: ResponseLimits,
}

impl RuntimeActivator {
    async fn activate(&self) -> AppResult<bool> {
        activate_latest_runtime(
            &self.runtime,
            &self.store,
            &self.transports,
            &self.circuits,
            self.master_key.as_deref(),
            &self.egress_policy,
            self.response_limits,
        )
        .await
    }
}

pub(super) async fn runtime_hint_supervisor(
    activator: RuntimeActivator,
    source: RuntimeHintSource,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = Duration::from_millis(100);
    loop {
        if *shutdown.borrow() {
            return;
        }
        let result: AppResult<()> = async {
            let mut subscriber =
                RuntimeHintSubscriber::connect(&source.url, &source.channel).await?;
            backoff = Duration::from_millis(100);
            loop {
                tokio::select! {
                    changed = shutdown.changed() => {
                        if changed.is_err() || *shutdown.borrow() {
                            return Ok(());
                        }
                    }
                    hint = subscriber.recv() => {
                        hint?;
                        match activator.activate().await {
                            Ok(true) => info!(
                                generation = ?activator.runtime.active_generation_ordinal(),
                                "runtime hint activated generation"
                            ),
                            Ok(false) => {}
                            Err(error) => error!(%error, "runtime hint rejected; retaining last-known-good"),
                        }
                    }
                }
            }
        }
        .await;
        if *shutdown.borrow() {
            return;
        }
        if let Err(error) = result {
            warn!(%error, "runtime hint subscriber failed; polling remains active");
        }
        tokio::select! {
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    return;
                }
            }
            () = tokio::time::sleep(backoff) => {}
        }
        backoff = (backoff * 2).min(Duration::from_secs(5));
    }
}

pub(super) fn spawn_runtime_poller(
    activator: RuntimeActivator,
    mut shutdown: watch::Receiver<bool>,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        let mut interval = tokio::time::interval(Duration::from_secs(5));
        interval.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Skip);
        loop {
            tokio::select! {
                _ = interval.tick() => {
                    match activator.activate().await {
                        Ok(true) => {
                            info!(
                                generation = ?activator.runtime.active_generation_ordinal(),
                                "runtime generation activated"
                            );
                        }
                        Ok(false) => {}
                        Err(error) => {
                            // Keep serving the last-known-good Arc. A bad release never
                            // partially changes live indexes.
                            error!(%error, "runtime poll rejected release; retaining last-known-good")
                        }
                    }
                }
                changed = shutdown.changed() => {
                    if changed.is_err() || *shutdown.borrow() {
                        return;
                    }
                }
            }
        }
    })
}

pub(super) async fn activate_latest_runtime(
    runtime: &Manager,
    store: &Store,
    transports: &TransportRegistry,
    circuits: &Breaker,
    master_key: Option<&MasterKey>,
    egress_policy: &EgressPolicy,
    response_limits: ResponseLimits,
) -> AppResult<bool> {
    let releases = store
        .recent_valid_runtime_releases_after(32, runtime.active_generation_ordinal())
        .await?;
    if releases.is_empty() {
        return Ok(false);
    }
    let current_api_keys = store.current_runtime_api_keys().await?;
    let mut rejected = Vec::new();
    for release in releases {
        let mut snapshot = match runtime
            .decode_release_candidate(release.activation_candidate(), current_api_keys.clone())
        {
            Ok(snapshot) => snapshot,
            Err(error) => {
                rejected.push(format!("{}: {error}", release.sequence));
                continue;
            }
        };
        // Provider transports are assembled from normalized secret storage, not
        // the public runtime payload. Require the release-time sidecar to match
        // every current transport-affecting field before accepting an LKG.
        let provider_configurations = match store.runtime_provider_configurations(&snapshot).await {
            Ok(configurations) => configurations,
            Err(error) => {
                rejected.push(format!("{}: {error}", release.sequence));
                continue;
            }
        };
        for configuration in provider_configurations {
            if let Some(revision) = configuration.provider_revision_id
                && let Some(provider) = snapshot.providers.get_mut(&configuration.provider_id)
            {
                provider.revision_id = Some(revision);
            }
        }
        let mut candidate_transports = transports.snapshot();
        if let Some(master_key) = master_key
            && let Err(error) = load_runtime_transports(
                store,
                master_key,
                &snapshot,
                &mut candidate_transports,
                egress_policy,
                response_limits,
            )
            .await
        {
            rejected.push(format!("{}: {error}", release.sequence));
            continue;
        }
        candidate_transports.retain(|provider_id, _| snapshot.providers.contains_key(provider_id));
        let live_targets = snapshot
            .routes
            .values()
            .flat_map(|route| {
                route
                    .targets
                    .iter()
                    .map(|target| target.routing_id.unwrap_or(target.id))
            })
            .collect::<BTreeSet<_>>();
        match runtime.install(snapshot, candidate_transports) {
            Ok(installed) => {
                if installed {
                    circuits.retain_targets(&live_targets);
                }
                if !rejected.is_empty() {
                    warn!(
                        rejected = ?rejected,
                        selected_sequence = release.sequence,
                        "installed previous verified runtime release after rejecting newer candidates"
                    );
                }
                return Ok(installed);
            }
            Err(error) => rejected.push(format!("{}: {error}", release.sequence)),
        }
    }
    if rejected.is_empty() {
        return Ok(false);
    }
    Err(std::io::Error::other(format!(
        "no verified runtime release could be installed: {}",
        rejected.join("; ")
    ))
    .into())
}
