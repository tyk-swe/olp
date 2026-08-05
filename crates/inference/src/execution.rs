use std::{
    collections::BTreeMap,
    sync::{
        Arc,
        atomic::{AtomicBool, Ordering},
    },
    time::Duration,
};

use chrono::Utc;
use olp_domain::{
    ApiKey, CanonicalEvent, CanonicalResult, Operation, OperationKind, RequestId, RequestMetadata,
    RouteSlug, Surface, TransportMode, authorize_api_key,
};
use olp_storage::{limits::LimitLease, request_metadata::RequestAttemptMetadata};

use crate::{
    InferenceError, InferenceService,
    accounting::{
        RequestAccountingGuard, RequestAccountingInput, RequestMetadataFinalizer, RequestOutcome,
        usage_from_result,
    },
    events::{MAX_COLLECTED_CANONICAL_EVENT_BYTES, collect_provider_events_with_observer},
    failover::{ExecutionOutput, ExecutionSuccess, FailoverContext, execute_with_failover},
    limits::{
        InferencePrincipal, InferenceReservation, RequestMediaGuard, operation_media_handles,
        release_limits, reserve_limits,
    },
    runtime::RuntimeBundle,
    selection::select_representable_attempts_filtered,
    telemetry::elapsed_ms,
};

/// Reservation already made by delivery-boundary request admission. The core
/// uses it to avoid double charging and to reconcile actual token usage.
#[derive(Clone, Default)]
pub struct RequestAdmission {
    reservation: Option<InferenceReservation>,
    reserved_tokens: Option<i64>,
    metadata_claimed: Option<Arc<AtomicBool>>,
}

impl RequestAdmission {
    #[must_use]
    pub const fn new(
        reservation: Option<InferenceReservation>,
        reserved_tokens: Option<i64>,
        metadata_claimed: Option<Arc<AtomicBool>>,
    ) -> Self {
        Self {
            reservation,
            reserved_tokens,
            metadata_claimed,
        }
    }

    #[must_use]
    pub const fn reserved_tokens(&self) -> Option<i64> {
        self.reserved_tokens
    }

