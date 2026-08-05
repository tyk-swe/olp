use super::*;

pub(super) struct BrowserFlow {
    pub(super) authorization_url: String,
    pub(super) state: String,
    pub(super) cookie_name: String,
    pub(super) flow_cookie: String,
}

pub(super) async fn begin_login(app: &Router) -> BrowserFlow {
    begin_login_with_request(app, "/api/v1/oidc/login", None).await
}

pub(super) async fn begin_login_with_request(
    app: &Router,
    uri: &str,
    cookies: Option<&str>,
) -> BrowserFlow {
    let response = send_empty(app, Method::GET, uri, cookies, None).await;
    assert_eq!(response.status(), StatusCode::SEE_OTHER);
    let authorization_url = response
        .headers()
        .get(header::LOCATION)
        .unwrap()
        .to_str()
        .unwrap()
        .to_owned();
    let state = query_value(&authorization_url, "state");
    let cookie_name = scoped_cookie_name("__Host-olp_oidc_login_", &state);
    assert_host_cookie_contract(&response, &cookie_name);
    let flow_cookie = named_cookie(&response, &cookie_name);
    assert_eq!(
        query_value(&authorization_url, "code_challenge_method"),
        "S256"
    );
    BrowserFlow {
        authorization_url,
        state,
        cookie_name,
        flow_cookie,
    }
}

pub(super) async fn begin_oidc_reauthentication(
    app: &Router,
    cookies: &str,
    csrf: &str,
    purpose: &str,
    resource_id: Option<&str>,
) -> BrowserFlow {
    let body = match resource_id {
        Some(resource_id) => json!({"purpose": purpose, "resource_id": resource_id}),
        None => json!({"purpose": purpose}),
    };
    let response = send_json(
        app,
        Method::POST,
        "/api/v1/oidc/reauthenticate",
        body,
        Some(cookies),
        Some(csrf),
        None,
    )
    .await;
    assert_eq!(response.status(), StatusCode::OK);
    let (cookie_name, flow_cookie) = first_scoped_cookie(&response, "__Host-olp_oidc_link_");
    assert_host_cookie_contract(&response, &cookie_name);
    let authorization_url = response_json(response).await["authorization_url"]
        .as_str()
        .unwrap()
        .to_owned();
    let state = query_value(&authorization_url, "state");
    // Reauthentication deliberately shares the authenticated-flow namespace
    // with link flows while retaining an independent flow ID.
    assert_eq!(
        cookie_name,
        scoped_cookie_name("__Host-olp_oidc_link_", &state)
    );
    BrowserFlow {
        authorization_url,
        state,
        cookie_name,
        flow_cookie,
    }
}

pub(super) async fn arm_idp(idp: &MockIdp, authorization_url: &str, wrong_nonce: bool) {
    let mut inner = idp.inner.lock().await;
    inner.expected = Some(ExpectedAuthorization {
        nonce: query_value(authorization_url, "nonce"),
        challenge: query_value(authorization_url, "code_challenge"),
        wrong_nonce,
    });
    inner.pkce_verified = false;
}

pub(super) async fn spawn_mock_idp() -> (MockIdp, tokio::task::JoinHandle<()>) {
    let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
    let issuer = format!("http://{}", listener.local_addr().unwrap());
    let private_der = STANDARD.decode(ED25519_PRIVATE_DER_B64).unwrap();
    let idp = MockIdp {
        issuer,
        encoding_key: Arc::new(EncodingKey::from_ed_der(&private_der)),
        public_x: ED25519_PUBLIC_X.to_owned(),
        inner: Arc::new(Mutex::new(MockInner {
            identity: MockIdentity {
                subject: "jit-developer-subject".to_owned(),
                email: "developer@example.test".to_owned(),
                name: "OIDC Developer".to_owned(),
                groups: vec!["engineering".to_owned()],
            },
            expected: None,
            pkce_verified: false,
        })),
    };
    let app = Router::new()
        .route("/.well-known/openid-configuration", get(mock_discovery))
        .route("/jwks", get(mock_jwks))
        .route("/authorize", get(|| async { StatusCode::NO_CONTENT }))
        .route("/token", post(mock_token))
        .with_state(idp.clone());
    let task = tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
    (idp, task)
}

pub(super) async fn mock_discovery(State(idp): State<MockIdp>) -> Json<Value> {
    Json(json!({
        "issuer": idp.issuer,
        "authorization_endpoint": format!("{}/authorize", idp.issuer),
        "token_endpoint": format!("{}/token", idp.issuer),
        "jwks_uri": format!("{}/jwks", idp.issuer),
        "response_types_supported": ["code"],
        "code_challenge_methods_supported": ["S256"],
        "token_endpoint_auth_methods_supported": ["client_secret_basic"],
        "id_token_signing_alg_values_supported": ["EdDSA"]
    }))
}

