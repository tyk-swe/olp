use std::collections::BTreeMap;
use std::collections::BTreeSet;
use std::num::NonZeroU16;
use std::num::NonZeroU32;
use std::path::PathBuf;
use std::sync::Arc;
use std::sync::atomic::AtomicBool;
use std::sync::atomic::AtomicUsize;
use std::sync::atomic::Ordering;

use axum::body::Body;
use axum::http::Request;
use axum::http::StatusCode;
use axum::http::header;
use http_body_util::BodyExt as _;
use olp::access::policy::ApiKey;
use olp::access::policy::ApiKeyDigest;
use olp::access::policy::ApiKeyLimits;
use olp::access::policy::ApiKeyScope;
use olp::access::policy::ApiKeyStatus;
use olp::crypto::key_material::AuthHmacKey;
use olp::http::router::gateway_router_for_test;
use olp::http::router::management_router_for_test;
use olp::ids::ApiKeyId;
use olp::ids::ApiKeyLookupId;
use olp::ids::DurationMs;
use olp::ids::ProviderId;
use olp::ids::RouteId;
use olp::ids::RouteSlug;
use olp::ids::RuntimeGenerationId;
use olp::ids::TargetId;
use olp::inference::transport::BoxFuture;
use olp::inference::transport::MediaSpool;
use olp::inference::transport::MediaUpload;
use olp::inference::transport::ProviderOutput;
use olp::inference::transport::ProviderRequest;
use olp::inference::transport::ProviderTransport;
use olp::inference::transport::TransportError;
use olp::media::jobs::MediaJobState;
use olp::media::jobs::MediaJobUpdate;
use olp::media::jobs::NewMediaJobReservation;
use olp::media::service::reconcile_media_jobs_once;
use olp::process::mode::ApiMode;
use olp::process::state::ProcessComposition;
use olp::protocols::canonical::identity::OperationKind;
use olp::protocols::canonical::identity::Surface;
use olp::protocols::canonical::identity::TransportMode;
use olp::protocols::canonical::requests::Operation;
use olp::protocols::canonical::requests::SourceExtensions;
use olp::protocols::canonical::requests::VideoOperation;
use olp::protocols::canonical::results::CanonicalResult;
use olp::protocols::canonical::results::VideoContentResult;
use olp::protocols::canonical::results::VideoDeleteResult;
use olp::protocols::canonical::results::VideoJobResult;
use olp::protocols::canonical::results::VideoStatus;
use olp::providers::runtime_model::Capability;
use olp::providers::runtime_model::Provider;
use olp::providers::runtime_model::ProviderKind;
use olp::routes::model::Route;
use olp::routes::model::Target;
use olp::runtime::manager::Manager;
use olp::runtime::snapshot::RuntimeGeneration;
use olp::runtime::snapshot::Snapshot;
use serde_json::Value;
use serde_json::json;
use sha2::Digest as _;
use sha2::Sha256;
use tower::ServiceExt as _;
use uuid::Uuid;