    fn claim_metadata(&self) {
        if let Some(claimed) = &self.metadata_claimed {
            claimed.store(true, Ordering::Release);
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RequiredTarget {
    pub provider_id: uuid::Uuid,
    pub upstream_model: String,
}

struct ExecutionContext {
    generation_id: uuid::Uuid,
    api_key_id: uuid::Uuid,
    request_id: RequestId,
    route_slug: RouteSlug,
    operation_kind: OperationKind,
    request_started_at: chrono::DateTime<Utc>,
    request_started: tokio::time::Instant,
    surface: Surface,
}

impl ExecutionContext {
    fn accounting_input(&self) -> RequestAccountingInput {
        RequestAccountingInput {
            generation_id: self.generation_id,
            api_key_id: self.api_key_id,
            request_id: self.request_id.as_uuid(),
            route_slug: self.route_slug.clone(),
            request_started_at: self.request_started_at,
            request_started: self.request_started,
            surface: self.surface,
            operation: self.operation_kind,
        }
    }
}

struct CompletedExecution {
    context: ExecutionContext,
    success: ExecutionSuccess,
    accounting: RequestAccountingGuard,
}

pub struct RoutedEventExecution {
    pub first: CanonicalEvent,
    pub events: olp_domain::ProviderEventStream,
    pub deadline: tokio::time::Instant,
    pub request_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    accounting: Option<RequestAccountingGuard>,
}

impl RoutedEventExecution {
    #[must_use]
    pub fn take_accounting(&mut self) -> RequestAccountingGuard {
        self.accounting
            .take()
            .expect("routed event execution owns request accounting")
    }

    pub async fn collect(mut self) -> Result<CompletedEventExecution, InferenceError> {
        let mut accounting = self.take_accounting();
        let events = collect_provider_events_with_observer(
            self.first.clone(),
            &mut self.events,
            self.deadline,
            MAX_COLLECTED_CANONICAL_EVENT_BYTES,
            &mut |event| accounting.usage_mut().observe(event),
        )
        .await;
        let events = match events {
            Ok(events) => events,
            Err(failure) => {
                accounting
                    .finish(RequestOutcome::from_error(&failure))
                    .await;
                return Err(failure);
            }
        };
        accounting.release_limits().await;
        let finalizer = accounting.into_finalizer();
        Ok(CompletedEventExecution {
            events,
            route_slug: self.route_slug,
            request_id: self.request_id,
            request_metadata_finalizer: Some(finalizer),
        })
    }
}

pub struct CompletedEventExecution {
    pub events: Vec<CanonicalEvent>,
    pub route_slug: RouteSlug,
    pub request_id: uuid::Uuid,
    request_metadata_finalizer: Option<RequestMetadataFinalizer>,
}

impl CompletedEventExecution {
    pub fn mark_success(&mut self) {
        if let Some(finalizer) = self.request_metadata_finalizer.take() {
            finalizer.finalize(&RequestOutcome::success());
        }
    }
}

impl Drop for CompletedEventExecution {
    fn drop(&mut self) {
        if let Some(finalizer) = self.request_metadata_finalizer.take() {
            finalizer.finalize(&RequestOutcome::provider_protocol_failure());
        }
    }
}

pub struct RoutedUnaryResult {
    pub result: Box<CanonicalResult>,
    pub request_id: RequestId,
    pub api_key_id: uuid::Uuid,
    pub route_slug: RouteSlug,
    pub provider_id: uuid::Uuid,
    pub upstream_model: String,
    request_metadata_finalizer: Option<RequestMetadataFinalizer>,
}

impl RoutedUnaryResult {
    pub fn mark_success(&mut self) {
        if let Some(finalizer) = self.request_metadata_finalizer.take() {
            finalizer.finalize(&RequestOutcome::success());
        }
    }

    pub fn mark_failure(&mut self, outcome: RequestOutcome) {
        if let Some(finalizer) = self.request_metadata_finalizer.take() {
            finalizer.finalize(&outcome);
        }
    }

    pub fn mark_provider_protocol_failure(&mut self) {
        self.mark_failure(RequestOutcome::provider_protocol_failure());
    }
}

impl Drop for RoutedUnaryResult {
    fn drop(&mut self) {
        if let Some(finalizer) = self.request_metadata_finalizer.take() {
            finalizer.finalize(&RequestOutcome::client_cancelled());
        }
    }
}

impl InferenceService {
    pub fn authorize_principal<'a>(
        &self,
        principal: &'a InferencePrincipal,
        operation: OperationKind,
        route: Option<&RouteSlug>,
    ) -> Result<&'a ApiKey, InferenceError> {
        authorize_api_key(principal.key(), route, operation, Utc::now())
            .map_err(|error| InferenceError::forbidden(error.to_string()))?;
        Ok(principal.key())
    }

    pub async fn execute_event(
        &self,
        principal: &InferencePrincipal,
        operation: Operation,
        mode: TransportMode,
        admission: RequestAdmission,
    ) -> Result<RoutedEventExecution, InferenceError> {
        let request_media = RequestMediaGuard::new(
            self.media_spool().clone(),
            operation_media_handles(&operation),
        );
        let result = self
            .execute_event_inner(principal, operation, mode, admission)
            .await;
        request_media.cleanup().await;
        result
    }

    async fn execute_event_inner(
        &self,
        principal: &InferencePrincipal,
        operation: Operation,
        mode: TransportMode,
        admission: RequestAdmission,
    ) -> Result<RoutedEventExecution, InferenceError> {
        let CompletedExecution {
            context,
            success,
            mut accounting,
        } = self
            .execute_operation(principal, operation, mode, None, admission)
            .await?;
        let ExecutionSuccess {
            output,
            deadline,
            attempts,
            attempt_started,
        } = success;
        let first_byte_ms = elapsed_ms(context.request_started.elapsed());
        accounting.record_attempts(attempts, Some(attempt_started), Some(first_byte_ms), true);
        let ExecutionOutput::Events { first, events } = output else {
            let failure = InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an incompatible generation response.",
            );
            accounting
                .finish(RequestOutcome::from_error(&failure))
                .await;
            return Err(failure);
        };
        Ok(RoutedEventExecution {
            first,
            events,
            deadline,
            accounting: Some(accounting),
            request_id: context.request_id.as_uuid(),
            route_slug: context.route_slug,
        })
    }

