use crate::access::{
    permissions::require_permission,
    policy::Permission,
    principal::{MutationPrincipal, ReadPrincipal},
};
use crate::http::{
    control::{
        idempotency::{MutationReply, ReplayableMutation},
        preconditions::{if_match, with_etag},
        provenance::Provenance,
        state::ManagementState,
    },
    problem::Problem,
};
use crate::routes::policy::{RoutingPolicy, RoutingPreferences};
use axum::{
    Json,
    extract::{Path, State},
    http::{HeaderMap, StatusCode},
    response::Response,
};
use serde::{Deserialize, Serialize};
use utoipa::ToSchema;
use uuid::Uuid;

#[derive(Serialize, ToSchema)]
pub struct PolicyResponse {
    pub policy: RoutingPolicy,
    pub etag: Uuid,
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Scope {
    Installation,
    RouteDraft,
    ApiKey,
}

impl Scope {
    fn parse(scope: &str) -> Result<Self, Problem> {
        match scope {
            "installation" => Ok(Self::Installation),
            "route-draft" => Ok(Self::RouteDraft),
            "api-key" => Ok(Self::ApiKey),
            _ => Err(Problem::bad_request(
                "invalid_policy_scope",
                "Use installation, route-draft, or api-key",
            )),
        }
    }

    fn read_sql(self) -> &'static str {
        match self {
            Self::Installation => "SELECT policy,etag FROM routing_settings WHERE singleton",
            Self::RouteDraft => "SELECT routing_policy,etag FROM route_drafts WHERE id=$1",
            Self::ApiKey => "SELECT routing_policy,etag FROM api_keys WHERE id=$1",
        }
    }

    fn lock_sql(self) -> &'static str {
        match self {
            Self::Installation => {
                "SELECT policy,etag FROM routing_settings WHERE singleton FOR UPDATE"
            }
            Self::RouteDraft => {
                "SELECT routing_policy,etag FROM route_drafts WHERE id=$1 FOR UPDATE"
            }
            Self::ApiKey => "SELECT routing_policy,etag FROM api_keys WHERE id=$1 FOR UPDATE",
        }
    }

    fn write_sql(self) -> &'static str {
        match self {
            Self::Installation => "UPDATE routing_settings SET policy=$1,etag=$2 WHERE singleton",
            Self::RouteDraft => {
                "UPDATE route_drafts SET routing_policy=$1,etag=$2,state='draft',updated_at=now() WHERE id=$3"
            }
            Self::ApiKey => "UPDATE api_keys SET routing_policy=$1,etag=$2 WHERE id=$3",
        }
    }

    fn permission(self) -> Permission {
        match self {
            Self::Installation => Permission::ManageSettings,
            Self::RouteDraft => Permission::ManageRoutes,
            Self::ApiKey => Permission::ManageApiKeys,
        }
    }

    /// The installation policy is a singleton; every other scope is keyed by id.
    fn binds_id(self) -> bool {
        self != Self::Installation
    }
}

fn not_found() -> Problem {
    Problem::new(
        StatusCode::NOT_FOUND,
        "policy_not_found",
        "Not found",
        "Routing policy not found",
    )
}

/// Reads the stored policy and etag for one scope, binding the id where the
/// scope's SQL expects it.
async fn fetch_policy<'e>(
    executor: impl sqlx::PgExecutor<'e>,
    sql: &'static str,
    scope: Scope,
    id: Uuid,
) -> Result<(RoutingPolicy, Uuid), Problem> {
    let mut query = sqlx::query_as::<_, (sqlx::types::Json<RoutingPolicy>, Uuid)>(sql);
    if scope.binds_id() {
        query = query.bind(id);
    }
    query
        .fetch_optional(executor)
        .await
        .map_err(|_| Problem::internal())?
        .map(|(policy, etag)| (policy.0, etag))
        .ok_or_else(not_found)
}

#[utoipa::path(get,path="/api/v3/routing-policies/{scope}/{id}",tag="routes",params(("scope"=String,Path),("id"=Uuid,Path)),responses((status=200,body=PolicyResponse)),security(("sessionCookie"=[])))]
pub(crate) async fn get_routing_policy(
    State(state): State<ManagementState>,
    Path((scope, id)): Path<(String, Uuid)>,
    ReadPrincipal(principal): ReadPrincipal,
) -> Result<Response, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    let scope = Scope::parse(&scope)?;
    let (policy, etag) =
        fetch_policy(&state.request_boundary.pool, scope.read_sql(), scope, id).await?;
    with_etag(Json(PolicyResponse { policy, etag }), etag)
}

