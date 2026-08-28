use super::*;

async fn activate_runtime_inside_handler(
    State(state): State<GatewayState>,
    Extension(principal): Extension<HttpRequestAdmission>,
) -> String {
    let pinned_before_activation = principal.runtime();
    let pinned_generation = pinned_before_activation.generation.id;
    state
        .runtime()
        .install(
            Snapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: pinned_before_activation.generation.ordinal + 1,
                    activated_at: chrono::Utc::now(),
                },
                providers: pinned_before_activation.providers.clone(),
                routes: pinned_before_activation.routes.clone(),
                api_keys: pinned_before_activation.api_keys.clone(),
            },
            BTreeMap::new(),
        )
        .unwrap();
    assert_ne!(state.runtime().pin().generation.id, pinned_generation);
    let detached_principal = tokio::spawn({
        let admission = principal.clone();
        async move { admission.runtime().generation.id }
    })
    .await
    .unwrap();
    assert_eq!(detached_principal, pinned_generation);
    pinned_generation.to_string()
}

#[tokio::test]
async fn inference_http_boundary_pins_one_generation_across_activation() {
    let (state, key) = inference_state(false);
    let state = state.gateway_state_for_test();
    let original_generation = state.runtime().pin().generation.id;
    let app = Router::new()
        .route(
            "/openai/test-generation-pin",
            get(activate_runtime_inside_handler),
        )
        .layer(middleware::from_fn_with_state(
            state.request_boundary().clone(),
            enforce_request_limits,
        ))
        .with_state(state.clone());

    let response = app
        .oneshot(
            Request::get("/openai/test-generation-pin")
                .header(axum::http::header::AUTHORIZATION, format!("Bearer {key}"))
                .body(Body::empty())
                .unwrap(),
        )
        .await
        .unwrap();
    assert_eq!(response.status(), axum::http::StatusCode::OK);
    let body = response.into_body().collect().await.unwrap().to_bytes();
    assert_eq!(body.as_ref(), original_generation.to_string().as_bytes());
    assert_ne!(state.runtime().pin().generation.id, original_generation);
}

#[tokio::test]
async fn response_completion_and_drop_release_the_http_concurrency_reservation() {
    for consume in [true, false] {
        let released = Arc::new(AtomicBool::new(false));
        let release_signal = released.clone();
        let body = Body::new(ReleaseReservationBody {
            inner: Body::from("response"),
            reservation: Reservation::for_test(async move {
                release_signal.store(true, Ordering::Release);
            }),
        });
        if consume {
            body.collect().await.unwrap();
        } else {
            drop(body);
        }
        tokio::task::yield_now().await;
        assert!(
            released.load(Ordering::Acquire),
            "reservation was not released when consume={consume}"
        );
    }
}

#[tokio::test]
async fn rejection_finalization_awaits_one_release_and_emits_metadata_once() {
    let release_count = Arc::new(AtomicUsize::new(0));
    let released = Arc::clone(&release_count);
    let reservation = Reservation::for_test(async move {
        tokio::task::yield_now().await;
        released.fetch_add(1, Ordering::AcqRel);
    });
    let (request_metadata, mut receiver) = Emitter::bounded(2);
    let metadata = LocalRequestMetadata {
        request_metadata: Some(request_metadata),
        request_started_at: chrono::Utc::now(),
        runtime_generation_id: uuid::Uuid::now_v7(),
        api_key_id: uuid::Uuid::now_v7(),
        route_slug: "invalid-request".to_owned(),
        operation: OperationKind::Generation,
        surface: Surface::OpenAi,
        always_emit: true,
    };

    RequestFinalization::new(Some(reservation), Some(metadata), None, 128)
        .finish_rejection(axum::http::StatusCode::BAD_REQUEST)
        .await;

    assert_eq!(release_count.load(Ordering::Acquire), 1);
    let event = receiver.recv_next().await.unwrap();
    assert_eq!(event.status_code, Some(400));
    assert_eq!(event.error_class.as_deref(), Some("client_error"));
    assert!(receiver.recv_next().await.is_none());
}

