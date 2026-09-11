use crate::inference::transport::*;
use crate::limits::admission::{LimitRequest, ReloadableLimiter, Reservation};
use crate::providers::pool::CredentialSlot;
use futures::StreamExt;
use std::{collections::BTreeMap, sync::Arc};

pub struct PoolTransport {
    pub transports: BTreeMap<Option<uuid::Uuid>, Arc<dyn ProviderTransport>>,
    pub default: Option<uuid::Uuid>,
    pub slots: Vec<CredentialSlot>,
    pub options: crate::providers::options::ConnectionOptions,
    pub limiter: ReloadableLimiter,
}

/// Admission can be cancelled between scopes. Until dispatch starts, dropping
/// these reservations must refund rate capacity as well as concurrency.
#[derive(Default)]
struct PendingReservations(Vec<Reservation>);

impl PendingReservations {
    fn commit(mut self) -> Vec<Reservation> {
        std::mem::take(&mut self.0)
    }

    fn start_refund(&mut self) -> Option<tokio::task::JoinHandle<()>> {
        if self.0.is_empty() {
            return None;
        }
        let reservations = std::mem::take(&mut self.0);
        let Ok(runtime) = tokio::runtime::Handle::try_current() else {
            tracing::warn!("could not refund provider admission outside a Tokio runtime");
            return None;
        };
        Some(runtime.spawn(async move {
            for reservation in reservations {
                reservation.refund().await;
            }
        }))
    }

    async fn refund(mut self) {
        if let Some(task) = self.start_refund() {
            let _ = task.await;
        }
    }
}

impl Drop for PendingReservations {
    fn drop(&mut self) {
        let _ = self.start_refund();
    }
}

impl PoolTransport {
    /// An empty pool for one provider: slots come from the snapshot's published
    /// credential policy; callers mount the per-credential transports.
    pub(crate) fn for_provider(
        snapshot: &crate::runtime::snapshot::Snapshot,
        provider: crate::ids::ProviderId,
        options: crate::providers::options::ConnectionOptions,
        default: Option<uuid::Uuid>,
        limiter: &ReloadableLimiter,
    ) -> Self {
        Self {
            transports: BTreeMap::new(),
            default,
            slots: snapshot
                .routing
                .credentials
                .get(&provider)
                .cloned()
                .unwrap_or_default(),
            options,
            limiter: limiter.clone(),
        }
    }

    async fn reserve(
        &self,
        id: uuid::Uuid,
        lookup: &str,
        limits: &crate::providers::options::ConnectionLimits,
        requested_tokens: i64,
        lease_ttl: std::time::Duration,
    ) -> Result<Option<Reservation>, TransportError> {
        if limits.requests_per_minute.is_none()
            && limits.tokens_per_minute.is_none()
            && limits.max_concurrency.is_none()
        {
            return Ok(None);
        }
        let backend = self
            .limiter
            .current()
            .ok_or_else(|| limit_error("provider_limits_unavailable"))?;
        let lease = backend
            .reserve(LimitRequest {
                api_key_id: id,
                lookup_id: lookup,
                requests_per_minute: limits.requests_per_minute.map(i64::from),
                tokens_per_minute: limits.tokens_per_minute.map(|n| n as i64),
                max_concurrency: limits.max_concurrency.map(i64::from),
                daily_cost_limit: None,
                monthly_cost_limit: None,
                requested_tokens,
                lease_ttl,
            })
            .await
            .map_err(|_| limit_error("provider_capacity_unavailable"))?;
        Ok(Some(Reservation::distributed(lease)))
    }
}

/// Limiter lookup id for a provider connection's shared quota.
pub(crate) fn connection_lookup(provider: uuid::Uuid) -> String {
    format!("pc_{}", provider.simple())
}

/// Limiter lookup id for one credential slot's quota.
pub(crate) fn slot_lookup(slot: uuid::Uuid) -> String {
    format!("ps_{}", slot.simple())
}

