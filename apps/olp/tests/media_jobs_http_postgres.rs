use std::{
    collections::{BTreeMap, BTreeSet},
    num::{NonZeroU16, NonZeroU32},
    path::PathBuf,
    sync::Arc,
    sync::atomic::{AtomicBool, AtomicUsize, Ordering},
};

use axum::{
    body::Body,
    http::{Request, StatusCode, header},
};
use http_body_util::BodyExt as _;
use olp::{ApiMode, ApiState, RuntimeManager, public_router, reconcile_media_jobs_once};
use olp_domain::{
    ApiKey, ApiKeyDigest, ApiKeyId, ApiKeyLimits, ApiKeyLookupId, ApiKeyScope, ApiKeyStatus,
    BoxFuture, CanonicalResult, Capability, DurationMs, MediaSpool, MediaUpload, Operation,
    OperationKind, Provider, ProviderId, ProviderKind, ProviderOutput, ProviderRequest,
    ProviderTransport, Route, RouteId, RouteSlug, RuntimeGeneration, RuntimeGenerationId,
    RuntimeSnapshot, SourceExtensions, Surface, Target, TargetId, TransportError, TransportMode,
    VideoContentResult, VideoDeleteResult, VideoJobResult, VideoOperation, VideoStatus,
};
use olp_storage::{KeyHasher, MediaJobState, MediaJobUpdate, NewMediaJobReservation, PgStore};
use serde_json::{Value, json};
use tower::ServiceExt as _;
use uuid::Uuid;

mod common;
use common::{BOOTSTRAP_TOKEN, configure_bootstrap};

const ORIGIN: &str = "https://olp.example.test";

#[derive(Clone)]
struct VideoLifecycleTransport {
    spool: Arc<dyn MediaSpool>,
    create_calls: Arc<AtomicUsize>,
    delete_calls: Arc<AtomicUsize>,
    fail_cleanup: Arc<AtomicBool>,
}

impl ProviderTransport for VideoLifecycleTransport {
    fn execute<'a>(
        &'a self,
        request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let spool = self.spool.clone();
        Box::pin(async move {
            let result = match request.operation {
                Operation::Video(VideoOperation::Create(_)) => {
                    let ordinal = self.create_calls.fetch_add(1, Ordering::AcqRel) + 1;
                    let id = if ordinal == 1 {
                        "upstream-video-created".to_owned()
                    } else {
                        format!("upstream-video-created-{ordinal}")
                    };
                    CanonicalResult::VideoJob(video_job(&id, VideoStatus::Queued))
                }
                Operation::Video(VideoOperation::Get(operation)) => {
                    assert_eq!(operation.job_id, "upstream-video-created");
                    CanonicalResult::VideoJob(video_job(
                        "upstream-video-created",
                        VideoStatus::Completed,
                    ))
                }
                Operation::Video(VideoOperation::Content(operation)) => {
                    assert_eq!(operation.job_id, "upstream-video-created");
                    let artifact = spool
                        .put(MediaUpload {
                            filename: "video.mp4".into(),
                            content_type: Some("video/mp4".into()),
                            maximum_length: 64,
                            bytes: Box::pin(futures::stream::once(async {
                                Ok(bytes::Bytes::from_static(b"video-content"))
                            })),
                        })
                        .await
                        .unwrap();
                    CanonicalResult::VideoContent(VideoContentResult {
                        media: artifact,
                        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
                    })
                }
                Operation::Video(VideoOperation::Delete(operation)) => {
                    assert!(operation.job_id.starts_with("upstream-video-created"));
                    assert_eq!(
                        operation
                            .extensions
                            .values
                            .get(olp_domain::MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION),
                        Some(&serde_json::Value::Bool(true))
                    );
                    self.delete_calls.fetch_add(1, Ordering::AcqRel);
                    if self.fail_cleanup.load(Ordering::Acquire)
                        && operation.job_id != "upstream-video-created"
                    {
                        return Err(TransportError {
                            phase: olp_domain::TransportPhase::FirstByte,
                            class: olp_domain::AttemptFailureClass::Ambiguous,
                            response_committed: true,
                            message: "injected cleanup ambiguity".to_owned(),
                        });
                    }
                    CanonicalResult::VideoDelete(VideoDeleteResult {
                        id: operation.job_id,
                        deleted: true,
                        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
                    })
                }
                operation => panic!(
                    "unexpected video lifecycle operation: {:?}",
                    operation.kind()
                ),
            };
            Ok(ProviderOutput::Result(Box::new(result)))
        })
    }
}