use crate::common::BOOTSTRAP_TOKEN;
use crate::common::configure_bootstrap;

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
            let result = match &*request.operation {
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
                    assert!(matches!(
                        operation.job_id.as_str(),
                        "upstream-video-created" | "upstream-video-http"
                    ));
                    CanonicalResult::VideoJob(video_job(&operation.job_id, VideoStatus::Completed))
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
                            .get(
                                olp::protocols::canonical::requests::MEDIA_DELETE_MISSING_IS_SUCCESS_EXTENSION,
                            ),
                        Some(&serde_json::Value::Bool(true))
                    );
                    self.delete_calls.fetch_add(1, Ordering::AcqRel);
                    if self.fail_cleanup.load(Ordering::Acquire)
                        && operation.job_id != "upstream-video-created"
                    {
                        return Err(TransportError {
                            upstream: Default::default(),
                            phase: olp::inference::transport::TransportPhase::FirstByte,
                            class: olp::inference::transport::AttemptFailureClass::Ambiguous,
                            response_committed: true,
                            message: "injected cleanup ambiguity".to_owned(),
                        });
                    }
                    CanonicalResult::VideoDelete(VideoDeleteResult {
                        id: operation.job_id.clone(),
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
#[ignore = "requires OLP_TEST_DATABASE_ADMIN_URL and OLP_TEST_DATABASE_URL_PREFIX"]
async fn media_job_management_views_are_session_authorized_and_metadata_only() {
    let db = olp::test_support::TestDb::create_migrated("media_jobs_http").await;
    let pool = db.pool(5).await;
    let mut state = ProcessComposition::new(
        ApiMode::Control,
        pool.clone(),
        Arc::new(Manager::empty()),
        ORIGIN,
        PathBuf::from("missing-console-for-media-job-test"),
    );
    configure_bootstrap(&mut state, [18; 32]);
    let app = management_router_for_test(state.mode_dependencies().management().unwrap());

    let mut setup_request = Request::post("/api/v3/setup")
        .header(header::CONTENT_TYPE, "application/json")
        .header(header::ORIGIN, ORIGIN)
        .header("x-olp-setup-token", BOOTSTRAP_TOKEN)
        .body(Body::from(
            serde_json::to_vec(&json!({
                "email": "owner@example.test",
                "password": "correct horse battery staple",
                "display_name": "Owner",
                "installation_name": "Media job HTTP test"
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
    let provider_etag = Uuid::now_v7();
    let api_key_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO providers
         (id, name, kind, state, auth_mode, etag, created_by, endpoint)
         VALUES ($1, 'media-provider', 'openai', 'active', 'none', $2, $3, 'https://media.example.test/v1/')",
    )
    .bind(provider_id)
    .bind(provider_etag)
    .bind(owner_id)
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query("INSERT INTO provider_credential_slots(id,provider_id,name,is_default) VALUES($1,$1,'Default',true)")
        .bind(provider_id).execute(&pool).await.unwrap();
    sqlx::query(
        "INSERT INTO api_keys
         (id, lookup_id, secret_digest, name, created_by)
         VALUES ($1, 'olpv3media02', $2, 'media test', $3)",
    )
    .bind(api_key_id)
    .bind([8_u8; 32].as_slice())
    .bind(owner_id)
    .execute(&pool)
    .await
    .unwrap();
    let auth_hmac_key = Arc::new(AuthHmacKey::new([19; 32]));
    let material = auth_hmac_key.generate_api_key();
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
    let runtime = Arc::new(Manager::empty());
    let mut gateway_state = ProcessComposition::new(
        ApiMode::Gateway,
        pool.clone(),
        runtime.clone(),
        ORIGIN,
        PathBuf::from("missing-console-for-video-inference-test"),
    );
    gateway_state.auth_hmac_key = auth_hmac_key;
    let delete_calls = Arc::new(AtomicUsize::new(0));
    let create_calls = Arc::new(AtomicUsize::new(0));
    let fail_cleanup = Arc::new(AtomicBool::new(false));
    let transport: Arc<dyn ProviderTransport> = Arc::new(VideoLifecycleTransport {
        spool: gateway_state.media_spool.clone(),
        create_calls: create_calls.clone(),
        delete_calls: delete_calls.clone(),
        fail_cleanup: fail_cleanup.clone(),
    });
    let provider_revision_id = Uuid::now_v7();
    sqlx::query(
        "INSERT INTO provider_revisions \
         (id, provider_id, revision, name, kind, auth_mode, connector_ready, \
          source_etag, activated_by, endpoint) \
         VALUES ($1, $2, 1, 'media-provider', 'openai', 'none', true, $3, $4, 'https://media.example.test/v1/')",
    )
    .bind(provider_revision_id)
    .bind(provider_id)
    .bind(provider_etag)
    .bind(owner_id)
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query("UPDATE providers SET active_revision_id = $1 WHERE id = $2")
        .bind(provider_revision_id)
        .bind(provider_id)
        .execute(&pool)
        .await
        .unwrap();
    let mut transaction = pool.begin().await.unwrap();
    olp::providers::pool_store::snapshot(&mut transaction, provider_id, provider_revision_id)
        .await
        .unwrap();
    transaction.commit().await.unwrap();
    sqlx::query(
        "WITH model AS (
             INSERT INTO provider_models
             (id, provider_id, upstream_model, display_name, enabled, discovered_at)
             VALUES (uuidv7(), $1, 'upstream-video-model', 'Video model', true, now())
             RETURNING id
         ), revision_model AS (
             INSERT INTO provider_revision_models
             (id, provider_revision_id, source_provider_model_id, upstream_model,
              display_name, enabled, discovered_at)
             SELECT uuidv7(), $2, id, 'upstream-video-model', 'Video model', true, now()
             FROM model RETURNING id
         )
         INSERT INTO provider_revision_capabilities
         (provider_revision_model_id, operation, surface, mode, source, certified_at)
         SELECT id, operation, 'openai',
                CASE WHEN operation = 'video_create' THEN 'async' ELSE 'unary' END,
                'certified', now()
         FROM revision_model CROSS JOIN
              unnest(ARRAY['video_create', 'video_list', 'video_get',
                           'video_content', 'video_delete']) AS operation",
    )
    .bind(provider_id)
    .bind(provider_revision_id)
    .execute(&pool)
    .await
    .unwrap();
    let generation_id = RuntimeGenerationId::new();
    let generation_sequence: i64 = sqlx::query_scalar(
        "INSERT INTO runtime_generations \
         (id, compiled_release, release_sha256, created_by) \
         VALUES ($1, $2, $3, $4) RETURNING sequence",
    )
    .bind(generation_id.as_uuid())
    .bind(Vec::<u8>::new())
    .bind([0_u8; 32].as_slice())
    .bind(owner_id)
    .fetch_one(&pool)
    .await
    .unwrap();
    let snapshot = Snapshot {
        routing: Default::default(),
        generation: RuntimeGeneration {
            id: generation_id,
            ordinal: u64::try_from(generation_sequence).unwrap(),
            activated_at: chrono::Utc::now(),
        },
        providers: BTreeMap::from([(
            core_provider_id,
            Provider {
                id: core_provider_id,
                revision_id: provider_revision_id,
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
                routing_id: RouteId::new(),
                slug: route_slug.clone(),
                operations,
                overall_timeout: DurationMs::new(5_000),
                max_attempts: NonZeroU16::new(1).unwrap(),
                targets: vec![Target {
                    id: TargetId::new(),
                    routing_id: TargetId::new(),
                    provider_id: core_provider_id,
                    upstream_model: "upstream-video-model".into(),
                    priority: 0,
                    weight: NonZeroU32::new(1).unwrap(),
                    timeout: DurationMs::new(4_000),
                }],
            },
        )]),
        api_keys: BTreeMap::from([(
            lookup_id.clone(),
            ApiKey {
                routing_policy: Default::default(),
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
    };
    let payload = snapshot.to_persisted_vec().unwrap();
    let release_sha256: [u8; 32] = Sha256::digest(&payload).into();
    sqlx::query(
        "UPDATE runtime_generations SET compiled_release = $1, release_sha256 = $2 \
         WHERE id = $3",
    )
    .bind(payload)
    .bind(release_sha256.as_slice())
    .bind(generation_id.as_uuid())
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO runtime_generation_provider_configs \
         (runtime_generation_id, provider_id, kind, auth_mode, provider_revision_id, endpoint) \
         VALUES ($1, $2, 'openai', 'none', $3, 'https://media.example.test/v1/')",
    )
    .bind(generation_id.as_uuid())
    .bind(provider_id)
    .bind(provider_revision_id)
    .execute(&pool)
    .await
    .unwrap();
    let job_id = Uuid::now_v7();
    olp::media::jobs::lifecycle::reserve_media_job(
        &pool,
        NewMediaJobReservation {
            credential_version_id: None,
            id: job_id,
            runtime_generation_id: generation_id.as_uuid(),
            api_key_id,
            provider_id,
            upstream_model: "upstream-video-model".to_owned(),
            route_slug: "video-default".to_owned(),
            operation: "video_create".parse().unwrap(),
            surface: "openai".parse().unwrap(),
        },
    )
    .await
    .unwrap();
    let job = olp::media::jobs::lifecycle::attach_media_job_upstream(
        &pool,
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
            Request::get("/api/v3/media-jobs?state=queued&limit=50")
                .header(header::COOKIE, &cookie)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(list.status(), StatusCode::OK);
    let list_body: Value =
        serde_json::from_slice(&list.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(list_body["items"][0]["id"], job.id.to_string());
    assert_eq!(list_body["items"][0]["surface"], "openai");
    assert_eq!(list_body["items"][0]["state"], "queued");
    assert_eq!(list_body["items"][0]["lifecycle"], "active");
    assert!(list_body["items"][0].get("prompt").is_none());
    assert!(list_body["items"][0].get("content").is_none());

    let detail = app
        .clone()
        .oneshot(
            Request::get(format!("/api/v3/media-jobs/{}", job.id))
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

    gateway_state
        .transports
        .register(core_provider_id, Arc::clone(&transport));
    runtime
        .install(snapshot, BTreeMap::from([(core_provider_id, transport)]))
        .unwrap();
    let reconciliation_state = gateway_state.clone();
    let gateway = gateway_router_for_test(gateway_state.mode_dependencies().gateway().unwrap());
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
            Request::post("/v1/videos")
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
    let create_status = create.status();
    let created: Value =
        serde_json::from_slice(&create.into_body().collect().await.unwrap().to_bytes()).unwrap();
    assert_eq!(create_status, StatusCode::CREATED, "{created}");
    let video_id = created["id"].as_str().unwrap().to_owned();
    assert_eq!(created["model"], "video-default");
    assert_ne!(video_id, "upstream-video-created");

    let videos = gateway
        .clone()
        .oneshot(
            Request::get("/v1/videos?limit=20&order=desc")
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
            Request::get(format!("/v1/videos/{video_id}"))
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

    // Mounted media dispatch uses current quotas while retaining the job's
    // historical transport. Each scope must fail closed without a limiter.
    for connection_quota in [true, false] {
        let options = if connection_quota {
            json!({"limits":{"requests_per_minute":1}})
        } else {
            serde_json::to_value(olp::providers::options::ConnectionOptions::default()).unwrap()
        };
        sqlx::query("UPDATE provider_revisions SET options=$2 WHERE id=$1")
            .bind(provider_revision_id)
            .bind(options)
            .execute(&pool)
            .await
            .unwrap();
        sqlx::query("UPDATE provider_revision_credentials SET configuration=jsonb_set(configuration,'{requests_per_minute}',$2) WHERE provider_revision_id=$1")
            .bind(provider_revision_id)
            .bind(if connection_quota { Value::Null } else { json!(1) })
            .execute(&pool).await.unwrap();
        let limited = gateway
            .clone()
            .oneshot(
                Request::get(format!("/v1/videos/{video_id}/content"))
                    .header(header::AUTHORIZATION, &authorization)
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        let status = limited.status();
        let body: Value =
            serde_json::from_slice(&limited.into_body().collect().await.unwrap().to_bytes())
                .unwrap();
        assert_eq!(status, StatusCode::TOO_MANY_REQUESTS, "{body}");
        assert_eq!(body["error"]["message"], "provider_limits_unavailable");
    }
    sqlx::query("UPDATE provider_revision_credentials SET configuration=configuration-'requests_per_minute' WHERE provider_revision_id=$1")
        .bind(provider_revision_id).execute(&pool).await.unwrap();

    let content = gateway
        .clone()
        .oneshot(
            Request::get(format!("/v1/videos/{video_id}/content"))
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
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER fail_test_media_finalize
         BEFORE UPDATE ON async_media_jobs
         FOR EACH ROW EXECUTE FUNCTION fail_test_media_finalize()",
    )
    .execute(&pool)
    .await
    .unwrap();
    let ambiguous_delete = gateway
        .clone()
        .oneshot(
            Request::delete(format!("/v1/videos/{video_id}"))
                .header(header::AUTHORIZATION, &authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(ambiguous_delete.status(), StatusCode::SERVICE_UNAVAILABLE);
    assert_eq!(delete_calls.load(Ordering::Acquire), 1);
    assert_eq!(
        olp::media::jobs::queries::media_job(&(pool), Uuid::parse_str(&video_id).unwrap())
            .await
            .unwrap()
            .lifecycle,
        olp::media::jobs::MediaJobLifecycle::DeletePending
    );
    sqlx::query("DROP TRIGGER fail_test_media_finalize ON async_media_jobs")
        .execute(&pool)
        .await
        .unwrap();
    sqlx::query("DROP FUNCTION fail_test_media_finalize()")
        .execute(&pool)
        .await
        .unwrap();

    // The retry models an upstream 404 after the first delete succeeded. The
    // durable delete intent permits the transport to reconcile it as success.
    let deleted = gateway
        .clone()
        .oneshot(
            Request::delete(format!("/v1/videos/{video_id}"))
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
            Request::delete(format!("/v1/videos/{video_id}"))
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
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query(
        "CREATE TRIGGER fail_test_media_attach
         BEFORE UPDATE ON async_media_jobs
         FOR EACH ROW EXECUTE FUNCTION fail_test_media_attach()",
    )
    .execute(&pool)
    .await
    .unwrap();
    let compensated_create = gateway
        .clone()
        .oneshot(
            Request::post("/v1/videos")
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
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(compensated_lifecycle, "deleted");

    fail_cleanup.store(true, Ordering::Release);
    let unresolved_create = gateway
        .clone()
        .oneshot(
            Request::post("/v1/videos")
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
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(unresolved_lifecycle, "create_cleanup_pending");
    let unresolved_authority: (Option<Uuid>, Option<Uuid>) = sqlx::query_as(
        "SELECT runtime_generation_id, provider_revision_id FROM async_media_jobs \
         WHERE upstream_job_id = 'upstream-video-created-3'",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(
        unresolved_authority,
        (Some(generation_id.as_uuid()), Some(provider_revision_id))
    );
    sqlx::query("DROP TRIGGER fail_test_media_attach ON async_media_jobs")
        .execute(&pool)
        .await
        .unwrap();
    sqlx::query("DROP FUNCTION fail_test_media_attach()")
        .execute(&pool)
        .await
        .unwrap();

    // Revoke the creating key before the bounded autonomous pass. Lifecycle
    // authority is the durable job target, not a still-valid client secret.
    sqlx::query("UPDATE api_keys SET revoked_at = now() WHERE id = $1")
        .bind(api_key_id)
        .execute(&pool)
        .await
        .unwrap();
    let current = runtime.pin();
    runtime
        .install(
            Snapshot {
                routing: Default::default(),
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: 2,
                    activated_at: chrono::Utc::now(),
                },
                providers: current.providers.clone(),
                routes: BTreeMap::new(),
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
    let reconciliation_state = reconciliation_state.mode_dependencies().gateway().unwrap();
    let pass = reconcile_media_jobs_once(&reconciliation_state.media_jobs, 8)
        .await
        .unwrap();
    assert!(pass.claimed >= 1);
    assert!(pass.completed >= 1);
    let reconciled_lifecycle: String = sqlx::query_scalar(
        "SELECT lifecycle_state FROM async_media_jobs
         WHERE upstream_job_id = 'upstream-video-created-3'",
    )
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(reconciled_lifecycle, "deleted");

    let missing = gateway
        .oneshot(
            Request::get(format!("/v1/videos/{video_id}"))
                .header(header::AUTHORIZATION, authorization)
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(missing.status(), StatusCode::UNAUTHORIZED);
}
