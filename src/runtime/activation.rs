use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::sync::Arc;
use std::time::Duration;

use crate::crypto::envelope::MasterKey;
use crate::ids::ProviderId;
use crate::inference::circuit::Breaker;
use crate::inference::transport::ProviderTransport;
use crate::limits::valkey::RuntimeHintSubscriber;
use crate::net::egress::EgressPolicy;
use crate::process::workers::RESTART_BACKOFF_FLOOR;
use crate::process::workers::next_restart_backoff;
use crate::process::workers::sleep_unless_shutdown;
use crate::providers::connector::ResponseLimits;
use crate::runtime::manager::Manager;
use crate::runtime::snapshot::Snapshot;
use sqlx::PgPool;
use tokio::sync::Mutex;
use tokio::sync::watch;
use tokio::task::JoinHandle;
use tracing::error;
use tracing::info;
use tracing::warn;

use crate::providers::runtime_config::load_runtime_transports;
use crate::runtime::transports::TransportRegistry;

use crate::process::error::AppResult;

pub(crate) struct RuntimeHintSource {
    pub(crate) url: String,
    pub(crate) channel: String,
}

#[derive(Clone)]
pub(crate) struct RuntimeActivator {
    pub(crate) runtime: Arc<Manager>,
    pool: PgPool,
    transports: TransportRegistry,
    circuits: Breaker,
    master_key: Option<Arc<MasterKey>>,
    egress_policy: Arc<EgressPolicy>,
    response_limits: ResponseLimits,
    limiter: crate::limits::admission::ReloadableLimiter,
    activation_lock: Arc<Mutex<()>>,
    #[cfg(test)]
    after_publication: Option<Arc<tokio::sync::Barrier>>,
    #[cfg(test)]
    after_authority_read: Option<Arc<tokio::sync::Barrier>>,
}

impl RuntimeActivator {
    #[allow(clippy::too_many_arguments)]
    pub(crate) fn new(
        runtime: Arc<Manager>,
        pool: PgPool,
        transports: TransportRegistry,
        circuits: Breaker,
        master_key: Option<Arc<MasterKey>>,
        egress_policy: Arc<EgressPolicy>,
        response_limits: ResponseLimits,
        limiter: crate::limits::admission::ReloadableLimiter,
    ) -> Self {
        Self {
            runtime,
            pool,
            transports,
            circuits,
            master_key,
            egress_policy,
            response_limits,
            limiter,
            activation_lock: Arc::new(Mutex::new(())),
            #[cfg(test)]
            after_publication: None,
            #[cfg(test)]
            after_authority_read: None,
        }
    }
}

pub(crate) async fn runtime_hint_supervisor(
    activator: RuntimeActivator,
    source: RuntimeHintSource,
    mut shutdown: watch::Receiver<bool>,
) {
    let mut backoff = RESTART_BACKOFF_FLOOR;
    loop {
        if *shutdown.borrow() {
            return;
        }
        let result: AppResult<()> = async {
            let mut subscriber =
                RuntimeHintSubscriber::connect(&source.url, &source.channel).await?;
            backoff = RESTART_BACKOFF_FLOOR;
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
        if !sleep_unless_shutdown(&mut shutdown, backoff).await {
            return;
        }
        backoff = next_restart_backoff(backoff);
    }
}

pub(crate) fn spawn_runtime_poller(
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
    pub(crate) async fn activate(&self) -> AppResult<bool> {
        let _activation = self.activation_lock.lock().await;
        let authority_read_started = std::time::Instant::now();
        let (current_authority, latest_sequence) = tokio::join!(
            crate::runtime::publication::compiler::current_runtime_authority(&self.pool),
            crate::runtime::publication::releases::latest_runtime_generation_sequence(&self.pool),
        );
        let current_authority = current_authority?;
        #[cfg(test)]
        if let Some(barrier) = &self.after_authority_read {
            barrier.wait().await;
            barrier.wait().await;
        }
        self.runtime
            .refresh_current_authority(&current_authority, authority_read_started)?;
        if let Some(sequence) = latest_sequence? {
            self.runtime
                .observe_desired_generation(u64::try_from(sequence)?);
        }
        let releases = crate::runtime::publication::releases::recent_valid_runtime_releases_after(
            &self.pool,
            32,
            self.runtime.active_generation_ordinal(),
        )
        .await?;
        if releases.is_empty() {
            return Ok(false);
        }
        let mut rejected = Vec::new();
        for release in releases {
            let mut snapshot = match self.runtime.decode_release_candidate(
                release.activation_candidate(),
                &current_authority.api_keys,
            ) {
                Ok(snapshot) => snapshot,
                Err(error) => {
                    rejected.push(format!("{}: {error}", release.sequence));
                    continue;
                }
            };
            snapshot.routing.installation = current_authority.installation.clone();
            snapshot.routing.credential_authority = Some(current_authority.credentials.clone());
            snapshot.routing.connection_limit_authority =
                Some(current_authority.connection_limits.clone());
            snapshot.routing.revoked_credential_versions =
                current_authority.revoked_credential_versions.clone();
            snapshot
                .routing
                .routes
                .extend(current_authority.routes.clone());
            // Provider transports are assembled from normalized secret storage, not
            // the public runtime payload. Require the release-time sidecar to match
            // every current transport-affecting field before accepting an LKG.
            let provider_configurations =
                match crate::providers::runtime::runtime_provider_configurations(
                    &self.pool, &snapshot,
                )
                .await
                {
                    Ok(configurations) => configurations,
                    Err(error) => {
                        rejected.push(format!("{}: {error}", release.sequence));
                        continue;
                    }
                };
            let mut candidate_transports = self.transports.snapshot();
            if let Err(error) = load_runtime_transports(
                &provider_configurations,
                self.master_key.as_deref(),
                &snapshot,
                &mut candidate_transports,
                &self.egress_policy,
                self.response_limits,
                &self.limiter,
            )
            .await
            {
                rejected.push(format!("{}: {error}", release.sequence));
                continue;
            }
            candidate_transports
                .retain(|provider_id, _| snapshot.providers.contains_key(provider_id));
            match self.install_candidate(snapshot, candidate_transports).await {
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
        snapshot: Snapshot,
        transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>>,
    ) -> AppResult<bool> {
        let live_targets = snapshot
            .routes
            .values()
            .flat_map(|route| route.targets.iter().map(|target| target.routing_id))
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
pub mod tests;
