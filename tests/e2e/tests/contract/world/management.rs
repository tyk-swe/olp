use std::sync::atomic::{AtomicU32, Ordering};

use std::time::Duration;

use reqwest::header::HeaderMap;
use serde_json::{Value, json};

use super::{OWNER_EMAIL, OWNER_PASSWORD};

pub(crate) struct MgmtResponse {
    pub(crate) status: u16,
    pub(crate) headers: HeaderMap,
    pub(crate) body: Value,
}

impl MgmtResponse {
    pub(crate) fn header(&self, name: &str) -> Option<String> {
        self.headers
            .get(name)
            .and_then(|value| value.to_str().ok())
            .map(str::to_owned)
    }

    pub(crate) fn etag(&self) -> Option<String> {
        self.header("etag")
    }

    pub(crate) fn require_etag(&self, what: &str) -> Result<String, String> {
        self.etag()
            .ok_or_else(|| format!("{what} response carries no ETag header"))
    }
}

pub(crate) struct Management {
    http: reqwest::Client,
    origin: String,
    cookie: String,
    csrf: String,
    sequence: AtomicU32,
}

impl Management {
    pub(crate) fn new(origin: &str) -> Self {
        Self {
            http: reqwest::Client::builder()
                .timeout(Duration::from_secs(30))
                .build()
                .expect("reqwest client builds"),
            origin: origin.to_owned(),
            cookie: String::new(),
            csrf: String::new(),
            sequence: AtomicU32::new(0),
        }
    }

    pub(crate) fn next_idempotency_key(&self) -> String {
        let next = self.sequence.fetch_add(1, Ordering::Relaxed) + 1;
        format!("e2e-contract-{next:04}")
    }

    /// Performs first-run setup and retains the resulting session.
    ///
    /// `POST /api/v1/setup` requires an `Origin` header matching
    /// `OLP_PUBLIC_ORIGIN`; the setup token is single-use.
    pub(crate) async fn setup(&mut self, setup_token: &str) -> Result<MgmtResponse, String> {
        let response = self
            .http
            .post(format!("{}/api/v1/setup", self.origin))
            .header("x-olp-setup-token", setup_token)
            .header(reqwest::header::ORIGIN, &self.origin)
            .json(&json!({
                "email": OWNER_EMAIL,
                "password": OWNER_PASSWORD,
                "display_name": "E2E Owner",
                "installation_name": "E2E contract"
            }))
            .send()
            .await
            .map_err(|error| format!("setup request failed: {error}"))?;
        let cookie = response
            .headers()
            .get_all(reqwest::header::SET_COOKIE)
            .iter()
            .filter_map(|value| value.to_str().ok())
            .filter_map(|value| value.split(';').next())
            .collect::<Vec<_>>()
            .join("; ");
        let parsed = read_response(response).await?;
        if parsed.status != 201 {
            return Err(format!(
                "setup returned {} instead of 201: {}",
                parsed.status, parsed.body
            ));
        }
        self.cookie = cookie;
        self.csrf = parsed.body["csrf_token"]
            .as_str()
            .ok_or_else(|| format!("setup response lacks csrf_token: {}", parsed.body))?
            .to_owned();
        Ok(parsed)
    }

    pub(crate) async fn send(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        idempotency_key: Option<&str>,
        if_match: Option<&str>,
    ) -> Result<MgmtResponse, String> {
        let mut request = self
            .http
            .request(method.clone(), format!("{}{path}", self.origin))
            .header(reqwest::header::COOKIE, &self.cookie);
        if method != reqwest::Method::GET {
            request = request
                .header("x-csrf-token", &self.csrf)
                .header(reqwest::header::ORIGIN, &self.origin);
        }
        // Mutating management operations require an Idempotency-Key. Callers
        // that do not pin one get a fresh key, so no call site has to restate a
        // header the API demands of every mutation.
        let generated_key;
        let key = match idempotency_key {
            Some(key) => key,
            None => {
                generated_key = self.next_idempotency_key();
                &generated_key
            }
        };
        request = request.header("idempotency-key", key);
        if let Some(etag) = if_match {
            request = request.header(reqwest::header::IF_MATCH, etag);
        }
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|error| format!("{method} {path} failed: {error}"))?;
        read_response(response).await
    }

    /// Sends a management request with exactly the headers given and no others.
    ///
    /// `send` supplies the session cookie, the CSRF token, `Origin` and an
    /// idempotency key because almost every call needs them. The documented
    /// failure modes are precisely the cases where one of those is absent or
    /// wrong, so they need a builder that adds nothing on the caller's behalf.
    pub(crate) async fn raw(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        headers: &[(&str, &str)],
    ) -> Result<MgmtResponse, String> {
        let mut request = self
            .http
            .request(method.clone(), format!("{}{path}", self.origin));
        for (name, value) in headers {
            request = request.header(*name, *value);
        }
        if let Some(body) = body {
            request = request.json(&body);
        }
        let response = request
            .send()
            .await
            .map_err(|error| format!("{method} {path} failed: {error}"))?;
        read_response(response).await
    }

    pub(crate) fn cookie(&self) -> &str {
        &self.cookie
    }

    pub(crate) fn csrf(&self) -> &str {
        &self.csrf
    }

    pub(crate) fn origin(&self) -> &str {
        &self.origin
    }

    pub(crate) async fn get(&self, path: &str) -> Result<MgmtResponse, String> {
        self.send(reqwest::Method::GET, path, None, None, None)
            .await
    }

    pub(crate) async fn post(&self, path: &str, body: Value) -> Result<MgmtResponse, String> {
        let key = self.next_idempotency_key();
        self.send(reqwest::Method::POST, path, Some(body), Some(&key), None)
            .await
    }

    pub(crate) async fn expect(
        &self,
        method: reqwest::Method,
        path: &str,
        body: Option<Value>,
        idempotency_key: Option<&str>,
        if_match: Option<&str>,
        expected_status: u16,
    ) -> Result<MgmtResponse, String> {
        let response = self
            .send(method.clone(), path, body, idempotency_key, if_match)
            .await?;
        if response.status != expected_status {
            return Err(format!(
                "{method} {path} returned {} instead of {expected_status}: {}",
                response.status, response.body
            ));
        }
        Ok(response)
    }
}

async fn read_response(response: reqwest::Response) -> Result<MgmtResponse, String> {
    let status = response.status().as_u16();
    let headers = response.headers().clone();
    let bytes = response
        .bytes()
        .await
        .map_err(|error| format!("failed to read response body: {error}"))?;
    let body = serde_json::from_slice(&bytes)
        .unwrap_or_else(|_| Value::String(String::from_utf8_lossy(&bytes).into_owned()));
    Ok(MgmtResponse {
        status,
        headers,
        body,
    })
}