fn video_job(id: &str, status: VideoStatus) -> VideoJobResult {
    VideoJobResult {
        id: id.into(),
        model: Some("upstream-video-model".into()),
        status,
        progress_percent: Some(100.0),
        created_at: Some(1_800_000_000),
        completed_at: None,
        expires_at: None,
        prompt: None,
        seconds: Some("8".into()),
        size: Some("1280x720".into()),
        error: None,
        extensions: SourceExtensions::new(Surface::OpenAi, BTreeMap::new()),
    }
}

#[tokio::test]
#[ignore = "requires an empty PostgreSQL 18 database in OLP_TEST_DATABASE_URL"]
async fn media_job_management_views_are_session_authorized_and_metadata_only() {
    let database_url = std::env::var("OLP_TEST_DATABASE_URL")
        .expect("OLP_TEST_DATABASE_URL must point to an empty PostgreSQL 18 database");
    let store = PgStore::connect(&database_url, 5).await.unwrap();
    store.migrate().await.unwrap();
    let mut state = ApiState::new(
        ApiMode::Control,
        Some(store.clone()),
        Arc::new(RuntimeManager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-media-job-test"),
    );
    configure_bootstrap(&mut state, [18; 32]);
    let app = public_router(state);

    let mut setup_request = Request::post("/api/v1/setup")
        .header(header::CONTENT_TYPE, "application/json")
        .header(header::ORIGIN, ORIGIN)
        .header("x-olp-setup-token", BOOTSTRAP_TOKEN)
        .body(Body::from(
            serde_json::to_vec(&json!({
                "email": "owner@example.test",
                "password": "correct horse battery staple",
                "display_name": "Owner",
                "organization_name": "Media job HTTP test"
            }))
            .unwrap(),
        ))
        .unwrap();
    setup_request
        .extensions_mut()
        .insert(axum::extract::ConnectInfo(
            "198.51.100.14:443".parse::<std::net::SocketAddr>().unwrap(),
        ));
    let setup = app.clone().oneshot(setup_request).await.unwrap();
    assert_eq!(setup.status(), StatusCode::CREATED);
    let cookie = setup
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .find_map(|value| {
            value
                .to_str()
                .ok()?
                .split(';')
                .next()
                .filter(|cookie| cookie.starts_with("__Host-olp_session="))
                .map(str::to_owned)
        })
        .unwrap();
    let setup_body: Value =
        serde_json::from_slice(&setup.into_body().collect().await.unwrap().to_bytes()).unwrap();
    let owner_id = Uuid::parse_str(setup_body["user"]["id"].as_str().unwrap()).unwrap();

    let provider_id = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers
         (id, name, kind, state, auth_mode, etag, created_by)
         VALUES ($1, 'media-provider', 'openai', 'active', 'api_key', $2, $3)",
    )
    .bind(provider_id)
    .bind(Uuid::now_v7())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv2media02', $2, 'media test', $3)",
    )
    .bind(api_key_id)
    .bind([8_u8; 32].as_slice())
    .bind(owner_id)
    .execute(store.pool())
    .await
    .unwrap();
    let job_id = Uuid::now_v7();
    store
        .reserve_media_job(NewMediaJobReservation {
            id: job_id,
            runtime_generation_id: Uuid::now_v7(),
            api_key_id,
            provider_id,
            provider_model: "video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "open_ai".parse().unwrap(),
        })
        .await
        .unwrap();
    let job = store
        .attach_media_job_upstream(
            job_id,
            "upstream-video-http",
            MediaJobUpdate {
                state: MediaJobState::Queued,
                progress_percent: Some(0.0),
                content_available: false,
                expires_at: None,
                error_class: None,
                last_polled_at: chrono::Utc::now(),
            },
        )
        .await
        .unwrap();

    let list = app
        .clone()
        .oneshot(
            Request::get("/api/v1/media-jobs?state=queued&limit=50")
                .header(header::COOKIE, &cookie)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(list.status(), StatusCode::OK);
    let list_body: Value =
        serde_json::from_slice(&list.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(list_body["data"][0]["id"], job.id.to_string());
    assert_eq!(list_body["data"][0]["surface"], "openai");
    assert_eq!(list_body["data"][0]["state"], "queued");
    assert_eq!(list_body["data"][0]["lifecycle"], "active");
    assert!(list_body["data"][0].get("prompt").is_none());
    assert!(list_body["data"][0].get("content").is_none());

    let detail = app
        .clone()
        .oneshot(
            Request::get(format!("/api/v1/media-jobs/{}", job.id))
                .header(header::COOKIE, cookie)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(detail.status(), StatusCode::OK);
    assert!(detail.headers().contains_key(header::ETAG));
    let detail_body: Value =
        serde_json::from_slice(&detail.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(detail_body["surface"], "openai");

    let key_hasher = Arc::new(KeyHasher::new([19; 32]));
    let material = key_hasher.generate_api_key();
    let plaintext_key = material.expose_once().to_owned();
    let lookup_id = ApiKeyLookupId::parse(material.lookup_id.clone()).unwrap();
    let core_provider_id = ProviderId::from_uuid(provider_id);
    let route_slug = RouteSlug::parse("video-default").unwrap();
    let operations = BTreeSet::from([
        OperationKind::VideoCreate,
        OperationKind::VideoList,
        OperationKind::VideoGet,
        OperationKind::VideoContent,
        OperationKind::VideoDelete,
    ]);
    let capabilities = BTreeSet::from([
        Capability::new(
            "upstream-video-model",
            OperationKind::VideoCreate,
            Surface::OpenAi,
            TransportMode::Async,
        ),
        Capability::new(
            "upstream-video-model",
            OperationKind::VideoGet,
            Surface::OpenAi,
            TransportMode::Unary,
        ),
        Capability::new(
            "upstream-video-model",
            OperationKind::VideoList,
            Surface::OpenAi,
            TransportMode::Unary,
        ),
        Capability::new(
            "upstream-video-model",
            OperationKind::VideoContent,
            Surface::OpenAi,
            TransportMode::Unary,
        ),
        Capability::new(
            "upstream-video-model",
            OperationKind::VideoDelete,
            Surface::OpenAi,
            TransportMode::Unary,
        ),
    ]);
    let runtime = Arc::new(RuntimeManager::empty());
    let mut gateway_state = ApiState::new(
        ApiMode::Gateway,
        Some(store.clone()),
        runtime.clone(),
        ORIGIN,
        PathBuf::from("missing-console-for-video-inference-test"),
    );
    gateway_state.key_hasher = Some(key_hasher);
    let delete_calls = Arc::new(AtomicUsize::new(0));
    let create_calls = Arc::new(AtomicUsize::new(0));
    let fail_cleanup = Arc::new(AtomicBool::new(false));
    let transport: Arc<dyn ProviderTransport> = Arc::new(VideoLifecycleTransport {
        spool: gateway_state.media_spool.clone(),
        create_calls: create_calls.clone(),
        delete_calls: delete_calls.clone(),
        fail_cleanup: fail_cleanup.clone(),
    });
    runtime
        .install(
            RuntimeSnapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: 1,
                    activated_at: chrono::Utc::now(),
                },
                providers: BTreeMap::from([(
                    core_provider_id,
                    Provider {
                        id: core_provider_id,
                        name: "video-provider".into(),
                        kind: ProviderKind::OpenAi,
                        enabled: true,
                        active_credential: None,
                        capabilities,
                    },
                )]),
                routes: BTreeMap::from([(
                    route_slug.clone(),
                    Route {
                        id: RouteId::new(),
                        routing_id: None,
                        slug: route_slug.clone(),
                        operations,
                        overall_timeout: DurationMs::new(5_000),
                        max_attempts: NonZeroU16::new(1).unwrap(),
                        targets: vec![Target {
                            id: TargetId::new(),
                            routing_id: None,
                            provider_id: core_provider_id,
                            provider_model: "upstream-video-model".into(),
                            priority: 0,
                            weight: NonZeroU32::new(1).unwrap(),
                            timeout: DurationMs::new(4_000),
                        }],
                    },
                )]),
                api_keys: BTreeMap::from([(
                    lookup_id.clone(),
                    ApiKey {
                        id: ApiKeyId::from_uuid(api_key_id),
                        lookup_id,
                        digest: ApiKeyDigest::new(material.digest),
                        status: ApiKeyStatus::Active,
                        expires_at: None,
                        scopes: BTreeSet::from([ApiKeyScope::Inference]),
                        allowed_routes: BTreeSet::new(),
                        limits: ApiKeyLimits::default(),
                    },
                )]),
            },
            BTreeMap::from([(core_provider_id, transport)]),
        )
        .unwrap();
    let reconciliation_state = gateway_state.clone();
    let gateway = public_router(gateway_state);
    let authorization = format!("Bearer {plaintext_key}");
    let create_body = concat!(
        "--video-boundary\r\n",
        "Content-Disposition: form-data; name=\"model\"\r\n\r\n",
        "video-default\r\n",
        "--video-boundary\r\n",
        "Content-Disposition: form-data; name=\"prompt\"\r\n\r\n",
        "private prompt that must not persist\r\n",
        "--video-boundary--\r\n"
    );
    let create = gateway
        .clone()
        .oneshot(
            Request::post("/openai/v1/videos")
                .header(header::AUTHORIZATION, &authorization)
                .header(
                    header::CONTENT_TYPE,
                    "multipart/form-data; boundary=video-boundary",
                )
                .body(Body::from(create_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(create.status(), StatusCode::CREATED);
    let created: Value =
        serde_json::from_slice(&create.into_body().collect().await.unwrap().to_bytes()).unwrap();
    let video_id = created["id"].as_str().unwrap().to_owned();
    assert_eq!(created["model"], "video-default");
    assert_ne!(video_id, "upstream-video-created");

    let videos = gateway
        .clone()
        .oneshot(
            Request::get("/openai/v1/videos?limit=20&order=desc")
                .header(header::AUTHORIZATION, &authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(videos.status(), StatusCode::OK);
    let videos: Value =
        serde_json::from_slice(&videos.into_body().collect().await.unwrap().to_bytes()).unwrap();
    let listed = videos["data"]
        .as_array()
        .unwrap()
        .iter()
        .find(|item| item["id"].as_str() == Some(video_id.as_str()))
        .unwrap();
    assert_eq!(listed["model"], "video-default");
    assert_eq!(listed["status"], "completed");
    assert!(listed.get("prompt").is_none() || listed["prompt"].is_null());

    let status = gateway
        .clone()
        .oneshot(
            Request::get(format!("/openai/v1/videos/{video_id}"))
                .header(header::AUTHORIZATION, &authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(status.status(), StatusCode::OK);
    let status: Value =
        serde_json::from_slice(&status.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(status["id"], video_id);
    assert_eq!(status["model"], "video-default");
    assert_eq!(status["status"], "completed");

    let content = gateway
        .clone()
        .oneshot(
            Request::get(format!("/openai/v1/videos/{video_id}/content"))
                .header(header::AUTHORIZATION, &authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(content.status(), StatusCode::OK);
    assert_eq!(content.headers()[header::CONTENT_TYPE], "video/mp4");
    assert_eq!(
        content.into_body().collect().await.unwrap().to_bytes(),
        bytes::Bytes::from_static(b"video-content")
    );

    sqlx::query(
        "CREATE FUNCTION fail_test_media_finalize() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN
             IF NEW.lifecycle_state = 'deleted' THEN
                 RAISE EXCEPTION 'injected finalization failure';
             END IF;
             RETURN NEW;
         END;
         $$",
    )
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER fail_test_media_finalize
         BEFORE UPDATE ON async_media_jobs
         FOR EACH ROW EXECUTE FUNCTION fail_test_media_finalize()",
    )
    .execute(store.pool())
    .await
    .unwrap();
    let ambiguous_delete = gateway
        .clone()
        .oneshot(
            Request::delete(format!("/openai/v1/videos/{video_id}"))
                .header(header::AUTHORIZATION, &authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(ambiguous_delete.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(delete_calls.load(Ordering::Acquire), 1);
    assert_eq!(
        store
            .media_job(Uuid::parse_str(&video_id).unwrap())
            .await
            .unwrap()
            .lifecycle,
        olp_storage::MediaJobLifecycle::DeletePending
    );
    sqlx::query("DROP TRIGGER fail_test_media_finalize ON async_media_jobs")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query("DROP FUNCTION fail_test_media_finalize()")
        .execute(store.pool())
        .await
        .unwrap();

    // The retry models an upstream 404 after the first delete succeeded. The
    // durable delete intent permits the transport to reconcile it as success.
    let deleted = gateway
        .clone()
        .oneshot(
            Request::delete(format!("/openai/v1/videos/{video_id}"))
                .header(header::AUTHORIZATION, &authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(deleted.status(), StatusCode::OK);
    assert_eq!(delete_calls.load(Ordering::Acquire), 2);
    let deleted: Value =
        serde_json::from_slice(&deleted.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(deleted["id"], video_id);
    assert_eq!(deleted["deleted"], true);

    let repeated_delete = gateway
        .clone()
        .oneshot(
            Request::delete(format!("/openai/v1/videos/{video_id}"))
                .header(header::AUTHORIZATION, &authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(repeated_delete.status(), StatusCode::OK);
    assert_eq!(delete_calls.load(Ordering::Acquire), 2);
    let repeated_delete: Value = serde_json::from_slice(
        &repeated_delete
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes(),
    )
    .unwrap();
    assert_eq!(repeated_delete["deleted"], true);

    sqlx::query(
        "CREATE FUNCTION fail_test_media_attach() RETURNS trigger LANGUAGE plpgsql AS $$
         BEGIN
             IF OLD.lifecycle_state = 'creating' AND NEW.lifecycle_state = 'active' THEN
                 RAISE EXCEPTION 'injected attach failure';
             END IF;
             RETURN NEW;
         END;
         $$",
    )
    .execute(store.pool())
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER fail_test_media_attach
         BEFORE UPDATE ON async_media_jobs
         FOR EACH ROW EXECUTE FUNCTION fail_test_media_attach()",
    )
    .execute(store.pool())
    .await
    .unwrap();
    let compensated_create = gateway
        .clone()
        .oneshot(
            Request::post("/openai/v1/videos")
                .header(header::AUTHORIZATION, &authorization)
                .header(
                    header::CONTENT_TYPE,
                    "multipart/form-data; boundary=video-boundary",
                )
                .body(Body::from(create_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(compensated_create.status(), StatusCode::SERVICE_UNAVAILABLE);
    let compensated_lifecycle: String = sqlx::query_scalar(
        "SELECT lifecycle_state FROM async_media_jobs
         WHERE upstream_job_id = 'upstream-video-created-2'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(compensated_lifecycle, "deleted");

    fail_cleanup.store(true, Ordering::Release);
    let unresolved_create = gateway
        .clone()
        .oneshot(
            Request::post("/openai/v1/videos")
                .header(header::AUTHORIZATION, &authorization)
                .header(
                    header::CONTENT_TYPE,
                    "multipart/form-data; boundary=video-boundary",
                )
                .body(Body::from(create_body))
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(unresolved_create.status(), StatusCode::SERVICE_UNAVAILABLE);
    let unresolved_lifecycle: String = sqlx::query_scalar(
        "SELECT lifecycle_state FROM async_media_jobs
         WHERE upstream_job_id = 'upstream-video-created-3'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(unresolved_lifecycle, "create_cleanup_pending");
    sqlx::query("DROP TRIGGER fail_test_media_attach ON async_media_jobs")
        .execute(store.pool())
        .await
        .unwrap();
    sqlx::query("DROP FUNCTION fail_test_media_attach()")
        .execute(store.pool())
        .await
        .unwrap();

    // Revoke the creating key before the bounded autonomous pass. Lifecycle
    // authority is the durable job target, not a still-valid client secret.
    sqlx::query("UPDATE api_keys SET revoked_at = now() WHERE id = $1")
        .bind(api_key_id)
        .execute(store.pool())
        .await
        .unwrap();
    let current = runtime.pin();
    runtime
        .install(
            RuntimeSnapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: 2,
                    activated_at: chrono::Utc::now(),
                },
                providers: current.providers.clone(),
                routes: current.routes.clone(),
                api_keys: BTreeMap::new(),
            },
            BTreeMap::from([(
                core_provider_id,
                current.transport(core_provider_id).unwrap(),
            )]),
        )
        .unwrap();
    drop(current);
    fail_cleanup.store(false, Ordering::Release);
    let pass = reconcile_media_jobs_once(&reconciliation_state, 8)
        .await
        .unwrap();
    assert!(pass.claimed >= 1);
    assert!(pass.completed >= 1);
    let reconciled_lifecycle: String = sqlx::query_scalar(
        "SELECT lifecycle_state FROM async_media_jobs
         WHERE upstream_job_id = 'upstream-video-created-3'",
    )
    .fetch_one(store.pool())
    .await
    .unwrap();
    assert_eq!(reconciled_lifecycle, "deleted");

    let missing = gateway
        .oneshot(
            Request::get(format!("/openai/v1/videos/{video_id}"))
                .header(header::AUTHORIZATION, authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(missing.status(), StatusCode::UNAUTHORIZED);
}
