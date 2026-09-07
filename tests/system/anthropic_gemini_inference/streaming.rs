use super::*;

struct PostCommitFailureTransport {
    calls: Arc<AtomicUsize>,
}

impl ProviderTransport for PostCommitFailureTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        Box::pin(async {
            let events = vec![
                Ok(Event::new(
                    0,
                    Kind::ResponseStart {
                        response_id: Some("committed".into()),
                        provider_model: Some("primary".into()),
                    },
                )),
                Err(TransportError {
                    upstream: Default::default(),
                    phase: TransportPhase::Body,
                    class: AttemptFailureClass::UpstreamServer,
                    response_committed: true,
                    message: "failed after commit".into(),
                }),
            ];
            Ok(ProviderOutput::Events(Box::pin(stream::iter(events))))
        })
    }
}

struct NeverCalledTransport(Arc<AtomicUsize>);

impl ProviderTransport for NeverCalledTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        self.0.fetch_add(1, Ordering::SeqCst);
        Box::pin(async {
            Err(TransportError {
                upstream: Default::default(),
                phase: TransportPhase::Connect,
                class: AttemptFailureClass::Connect,
                response_committed: false,
                message: "secondary invoked".into(),
            })
        })
    }
}

#[tokio::test]
async fn streaming_never_fails_over_after_the_first_canonical_event() {
    let fixture = test_gateway();
    let snapshot = fixture.state.runtime().pin();
    let mut snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: snapshot.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers: snapshot.providers.clone(),
        routes: snapshot.routes.clone(),
        api_keys: snapshot.api_keys.clone(),
    };
    let provider_ids = snapshot.providers.keys().copied().collect::<Vec<_>>();
    for provider in snapshot.providers.values_mut() {
        provider.kind = ProviderKind::Anthropic;
        provider.capabilities = BTreeSet::from([Capability::new(
            if provider.id == provider_ids[0] {
                "claude-private"
            } else {
                "gemini-private"
            },
            OperationKind::Generation,
            Surface::Anthropic,
            TransportMode::Streaming,
        )]);
    }
    let route = snapshot.routes.values_mut().next().unwrap();
    route.operations = BTreeSet::from([OperationKind::Generation]);
    route.targets[0].priority = 0;
    route.targets[1].priority = 1;
    let primary_calls = Arc::new(AtomicUsize::new(0));
    let secondary_calls = Arc::new(AtomicUsize::new(0));
    let transports: BTreeMap<ProviderId, Arc<dyn ProviderTransport>> = BTreeMap::from([
        (
            route.targets[0].provider_id,
            Arc::new(PostCommitFailureTransport {
                calls: primary_calls.clone(),
            }) as Arc<dyn ProviderTransport>,
        ),
        (
            route.targets[1].provider_id,
            Arc::new(NeverCalledTransport(secondary_calls.clone())) as Arc<dyn ProviderTransport>,
        ),
    ]);
    fixture
        .state
        .runtime()
        .install(snapshot, transports)
        .unwrap();
    let response = gateway_router_for_test(fixture.state)
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({"model":"team-default","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}),
        ))
        .await
        .unwrap();
    let wire = String::from_utf8(
        response
            .into_body()
            .collect()
            .await
            .unwrap()
            .to_bytes()
            .to_vec(),
    )
    .unwrap();
    assert!(wire.contains("event: error"));
    assert_eq!(primary_calls.load(Ordering::SeqCst), 1);
    assert_eq!(secondary_calls.load(Ordering::SeqCst), 0);
}

struct DropAwareStream {
    first: Option<Event>,
    dropped: Arc<AtomicBool>,
}

impl Stream for DropAwareStream {
    type Item = Result<Event, TransportError>;

    fn poll_next(
        mut self: std::pin::Pin<&mut Self>,
        _context: &mut std::task::Context<'_>,
    ) -> std::task::Poll<Option<Self::Item>> {
        if let Some(first) = self.first.take() {
            std::task::Poll::Ready(Some(Ok(first)))
        } else {
            std::task::Poll::Pending
        }
    }
}

impl Drop for DropAwareStream {
    fn drop(&mut self) {
        self.dropped.store(true, Ordering::SeqCst);
    }
}

struct DropAwareTransport(Arc<AtomicBool>);

impl ProviderTransport for DropAwareTransport {
    fn execute<'a>(
        &'a self,
        _request: ProviderRequest,
    ) -> BoxFuture<'a, Result<ProviderOutput, TransportError>> {
        let dropped = self.0.clone();
        Box::pin(async move {
            Ok(ProviderOutput::Events(Box::pin(DropAwareStream {
                first: Some(Event::new(
                    0,
                    Kind::ResponseStart {
                        response_id: Some("cancel".into()),
                        provider_model: Some("private".into()),
                    },
                )),
                dropped,
            })
                as ProviderEventStream))
        })
    }
}

#[tokio::test]
async fn client_disconnect_drops_the_upstream_stream() {
    let fixture = test_gateway();
    let snapshot = fixture.state.runtime().pin();
    let mut snapshot = Snapshot {
        generation: RuntimeGeneration {
            id: RuntimeGenerationId::new(),
            ordinal: snapshot.generation.ordinal + 1,
            activated_at: Utc::now(),
        },
        providers: snapshot.providers.clone(),
        routes: snapshot.routes.clone(),
        api_keys: snapshot.api_keys.clone(),
    };
    let route = snapshot.routes.values_mut().next().unwrap();
    route.operations = BTreeSet::from([OperationKind::Generation]);
    route.max_attempts = NonZeroU16::new(1).unwrap();
    route.targets.truncate(1);
    let provider_id = route.targets[0].provider_id;
    snapshot.providers.retain(|id, _| *id == provider_id);
    snapshot
        .providers
        .get_mut(&provider_id)
        .unwrap()
        .capabilities = BTreeSet::from([Capability::new(
        route.targets[0].upstream_model.clone(),
        OperationKind::Generation,
        Surface::Anthropic,
        TransportMode::Streaming,
    )]);
    let dropped = Arc::new(AtomicBool::new(false));
    fixture
        .state
        .runtime()
        .install(
            snapshot,
            BTreeMap::from([(
                provider_id,
                Arc::new(DropAwareTransport(dropped.clone())) as Arc<dyn ProviderTransport>,
            )]),
        )
        .unwrap();
    let response = gateway_router_for_test(fixture.state)
        .oneshot(post_json(
            "/anthropic/v1/messages",
            ("x-api-key", &fixture.key),
            json!({"model":"team-default","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hello"}]}),
        ))
        .await
        .unwrap();
    drop(response);
    tokio::time::timeout(Duration::from_secs(1), async {
        while !dropped.load(Ordering::SeqCst) {
            tokio::task::yield_now().await;
        }
    })
    .await
    .unwrap();
}