#[tokio::test]
async fn concurrent_final_reservation_drops_release_once() {
    let released = Arc::new(AtomicBool::new(false));
    let release_signal = Arc::clone(&released);
    let reservation = Reservation::for_test(async move {
        release_signal.store(true, Ordering::Release);
    });
    let left = reservation.clone();
    let right = reservation.clone();
    drop(reservation);
    let barrier = Arc::new(tokio::sync::Barrier::new(2));
    let left_task = tokio::spawn({
        let barrier = Arc::clone(&barrier);
        async move {
            barrier.wait().await;
            drop(left);
        }
    });
    let right_task = tokio::spawn(async move {
        barrier.wait().await;
        drop(right);
    });
    left_task.await.unwrap();
    right_task.await.unwrap();
    tokio::time::timeout(Duration::from_secs(1), async {
        while !released.load(Ordering::Acquire) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("the final reservation drop must schedule its release");
}

fn pinned_principal(state: &GatewayState) -> Principal {
    let pinned = state.runtime().pin();
    let (lookup_id, _) = pinned.api_keys.iter().next().unwrap();
    Principal::new(
        Arc::clone(&pinned),
        lookup_id.clone(),
        Surface::OpenAi,
        Some(olp_engine::domain::auth::GatewayCapability::Inference),
    )
}

#[tokio::test]
async fn detached_inference_task_holds_the_http_reservation_after_request_cancellation() {
    let (state, _) = inference_state(false);
    let state = state.gateway_state_for_test();
    let released = Arc::new(AtomicBool::new(false));
    let release_signal = Arc::clone(&released);
    let reservation = Reservation::for_test(async move {
        release_signal.store(true, Ordering::Release);
    });
    let admission =
        HttpRequestAdmission::for_test(pinned_principal(&state), Some(reservation), Some(2_000));
    let started = Arc::new(tokio::sync::Notify::new());
    let release_child = Arc::new(tokio::sync::Notify::new());
    let (completed_sender, completed) = tokio::sync::oneshot::channel();
    let started_wait = started.notified();
    let outer = tokio::spawn({
        let admission = admission.clone();
        let started = Arc::clone(&started);
        let release_child = Arc::clone(&release_child);
        async move {
            let _task = tokio::spawn(async move {
                started.notify_one();
                release_child.notified().await;
                drop(admission);
                let _ = completed_sender.send(());
            });
            futures::future::pending::<()>().await;
        }
    });
    drop(admission);
    started_wait.await;
    outer.abort();
    let _ = outer.await;
    assert!(
        !released.load(Ordering::Acquire),
        "the detached task must retain the reservation after outer cancellation"
    );

    release_child.notify_one();
    completed.await.unwrap();
    tokio::time::timeout(Duration::from_secs(1), async {
        while !released.load(Ordering::Acquire) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .expect("the final detached reservation owner must release the lease");
}

#[tokio::test]
async fn spawned_inference_task_inherits_the_http_execution_context() {
    let (state, _) = inference_state(false);
    let state = state.gateway_state_for_test();
    let principal = pinned_principal(&state);
    let pinned_generation = principal.runtime().generation.id;
    state
        .runtime()
        .install(
            Snapshot {
                generation: RuntimeGeneration {
                    id: RuntimeGenerationId::new(),
                    ordinal: principal.runtime().generation.ordinal + 1,
                    activated_at: chrono::Utc::now(),
                },
                providers: principal.runtime().providers.clone(),
                routes: principal.runtime().routes.clone(),
                api_keys: principal.runtime().api_keys.clone(),
            },
            BTreeMap::new(),
        )
        .unwrap();
    let admission = HttpRequestAdmission::for_test(
        principal,
        Some(Reservation::for_test(async {})),
        Some(2_000),
    );
    let task = tokio::spawn({
        let admission = admission.clone();
        async move {
            admission.claim_metadata();
            (
                admission.runtime().generation.id,
                admission.surface(),
                admission.gateway_capability(),
                admission.reserved_tokens(),
                admission.holds_reservation(),
            )
        }
    });
    let (principal_generation, principal_surface, principal_capability, reserved_tokens, held) =
        task.await.unwrap();
    assert_eq!(principal_generation, pinned_generation);
    assert_eq!(principal_surface, Surface::OpenAi);
    assert_eq!(
        principal_capability,
        Some(olp_engine::domain::auth::GatewayCapability::Inference)
    );
    assert_ne!(state.runtime().pin().generation.id, pinned_generation);
    assert_eq!(reserved_tokens, Some(2_000));
    assert!(held);
    assert!(
        admission.metadata_claimed(),
        "the detached task's claim is visible to the HTTP boundary"
    );
}
