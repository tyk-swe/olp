use super::*;

#[derive(Clone)]
struct MockOpenAiState {
    model_requests: Arc<AtomicUsize>,
    authorizations: Arc<Mutex<Vec<String>>>,
}

pub(super) struct MockOpenAiProvider {
    base_url: String,
    state: MockOpenAiState,
}

impl MockOpenAiProvider {
    pub(super) async fn spawn() -> Self {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        let state = MockOpenAiState {
            model_requests: Arc::new(AtomicUsize::new(0)),
            authorizations: Arc::new(Mutex::new(Vec::new())),
        };
        let app = Router::new()
            .route("/v1/models", get(mock_openai_models))
            .route("/v1/embeddings", post(mock_openai_embeddings))
            .with_state(state.clone());
        tokio::spawn(async move {
            axum::serve(listener, app).await.unwrap();
        });
        Self {
            base_url: format!("http://{address}/v1/"),
            state,
        }
    }

    pub(super) fn connector(&self, api_key: &str) -> Connector {
        Connector::new(
            ConnectorConfig::for_local_test(
                &self.base_url,
                Timeouts {
                    connect: Duration::from_secs(1),
                    first_byte: Duration::from_secs(1),
                    idle: Duration::from_secs(1),
                },
            ),
            ApiKey::new(api_key).unwrap(),
        )
    }

    pub(super) fn model_requests(&self) -> usize {
        self.state.model_requests.load(Ordering::SeqCst)
    }

    pub(super) fn last_authorization(&self) -> Option<String> {
        self.state.authorizations.lock().unwrap().last().cloned()
    }
}

async fn mock_openai_models(
    State(state): State<MockOpenAiState>,
    headers: HeaderMap,
) -> Json<Value> {
    state.model_requests.fetch_add(1, Ordering::SeqCst);
    state.authorizations.lock().unwrap().push(
        headers
            .get(header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned(),
    );
    Json(json!({
        "object": "list",
        "data": [{"id": "compatible-model", "object": "model"}]
    }))
}

async fn mock_openai_embeddings(
    State(state): State<MockOpenAiState>,
    headers: HeaderMap,
    Json(body): Json<Value>,
) -> Json<Value> {
    state.authorizations.lock().unwrap().push(
        headers
            .get(header::AUTHORIZATION)
            .and_then(|value| value.to_str().ok())
            .unwrap_or_default()
            .to_owned(),
    );
    Json(json!({
        "object": "list",
        "model": body["model"],
        "data": [{"object": "embedding", "index": 0, "embedding": [0.25]}],
        "usage": {"prompt_tokens": 1, "total_tokens": 1}
    }))
}

#[allow(clippy::too_many_arguments)]
pub(super) async fn send(
    app: &Router,
    method: Method,
    uri: &str,
    body: Option<Value>,
    cookie: Option<&str>,
    csrf: Option<&str>,
    idempotency_key: Option<&str>,
    if_match: Option<&str>,
) -> Response<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if body.is_some() {
        builder = builder.header(header::CONTENT_TYPE, "application/json");
    }
    if let Some(cookie) = cookie {
        builder = builder.header(header::COOKIE, cookie);
    }
    if let Some(csrf) = csrf {
        builder = builder
            .header("x-csrf-token", csrf)
            .header(header::ORIGIN, ORIGIN);
    } else if body.is_some() {
        builder = builder.header(header::ORIGIN, ORIGIN);
    }
    if let Some(idempotency_key) = idempotency_key {
        builder = builder.header("idempotency-key", idempotency_key);
    }
    if let Some(if_match) = if_match {
        builder = builder.header(header::IF_MATCH, if_match);
    }
    if uri == "/api/v1/setup" {
        builder = builder.header("x-olp-setup-token", BOOTSTRAP_TOKEN);
    }
    let body = body.map_or_else(Body::empty, |value| Body::from(value.to_string()));
    let mut request = builder.body(body).unwrap();
    request.extensions_mut().insert(axum::extract::ConnectInfo(
        "198.51.100.10:443".parse::<std::net::SocketAddr>().unwrap(),
    ));
    app.clone().oneshot(request).await.unwrap()
}

pub(super) fn cookie_header(response: &Response<Body>) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .filter_map(|cookie| cookie.split(';').next())
        .collect::<Vec<_>>()
        .join("; ")
}

pub(super) fn etag(response: &Response<Body>) -> String {
    response
        .headers()
        .get(header::ETAG)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned()
}

pub(super) async fn response_json(response: Response<Body>) -> Value {
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    serde_json::from_slice(&bytes).unwrap()
}
