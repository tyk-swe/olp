use std::{
    collections::{BTreeMap, BTreeSet},
    sync::Arc,
    time::Duration,
};

use olp_db::{security::envelope::MasterKey, store::Store, valkey::RuntimeHintSubscriber};
use olp_engine::domain::{ids::ProviderId, ports::ProviderTransport, routing::snapshot::Snapshot};
use olp_engine::inference::{circuit::Breaker, runtime::Manager};
use olp_engine::providers::{connector::ResponseLimits, http_egress::EgressPolicy};
use tokio::{
    sync::{Mutex, MutexGuard, watch},
    task::JoinHandle,
};
use tracing::{error, info, warn};

use crate::{
    application::transports::TransportRegistry,
    bootstrap::{connectors::load_runtime_transports, state::ProcessComposition},
};

use super::AppResult;

pub(super) struct RuntimeHintSource {
    pub(super) url: String,
    pub(super) channel: String,
}

#[derive(Clone)]
pub(super) struct RuntimeActivator {
    pub(super) runtime: Arc<Manager>,
    store: Store,
    transports: TransportRegistry,
    circuits: Breaker,
    master_key: Option<Arc<MasterKey>>,
    egress_policy: Arc<EgressPolicy>,
    response_limits: ResponseLimits,
    activation_lock: Arc<Mutex<()>>,
    #[cfg(test)]
    after_publication: Option<Arc<tokio::sync::Barrier>>,
    #[cfg(test)]
    after_authority_read: Option<Arc<tokio::sync::Barrier>>,
}

impl RuntimeActivator {
    pub(super) fn new(state: &ProcessComposition) -> Self {
        Self {
            runtime: Arc::clone(&state.runtime),
            store: state.store.clone(),
            transports: state.transports.clone(),
            circuits: state.circuits.clone(),
            master_key: state.master_key.clone(),
            egress_policy: Arc::clone(&state.provider_egress_policy),
            response_limits: state.provider_response_limits,
            activation_lock: Arc::new(Mutex::new(())),
            #[cfg(test)]
            after_publication: None,
            #[cfg(test)]
            after_authority_read: None,
        }
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

impl RuntimeActivator {
    pub(super) async fn activate(&self) -> AppResult<bool> {
        let activation = self.activation_lock.lock().await;
        let current_api_keys = self.store.current_runtime_api_keys().await?;
        #[cfg(test)]
        if let Some(barrier) = &self.after_authority_read {
            barrier.wait().await;
            barrier.wait().await;
        }
        self.runtime.refresh_api_keys(current_api_keys.clone())?;
        let releases = self
            .store
            .recent_valid_runtime_releases_after(32, self.runtime.active_generation_ordinal())
            .await?;
        if releases.is_empty() {
            return Ok(false);
        }
        let mut rejected = Vec::new();
        for release in releases {
            let mut snapshot = match self
                .runtime
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
            let provider_configurations =
                match self.store.runtime_provider_configurations(&snapshot).await {
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
            let mut candidate_transports = self.transports.snapshot();
            if let Some(master_key) = self.master_key.as_deref()
                && let Err(error) = load_runtime_transports(
                    &self.store,
                    master_key,
                    &snapshot,
                    &mut candidate_transports,
                    &self.egress_policy,
                    self.response_limits,
                )
                .await
            {
                rejected.push(format!("{}: {error}", release.sequence));
                continue;
            }
            candidate_transports
                .retain(|provider_id, _| snapshot.providers.contains_key(provider_id));
            match self
                .install_candidate(&activation, snapshot, candidate_transports)
                .await
            {
                Ok(installed) => {
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

    async fn install_candidate(
        &self,
        _activation: &MutexGuard<'_, ()>,
        snapshot: Snapshot,
        transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
    ) -> AppResult<bool> {
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
        let installed = self.runtime.install(snapshot, transports)?;
        if installed {
            #[cfg(test)]
            if let Some(barrier) = &self.after_publication {
                barrier.wait().await;
            }
            self.circuits.retain_targets(&live_targets);
        }
        Ok(installed)
    }
}

#[cfg(test)]
mod tests;