pub(super) async fn mock_jwks(State(idp): State<MockIdp>) -> Json<Value> {
    Json(json!({"keys": [{
        "kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": "mock-key",
        "x": idp.public_x
    }]}))
}

pub(super) async fn mock_token(
    State(idp): State<MockIdp>,
    headers: HeaderMap,
    Form(form): Form<BTreeMap<String, String>>,
) -> Result<Json<Value>, StatusCode> {
    let expected_basic = format!(
        "Basic {}",
        STANDARD.encode(format!("{CLIENT_ID}:{CLIENT_SECRET}"))
    );
    if headers
        .get(header::AUTHORIZATION)
        .and_then(|value| value.to_str().ok())
        != Some(expected_basic.as_str())
        || form.get("grant_type").map(String::as_str) != Some("authorization_code")
        || form.get("code").map(String::as_str) != Some("mock-code")
        || form.get("client_id").map(String::as_str) != Some(CLIENT_ID)
    {
        return Err(StatusCode::UNAUTHORIZED);
    }
    let verifier = form.get("code_verifier").ok_or(StatusCode::BAD_REQUEST)?;
    let challenge = URL_SAFE_NO_PAD.encode(Sha256::digest(verifier.as_bytes()));
    let mut inner = idp.inner.lock().await;
    let expected = inner.expected.take().ok_or(StatusCode::BAD_REQUEST)?;
    if challenge != expected.challenge {
        return Err(StatusCode::BAD_REQUEST);
    }
    inner.pkce_verified = true;
    let nonce = if expected.wrong_nonce {
        "wrong-nonce".to_owned()
    } else {
        expected.nonce
    };
    let identity = inner.identity.clone();
    drop(inner);
    let mut header = Header::new(Algorithm::EdDSA);
    header.kid = Some("mock-key".to_owned());
    let now = Utc::now().timestamp();
    let id_token = encode(
        &header,
        &json!({
            "iss": idp.issuer,
            "sub": identity.subject,
            "aud": CLIENT_ID,
            "iat": now,
            "exp": now + 300,
            "auth_time": now,
            "nonce": nonce,
            "email": identity.email,
            "email_verified": true,
            "name": identity.name,
            "groups": identity.groups
        }),
        &idp.encoding_key,
    )
    .map_err(|_| StatusCode::INTERNAL_SERVER_ERROR)?;
    Ok(Json(
        json!({"id_token": id_token, "access_token": "never-persist-this", "token_type": "Bearer"}),
    ))
}

pub(super) async fn callback_request(
    app: &Router,
    state: &str,
    flow_cookie: &str,
    session_cookies: Option<&str>,
) -> Response<Body> {
    let cookie_name = if flow_cookie.starts_with("v2.") {
        scoped_cookie_name("__Host-olp_oidc_login_", state)
    } else {
        scoped_cookie_name("__Host-olp_oidc_link_", state)
    };
    let cookies = session_cookies.map_or_else(
        || format!("{cookie_name}={flow_cookie}"),
        |session| format!("{session}; {cookie_name}={flow_cookie}"),
    );
    let uri = format!("/api/v1/oidc/callback?code=mock-code&state={state}");
    let request = Request::builder()
        .method(Method::GET)
        .uri(uri)
        .header(header::COOKIE, cookies)
        .body(Body::empty())
        .unwrap();
    app.clone().oneshot(request).await.unwrap()
}

pub(super) async fn send_json(
    app: &Router,
    method: Method,
    uri: &str,
    body: Value,
    cookies: Option<&str>,
    csrf: Option<&str>,
    if_match: Option<&str>,
) -> Response<Body> {
    request(app, method, uri, Some(body), cookies, csrf, if_match, true).await
}

pub(super) async fn send_empty(
    app: &Router,
    method: Method,
    uri: &str,
    cookies: Option<&str>,
    csrf: Option<&str>,
) -> Response<Body> {
    request(app, method, uri, None, cookies, csrf, None, false).await
}

pub(super) async fn send_empty_with_origin(
    app: &Router,
    method: Method,
    uri: &str,
    cookies: Option<&str>,
    csrf: Option<&str>,
) -> Response<Body> {
    request(app, method, uri, None, cookies, csrf, None, true).await
}