    pub async fn execute_result(
        &self,
        principal: &InferencePrincipal,
        operation: Operation,
        mode: TransportMode,
        required_target: Option<RequiredTarget>,
        admission: RequestAdmission,
    ) -> Result<RoutedUnaryResult, InferenceError> {
        let request_media = RequestMediaGuard::new(
            self.media_spool().clone(),
            operation_media_handles(&operation),
        );
        let result = self
            .execute_result_inner(principal, operation, mode, required_target, admission)
            .await;
        request_media.cleanup().await;
        result
    }

    /// Executes an autonomous media-job poll/delete against the provider and
    /// model recorded when the job was created. These operations have no live
    /// API-key principal or request-admission reservation, but retain the same
    /// runtime pinning, circuit, failover, and metadata accounting semantics.
    pub async fn execute_reconciliation_result(
        &self,
        api_key_id: uuid::Uuid,
        operation: Operation,
        surface: Surface,
        required_target: RequiredTarget,
    ) -> Result<Box<CanonicalResult>, InferenceError> {
        let request_media = RequestMediaGuard::new(
            self.media_spool().clone(),
            operation_media_handles(&operation),
        );
        let result = self
            .execute_reconciliation_result_inner(api_key_id, operation, surface, &required_target)
            .await;
        request_media.cleanup().await;
        result
    }