#[utoipa::path(put,path="/api/v3/routing-policies/{scope}/{id}",tag="routes",params(("scope"=String,Path),("id"=Uuid,Path)),request_body=RoutingPolicy,responses((status=200,body=PolicyResponse)),security(("sessionCookie"=[],"csrfToken"=[])))]
pub(crate) async fn put_routing_policy(
    State(state): State<ManagementState>,
    Path((scope, id)): Path<(String, Uuid)>,
    headers: HeaderMap,
    Provenance(provenance): Provenance,
    MutationPrincipal(principal): MutationPrincipal,
    Json(policy): Json<RoutingPolicy>,
) -> Result<Response, Problem> {
    let policy_scope = Scope::parse(&scope)?;
    require_permission(&principal, policy_scope.permission())?;
    policy
        .validate()
        .map_err(|e| Problem::field_validation("policy", e))?;
    let expected = if_match(&headers)?;
    let fingerprint = serde_json::json!({"scope":scope,"id":id,"etag":expected,"policy":policy});
    ReplayableMutation::new(
        &state,
        principal.user_id,
        "routing_policy.put",
        &headers,
        &fingerprint,
    )?
    .run(|_| async {
        let mut tx = state
            .request_boundary
            .pool
            .begin()
            .await
            .map_err(|_| Problem::internal())?;
        crate::runtime::publication::compiler::prepare_runtime_mutation(&mut tx)
            .await
            .map_err(|_| Problem::internal())?;
        let (_, etag) = fetch_policy(&mut *tx, policy_scope.lock_sql(), policy_scope, id).await?;
        if etag != expected {
            return Err(crate::http::control::error_mapping::map_configuration(
                crate::providers::error::Error::PreconditionFailed,
            ));
        }
        let etag = Uuid::now_v7();
        let mut query = sqlx::query(policy_scope.write_sql())
            .bind(sqlx::types::Json(&policy))
            .bind(etag);
        if policy_scope.binds_id() {
            query = query.bind(id);
        }
        query
            .execute(&mut *tx)
            .await
            .map_err(|_| Problem::internal())?;
        crate::access::audit_events::record_success(
            &mut *tx,
            &provenance,
            principal.user_id,
            "routing_policy.put",
            "routing_policy",
            id,
        )
        .await
        .map_err(|_| Problem::internal())?;
        if policy_scope != Scope::RouteDraft {
            crate::runtime::publication::compiler::compile_and_publish_runtime_in_transaction(
                &mut tx,
                principal.user_id,
            )
            .await
            .map_err(|_| Problem::internal())?;
        }
        tx.commit().await.map_err(|_| Problem::internal())?;
        Ok(MutationReply {
            status: StatusCode::OK,
            body: PolicyResponse {
                policy: policy.clone(),
                etag,
            },
            etag: Some(etag),
            location: None,
        })
    })
    .await
}

#[derive(Deserialize, ToSchema)]
#[serde(deny_unknown_fields)]
pub struct SimulationRequest {
    #[schema(value_type=Object)]
    pub operation: crate::protocols::canonical::requests::Operation,
    pub surface: crate::protocols::canonical::identity::Surface,
    pub mode: crate::protocols::canonical::identity::TransportMode,
    #[serde(default)]
    pub preferences: RoutingPreferences,
    pub api_key_id: Option<Uuid>,
    #[serde(default)]
    #[schema(max_length = 256)]
    pub seed: String,
}

#[utoipa::path(post,path="/api/v3/routing/simulate",tag="routes",request_body=SimulationRequest,responses((status=200,body=Vec<crate::inference::provider_selection::RoutingDecision>)),security(("sessionCookie"=[],"csrfToken"=[])))]
pub(crate) async fn simulate_routing(
    State(state): State<ManagementState>,
    ReadPrincipal(principal): ReadPrincipal,
    Json(request): Json<SimulationRequest>,
) -> Result<Json<Vec<crate::inference::provider_selection::RoutingDecision>>, Problem> {
    require_permission(&principal, Permission::ReadConfiguration)?;
    if request.seed.len() > 256 {
        return Err(Problem::field_validation("seed", "Use at most 256 bytes"));
    }
    let route = request.operation.route().ok_or_else(|| {
        Problem::bad_request("route_required", "Simulation requires a routed operation")
    })?;
    let runtime = state.request_boundary.inference.runtime.pin();
    let key = match request.api_key_id {
        Some(id) => Some(
            runtime
                .api_keys
                .values()
                .find(|k| k.id.as_uuid() == id)
                .ok_or_else(|| {
                    Problem::new(
                        StatusCode::NOT_FOUND,
                        "key_not_found",
                        "Not found",
                        "API key not found",
                    )
                })?,
        ),
        None => None,
    };
    if let Some(key) = key {
        crate::access::policy::authorize_api_key(
            key,
            Some(route),
            Some(crate::access::policy::gateway_capability_for_operation(
                request.operation.kind(),
            )),
            crate::access::policy::gateway_capability_for_operation(request.operation.kind()),
            chrono::Utc::now(),
        )
        .map_err(|_| Problem::forbidden("route_forbidden", "API key cannot access this route"))?;
    }
    let preferences = state
        .request_boundary
        .inference
        .routing_preferences(&runtime, route, &request.preferences)
        .await;
    let selection = crate::inference::provider_selection::explain(
        &runtime,
        route,
        &request.operation,
        request.surface,
        request.mode,
        request.seed.as_bytes(),
        key,
        &preferences,
        |_, target| {
            state
                .request_boundary
                .inference
                .circuits
                .is_selectable(target.routing_id)
        },
    )
    .map_err(|e| Problem::bad_request(e.code(), e.message()))?;
    Ok(Json(selection.decisions))
}