impl ProviderTransport for PoolTransport {
    fn execute(
        &self,
        request: ProviderRequest,
    ) -> BoxFuture<'_, Result<ProviderOutput, TransportError>> {
        Box::pin(async move {
            let version = request.attempt.credential_version_id.or(self.default);
            let cooldown_scope = credential_scope(request.attempt.provider_id.as_uuid(), version);
            let slot_id = request
                .attempt
                .credential_slot_id
                .unwrap_or(request.attempt.provider_id.as_uuid());
            if let Some(backend) = self.limiter.current()
                && is_cooling(
                    backend.as_ref(),
                    request.attempt.provider_id.as_uuid(),
                    slot_id,
                    version,
                )
                .await
                    == Some(true)
            {
                return Err(limit_error("provider_credential_cooling_down"));
            }
            let transport = self.transports.get(&version).ok_or_else(|| {
                crate::providers::transport_common::protocol_error(
                    "Pinned credential transport is unavailable",
                )
            })?;
            let mut reservations = PendingReservations::default();
            let requested_tokens = crate::providers::http_options::estimate_tokens(
                &request.operation,
                request.attempt.provider_kind,
                &self.options,
            )
            .max(1);
            let lease_ttl = request.attempt.timeout.as_duration();
            // Connection admission is settled before slot admission so a
            // rejected slot refunds exactly the connection lease it acquired.
            if let Some(limits) = request
                .attempt
                .connection_limits
                .as_ref()
                .or(self.options.limits.as_ref())
                && let Some(lease) = self
                    .reserve(
                        request.attempt.provider_id.as_uuid(),
                        &connection_lookup(request.attempt.provider_id.as_uuid()),
                        limits,
                        requested_tokens,
                        lease_ttl,
                    )
                    .await?
            {
                reservations.0.push(lease);
            }
            if let Some(limits) = request.attempt.credential_limits.clone().or_else(|| {
                self.slots
                    .iter()
                    .find(|slot| slot.id == slot_id)
                    .map(CredentialSlot::limits)
            }) {
                match self
                    .reserve(
                        slot_id,
                        &slot_lookup(slot_id),
                        &limits,
                        requested_tokens,
                        lease_ttl,
                    )
                    .await
                {
                    Ok(Some(lease)) => reservations.0.push(lease),
                    Ok(None) => {}
                    Err(error) => {
                        reservations.refund().await;
                        return Err(error);
                    }
                }
            }
            let reservations = reservations.commit();
            let output = match transport.execute(request).await {
                Ok(output) => output,
                Err(error) => {
                    record_failure(&self.limiter, &cooldown_scope, slot_id, &error).await;
                    return Err(error);
                }
            };
            match output {
                ProviderOutput::Events(events) => {
                    let limiter = self.limiter.clone();
                    Ok(ProviderOutput::Events(Box::pin(futures::stream::unfold(
                        (
                            events,
                            reservations,
                            crate::inference::lifecycle::UsageCapture::default(),
                        ),
                        move |(mut events, mut reservations, mut usage)| {
                            let limiter = limiter.clone();
                            let cooldown_scope = cooldown_scope.clone();
                            async move {
                                match events.next().await {
                                    Some(event) => {
                                        if let Err(error) = &event {
                                            record_failure(
                                                &limiter,
                                                &cooldown_scope,
                                                slot_id,
                                                error,
                                            )
                                            .await;
                                        }
                                        if let Ok(event) = &event {
                                            usage.observe(event);
                                            use crate::protocols::canonical::events::{
                                                ErrorClass, Kind,
                                            };
                                            if let Kind::Error { error } = &event.kind {
                                                let trigger = match error.class {
                                                    ErrorClass::Authentication => {
                                                        Some(CooldownTrigger::Authentication)
                                                    }
                                                    ErrorClass::RateLimit => {
                                                        Some(CooldownTrigger::RateLimit(None))
                                                    }
                                                    _ => None,
                                                };
                                                if let Some(trigger) = trigger {
                                                    record_trigger(
                                                        &limiter,
                                                        &cooldown_scope,
                                                        slot_id,
                                                        trigger,
                                                    )
                                                    .await;
                                                }
                                            }
                                            if matches!(event.kind, Kind::Done) {
                                                settle(
                                                    std::mem::take(&mut reservations),
                                                    usage.actual_tokens(),
                                                );
                                            }
                                        }
                                        Some((event, (events, reservations, usage)))
                                    }
                                    None => {
                                        drop(reservations);
                                        None
                                    }
                                }
                            }
                        },
                    ))))
                }
                ProviderOutput::Result(result) => {
                    let tokens =
                        crate::inference::lifecycle::usage_from_result(&result).actual_tokens();
                    settle(reservations, tokens);
                    Ok(ProviderOutput::Result(result))
                }
            }
        })
    }
}

/// Upstream failures that take a credential or slot out of rotation.
#[derive(Clone, Copy)]
enum CooldownTrigger {
    /// The secret itself was rejected: cool the credential version for a day.
    Authentication,
    /// The account behind the slot is throttled: honour the upstream hint or
    /// back off briefly.
    RateLimit(Option<std::time::Duration>),
}

/// Single cooldown policy table shared by unary and streaming failures.
fn cooldown_for(
    credential_scope: &str,
    slot: uuid::Uuid,
    trigger: CooldownTrigger,
) -> (String, std::time::Duration) {
    match trigger {
        CooldownTrigger::Authentication => (
            credential_scope.to_owned(),
            std::time::Duration::from_secs(86400),
        ),
        CooldownTrigger::RateLimit(retry_after) => (
            slot_scope(slot),
            retry_after.unwrap_or(std::time::Duration::from_secs(30)),
        ),
    }
}

async fn record_trigger(
    limiter: &ReloadableLimiter,
    credential_scope: &str,
    slot: uuid::Uuid,
    trigger: CooldownTrigger,
) {
    let (scope, duration) = cooldown_for(credential_scope, slot, trigger);
    record_cooldown(limiter, &scope, duration).await;
}