    async fn execute_reconciliation_result_inner(
        &self,
        api_key_id: uuid::Uuid,
        operation: Operation,
        surface: Surface,
        required_target: &RequiredTarget,
    ) -> Result<Box<CanonicalResult>, InferenceError> {
        let runtime = self.runtime().pin();
        let route_slug = operation
            .route()
            .cloned()
            .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
        let context = ExecutionContext {
            generation_id: runtime.generation.id.as_uuid(),
            api_key_id,
            request_id: RequestId::new(),
            route_slug,
            operation_kind: operation.kind(),
            request_started_at: Utc::now(),
            request_started: tokio::time::Instant::now(),
            surface,
        };
        let mut accounting =
            RequestAccountingGuard::new(self.clone(), context.accounting_input(), None, None, None);
        let attempts = match select_representable_attempts_filtered(
            &runtime,
            &context.route_slug,
            &operation,
            surface,
            TransportMode::Unary,
            context.request_id.as_uuid().as_bytes(),
            |_, target| {
                self.circuits().is_selectable(target.id)
                    && target.provider_id.as_uuid() == required_target.provider_id
                    && target.upstream_model == required_target.upstream_model
            },
        ) {
            Ok(attempts) => attempts,
            Err(error) => {
                let failure = if error.code() == "no_eligible_provider" {
                    InferenceError::unavailable("media_job_target_unavailable")
                } else {
                    error
                };
                accounting
                    .finish(RequestOutcome::from_error(&failure))
                    .await;
                return Err(failure);
            }
        };
        let route = runtime
            .routes
            .get(&context.route_slug)
            .expect("attempt selection returned a known route");
        let execution = {
            let mut record_attempt_started =
                |completed: &[RequestAttemptMetadata],
                 attempt: &olp_domain::AttemptPlan,
                 ordinal: u16,
                 started_at: chrono::DateTime<Utc>,
                 started: tokio::time::Instant| {
                    accounting.record_attempt_started(
                        completed,
                        ordinal,
                        attempt.provider_id.as_uuid(),
                        &attempt.upstream_model,
                        started_at,
                        started,
                    );
                };
            execute_with_failover(
                FailoverContext {
                    runtime: &runtime,
                    overall_timeout: route.overall_timeout.as_duration(),
                    media_spool: self.media_spool().clone(),
                    circuits: self.circuits(),
                    on_attempt_started: Some(&mut record_attempt_started),
                },
                attempts,
                RequestMetadata {
                    request_id: context.request_id,
                    operation: context.operation_kind,
                    surface,
                    mode: TransportMode::Unary,
                },
                operation,
            )
            .await
        };
        let success = match execution {
            Ok(success) => success,
            Err(failure) => {
                let error = failure.error;
                accounting.record_attempts(failure.attempts, None, None, false);
                accounting.finish(RequestOutcome::from_error(&error)).await;
                return Err(error);
            }
        };
        let first_byte_ms = elapsed_ms(context.request_started.elapsed());
        accounting.record_attempts(
            success.attempts,
            Some(success.attempt_started),
            Some(first_byte_ms),
            true,
        );
        let ExecutionOutput::Result(result) = success.output else {
            let failure = InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an event stream for a media-job operation.",
            );
            accounting
                .finish(RequestOutcome::from_error(&failure))
                .await;
            return Err(failure);
        };
        accounting.replace_usage(usage_from_result(&result));
        accounting.finish(RequestOutcome::success()).await;
        Ok(result)
    }

    async fn execute_result_inner(
        &self,
        principal: &InferencePrincipal,
        operation: Operation,
        mode: TransportMode,
        required_target: Option<RequiredTarget>,
        admission: RequestAdmission,
    ) -> Result<RoutedUnaryResult, InferenceError> {
        let CompletedExecution {
            context,
            success,
            mut accounting,
        } = self
            .execute_operation(principal, operation, mode, required_target, admission)
            .await?;
        let ExecutionSuccess {
            output,
            attempts,
            attempt_started,
            ..
        } = success;
        let first_byte_ms = elapsed_ms(context.request_started.elapsed());
        accounting.record_attempts(
            attempts.clone(),
            Some(attempt_started),
            Some(first_byte_ms),
            true,
        );
        let ExecutionOutput::Result(result) = output else {
            let failure = InferenceError::bad_gateway(
                "provider_protocol_error",
                "The provider returned an event stream for a unary result operation.",
            );
            accounting
                .finish(RequestOutcome::from_error(&failure))
                .await;
            return Err(failure);
        };
        accounting.replace_usage(usage_from_result(&result));
        accounting.release_limits().await;
        let finalizer = accounting.into_finalizer();
        let final_attempt = attempts
            .last()
            .expect("a successful execution has one provider attempt");
        Ok(RoutedUnaryResult {
            result,
            request_id: context.request_id,
            api_key_id: context.api_key_id,
            route_slug: context.route_slug,
            provider_id: final_attempt.provider_id,
            upstream_model: final_attempt.upstream_model.clone(),
            request_metadata_finalizer: Some(finalizer),
        })
    }

    async fn execute_operation(
        &self,
        principal: &InferencePrincipal,
        operation: Operation,
        mode: TransportMode,
        required_target: Option<RequiredTarget>,
        mut admission: RequestAdmission,
    ) -> Result<CompletedExecution, InferenceError> {
        let route_slug = operation
            .route()
            .cloned()
            .ok_or_else(|| InferenceError::invalid_request("A route model is required."))?;
        let context = ExecutionContext {
            generation_id: principal.runtime().generation.id.as_uuid(),
            api_key_id: principal.key().id.as_uuid(),
            request_id: RequestId::new(),
            route_slug,
            operation_kind: operation.kind(),
            request_started_at: Utc::now(),
            request_started: tokio::time::Instant::now(),
            surface: principal.surface(),
        };
        admission.claim_metadata();
        if let Err(error) = authorize_api_key(
            principal.key(),
            Some(&context.route_slug),
            context.operation_kind,
            Utc::now(),
        ) {
            let failure = InferenceError::forbidden(error.to_string());
            RequestAccountingGuard::new(
                self.clone(),
                context.accounting_input(),
                None,
                admission.reservation.take(),
                admission.reserved_tokens,
            )
            .finish(RequestOutcome::from_error(&failure))
            .await;
            return Err(failure);
        }
        let lease_ttl = principal
            .runtime()
            .routes
            .get(&context.route_slug)
            .map(|route| route.overall_timeout.as_duration())
            .unwrap_or(Duration::from_secs(30))
            .saturating_add(Duration::from_secs(30));
        let lease = match reserve_limits(
            self.limiter(),
            principal.key(),
            &operation,
            principal.lookup_id().as_str(),
            lease_ttl,
            admission.reserved_tokens,
        )
        .await
        {
            Ok(lease) => lease,
            Err(failure) => {
                RequestAccountingGuard::new(
                    self.clone(),
                    context.accounting_input(),
                    None,
                    admission.reservation.take(),
                    admission.reserved_tokens,
                )
                .finish(RequestOutcome::from_error(&failure))
                .await;
                return Err(failure);
            }
        };
        let mut accounting = RequestAccountingGuard::new(
            self.clone(),
            context.accounting_input(),
            lease,
            admission.reservation.take(),
            admission.reserved_tokens,
        );
        let attempts = match select_representable_attempts_filtered(
            principal.runtime(),
            &context.route_slug,
            &operation,
            principal.surface(),
            mode,
            context.request_id.as_uuid().as_bytes(),
            |_, target| {
                self.circuits().is_selectable(target.id)
                    && required_target.as_ref().is_none_or(|required| {
                        target.provider_id.as_uuid() == required.provider_id
                            && target.upstream_model == required.upstream_model
                    })
            },
        ) {
            Ok(attempts) => attempts,
            Err(error) => {
                let failure = if required_target.is_some() && error.code() == "no_eligible_provider"
                {
                    InferenceError::unavailable("media_job_target_unavailable")
                } else {
                    error
                };
                accounting
                    .finish(RequestOutcome::from_error(&failure))
                    .await;
                return Err(failure);
            }
        };
        let route = principal
            .runtime()
            .routes
            .get(&context.route_slug)
            .expect("attempt selection returned a known route");
        let execution = {
            let mut record_attempt_started =
                |completed: &[RequestAttemptMetadata],
                 attempt: &olp_domain::AttemptPlan,
                 ordinal: u16,
                 started_at: chrono::DateTime<Utc>,
                 started: tokio::time::Instant| {
                    accounting.record_attempt_started(
                        completed,
                        ordinal,
                        attempt.provider_id.as_uuid(),
                        &attempt.upstream_model,
                        started_at,
                        started,
                    );
                };
            execute_with_failover(
                FailoverContext {
                    runtime: principal.runtime(),
                    overall_timeout: route.overall_timeout.as_duration(),
                    media_spool: self.media_spool().clone(),
                    circuits: self.circuits(),
                    on_attempt_started: Some(&mut record_attempt_started),
                },
                attempts,
                RequestMetadata {
                    request_id: context.request_id,
                    operation: context.operation_kind,
                    surface: principal.surface(),
                    mode,
                },
                operation,
            )
            .await
        };
        match execution {
            Ok(success) => Ok(CompletedExecution {
                context,
                success,
                accounting,
            }),
            Err(failure) => {
                let error = failure.error;
                accounting.record_attempts(failure.attempts, None, None, false);
                accounting.finish(RequestOutcome::from_error(&error)).await;
                Err(error)
            }
        }
    }

    pub fn authorize_model_access<'a>(
        &self,
        principal: &'a InferencePrincipal,
        operation: OperationKind,
    ) -> Result<(&'a RuntimeBundle, &'a ApiKey), InferenceError> {
        let key = self.authorize_principal(principal, operation, None)?;
        Ok((principal.runtime(), key))
    }

    pub async fn reserve_model_limits(
        &self,
        principal: &InferencePrincipal,
        admission_reserved_tokens: Option<i64>,
    ) -> Result<Option<LimitLease>, InferenceError> {
        let operation = Operation::Models(olp_domain::ModelOperation::List {
            extensions: olp_domain::SourceExtensions::new(principal.surface(), BTreeMap::new()),
        });
        reserve_limits(
            self.limiter(),
            principal.key(),
            &operation,
            principal.lookup_id().as_str(),
            Duration::from_secs(30),
            admission_reserved_tokens,
        )
        .await
    }

    pub async fn release_model_limits(&self, lease: Option<&LimitLease>) {
        release_limits(self.limiter(), lease, None).await;
    }
}