#[allow(clippy::too_many_arguments)]
pub(super) async fn request(
    app: &Router,
    method: Method,
    uri: &str,
    body: Option<Value>,
    cookies: Option<&str>,
    csrf: Option<&str>,
    if_match: Option<&str>,
    origin: bool,
) -> Response<Body> {
    let mut builder = Request::builder().method(method).uri(uri);
    if origin {
        builder = builder.header(header::ORIGIN, ORIGIN);
    }
    if let Some(cookies) = cookies {
        builder = builder.header(header::COOKIE, cookies);
    }
    if let Some(csrf) = csrf {
        builder = builder.header("x-csrf-token", csrf);
    }
    if let Some(if_match) = if_match {
        builder = builder.header(header::IF_MATCH, if_match);
    }
    if uri == "/api/v1/setup" {
        builder = builder.header("x-olp-setup-token", BOOTSTRAP_TOKEN);
    }
    let body = if let Some(value) = body {
        builder = builder.header(header::CONTENT_TYPE, "application/json");
        Body::from(value.to_string())
    } else {
        Body::empty()
    };
    let mut request = builder.body(body).unwrap();
    request.extensions_mut().insert(axum::extract::ConnectInfo(
        "198.51.100.13:443".parse::<std::net::SocketAddr>().unwrap(),
    ));
    app.clone().oneshot(request).await.unwrap()
}

pub(super) fn cookie_header(response: &Response<Body>) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .map(|cookie| cookie.to_str().unwrap().split(';').next().unwrap())
        .filter(|cookie| {
            !cookie.starts_with("__Host-olp_oidc_flow=")
                && !cookie.starts_with("__Host-olp_oidc_login_flow=")
                && !cookie.starts_with("__Host-olp_oidc_login_")
                && !cookie.starts_with("__Host-olp_oidc_link_")
        })
        .collect::<Vec<_>>()
        .join("; ")
}

pub(super) fn apply_response_cookies(existing: &str, response: &Response<Body>) -> String {
    let mut cookies = BTreeMap::new();
    for cookie in existing.split(';') {
        let Some((name, value)) = cookie.trim().split_once('=') else {
            continue;
        };
        if value.is_empty() {
            cookies.remove(name);
        } else {
            cookies.insert(name.to_owned(), value.to_owned());
        }
    }
    for cookie in response.headers().get_all(header::SET_COOKIE).iter() {
        let Some((name, value)) = cookie
            .to_str()
            .ok()
            .and_then(|value| value.split(';').next())
            .and_then(|value| value.split_once('='))
        else {
            continue;
        };
        if value.is_empty() {
            cookies.remove(name);
        } else {
            cookies.insert(name.to_owned(), value.to_owned());
        }
    }
    cookies
        .into_iter()
        .map(|(name, value)| format!("{name}={value}"))
        .collect::<Vec<_>>()
        .join("; ")
}

pub(super) fn scoped_cookie_name(prefix: &str, state: &str) -> String {
    let flow_id = state
        .split_once('.')
        .map(|(flow_id, _)| flow_id)
        .expect("scoped OIDC state must contain a flow ID");
    format!("{prefix}{flow_id}")
}

pub(super) fn first_scoped_cookie(response: &Response<Body>, prefix: &str) -> (String, String) {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .find_map(|cookie| {
            let first = cookie.to_str().ok()?.split(';').next()?;
            let (name, value) = first.split_once('=')?;
            name.starts_with(prefix)
                .then(|| (name.to_owned(), value.to_owned()))
        })
        .unwrap_or_else(|| panic!("missing cookie with prefix {prefix}"))
}

pub(super) fn named_cookie(response: &Response<Body>, name: &str) -> String {
    response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .find_map(|cookie| {
            let first = cookie.to_str().ok()?.split(';').next()?;
            first.strip_prefix(&format!("{name}="))
        })
        .unwrap()
        .to_owned()
}

pub(super) fn assert_host_cookie_contract(response: &Response<Body>, name: &str) {
    let cookie = response
        .headers()
        .get_all(header::SET_COOKIE)
        .iter()
        .filter_map(|value| value.to_str().ok())
        .find(|value| value.starts_with(&format!("{name}=")))
        .unwrap_or_else(|| panic!("missing {name} cookie"));
    assert!(
        cookie.contains("; Path=/;"),
        "invalid __Host Path: {cookie}"
    );
    assert!(cookie.contains("; Secure;"), "missing Secure: {cookie}");
    assert!(
        !cookie.contains("Domain="),
        "__Host cookie has Domain: {cookie}"
    );
}

pub(super) fn query_value(url: &str, name: &str) -> String {
    Url::parse(url)
        .unwrap()
        .query_pairs()
        .find_map(|(key, value)| (key == name).then(|| value.into_owned()))
        .unwrap()
}

pub(super) async fn response_json(response: Response<Body>) -> Value {
    serde_json::from_str(&response_text(response).await).unwrap()
}

pub(super) async fn response_text(response: Response<Body>) -> String {
    let bytes = response.into_body().collect().await.unwrap().to_bytes();
    String::from_utf8(bytes.to_vec()).unwrap()
}