async fn record_failure(
    limiter: &ReloadableLimiter,
    credential_scope: &str,
    slot: uuid::Uuid,
    error: &TransportError,
) {
    let trigger = if error.upstream.status == Some(401) {
        CooldownTrigger::Authentication
    } else if error.class == AttemptFailureClass::RateLimit {
        CooldownTrigger::RateLimit(error.upstream.retry_after)
    } else {
        return;
    };
    record_trigger(limiter, credential_scope, slot, trigger).await;
}

async fn record_cooldown(limiter: &ReloadableLimiter, scope: &str, duration: std::time::Duration) {
    // Zero means "retry now", not permission to clear a concurrent failure's
    // longer cooldown. Explicit validation clears use clear_cooldowns instead.
    if duration.is_zero() {
        return;
    }
    if let Some(backend) = limiter.current() {
        let _ = tokio::time::timeout(
            std::time::Duration::from_millis(250),
            backend.provider_cooldown(scope, Some(duration)),
        )
        .await;
    }
}

fn limit_error(message: &str) -> TransportError {
    crate::providers::transport_common::transport_error(
        TransportPhase::Connect,
        AttemptFailureClass::RateLimit,
        false,
        message,
    )
}

/// Cooldown scope for one credential version of a provider. A version-less
/// default credential is keyed by the nil UUID.
fn credential_scope(provider: uuid::Uuid, version: Option<uuid::Uuid>) -> String {
    format!("{provider}:{}", version.unwrap_or(uuid::Uuid::nil()))
}

/// Cooldown scope for one credential slot (the account behind the secret).
fn slot_scope(slot: uuid::Uuid) -> String {
    format!("slot:{slot}")
}

/// Lifts both cooldowns after an operator re-validates a credential.
pub(crate) async fn clear_cooldowns(
    backend: &dyn crate::limits::admission::LimitBackend,
    provider: uuid::Uuid,
    slot: uuid::Uuid,
    version: Option<uuid::Uuid>,
) {
    for scope in [credential_scope(provider, version), slot_scope(slot)] {
        let _ = backend
            .provider_cooldown(&scope, Some(std::time::Duration::ZERO))
            .await;
    }
}

fn settle(reservations: Vec<Reservation>, tokens: Option<i64>) {
    if reservations.is_empty() {
        return;
    }
    tokio::spawn(async move {
        for lease in reservations {
            if let Some(tokens) = tokens {
                lease.reconcile(tokens).await;
            }
            lease.release().await;
        }
    });
}

pub(crate) async fn is_cooling(
    backend: &dyn crate::limits::admission::LimitBackend,
    provider: uuid::Uuid,
    slot: uuid::Uuid,
    version: Option<uuid::Uuid>,
) -> Option<bool> {
    let credential = credential_scope(provider, version);
    let account = slot_scope(slot);
    tokio::time::timeout(std::time::Duration::from_millis(250), async {
        let (credential, account) = tokio::join!(
            backend.provider_cooldown(&credential, None),
            backend.provider_cooldown(&account, None)
        );
        Some(credential.ok()? || account.ok()?)
    })
    .await
    .ok()
    .flatten()
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::limits::distributed::DistributedLimiter;
    use crate::providers::transport_common::upstream_response_error;
    use std::time::Duration;

    #[tokio::test]
    #[ignore = "requires Valkey via make integration"]
    async fn zero_retry_hints_preserve_a_concurrent_cooldown_until_validation_clears_it() {
        let limiter = ReloadableLimiter::default();
        limiter.install(
            DistributedLimiter::connect(
                &std::env::var("OLP_VALKEY_URL").unwrap(),
                format!("olp_test_zero_hint_{}", uuid::Uuid::now_v7().simple()),
            )
            .await
            .unwrap(),
        );
        let backend = limiter.current().unwrap();
        let provider = uuid::Uuid::now_v7();
        let slot = uuid::Uuid::now_v7();
        let version = Some(uuid::Uuid::now_v7());
        let credential = credential_scope(provider, version);
        let limited = |hint: &'static str| {
            upstream_response_error(
                TransportPhase::FirstByte,
                ::http::StatusCode::TOO_MANY_REQUESTS,
                &::http::HeaderMap::from_iter([(
                    ::http::header::RETRY_AFTER,
                    ::http::HeaderValue::from_static(hint),
                )]),
                "limited",
            )
        };
        record_failure(&limiter, &credential, slot, &limited("120")).await;
        for hint in ["0", "Wed, 21 Oct 2015 07:28:00 GMT"] {
            let error = limited(hint);
            assert_eq!(error.upstream.retry_after, Some(Duration::ZERO));
            record_failure(&limiter, &credential, slot, &error).await;
            assert_eq!(
                is_cooling(backend.as_ref(), provider, slot, version).await,
                Some(true)
            );
        }
        clear_cooldowns(backend.as_ref(), provider, slot, version).await;
        assert_eq!(
            is_cooling(backend.as_ref(), provider, slot, version).await,
            Some(false)
        );
    }
}
