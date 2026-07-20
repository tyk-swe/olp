use http::{HeaderMap, HeaderValue, StatusCode, header};
use olp_domain::{AttemptFailureClass, ProviderRequest, TransportError, TransportPhase};
use reqwest::{Method, Response, multipart};
use tokio::time::{Instant, timeout};

use crate::openai::headers::sanitize_forward_headers;

use super::super::{OpenAiConnector, errors::*, streams::*};

impl OpenAiConnector {
    pub(super) async fn post_raw_json(
        &self,
        request: &ProviderRequest,
        path: &str,
        body: Vec<u8>,
    ) -> Result<DeadlineResponse, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url(path)
            .map_err(map_endpoint_error)?;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        let wait = bounded_duration(
            self.config.timeouts.first_byte,
            remaining(attempt_deadline, TransportPhase::FirstByte)?,
        );
        let response = timeout(wait, client.post(url).headers(headers).body(body).send())
            .await
            .map_err(|_| first_byte_timeout())?
            .map_err(map_send_error)?;
        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }
        Ok(DeadlineResponse::new(
            response,
            self.config.timeouts.first_byte,
            attempt_deadline,
        ))
    }

    pub(super) async fn request_json(
        &self,
        request: &ProviderRequest,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
    ) -> Result<Vec<u8>, TransportError> {
        let response = self
            .request_raw(request, method, path, body, "application/json")
            .await?;
        require_content_type(&response, "application/json")?;
        read_deadline_body(
            response,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await
    }

    pub(super) async fn request_raw(
        &self,
        request: &ProviderRequest,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        accept: &'static str,
    ) -> Result<DeadlineResponse, TransportError> {
        let response = self
            .request_raw_unchecked(request, method, path, body, accept)
            .await?;
        if !response.status().is_success() {
            return Err(self
                .map_error_response(response.response, response.attempt_deadline)
                .await);
        }
        Ok(response)
    }

    pub(super) async fn request_raw_unchecked(
        &self,
        request: &ProviderRequest,
        method: Method,
        path: &str,
        body: Option<Vec<u8>>,
        accept: &'static str,
    ) -> Result<DeadlineResponse, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let (resource, query) = path
            .split_once('?')
            .map_or((path, None), |(path, query)| (path, Some(query)));
        let mut url = self
            .config
            .endpoint
            .resource_url(resource)
            .map_err(map_endpoint_error)?;
        if let Some(query) = query {
            let combined = url.query().map_or_else(
                || query.to_owned(),
                |existing| format!("{existing}&{query}"),
            );
            url.set_query(Some(&combined));
        }
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(header::ACCEPT, HeaderValue::from_static(accept));
        let mut builder = client.request(method, url).headers(headers);
        if let Some(body) = body {
            builder = builder
                .header(header::CONTENT_TYPE, "application/json")
                .body(body);
        }
        let wait = bounded_duration(
            self.config.timeouts.first_byte,
            remaining(attempt_deadline, TransportPhase::FirstByte)?,
        );
        let response = timeout(wait, builder.send())
            .await
            .map_err(|_| first_byte_timeout())?
            .map_err(map_send_error)?;
        Ok(DeadlineResponse::new(
            response,
            self.config.timeouts.first_byte,
            attempt_deadline,
        ))
    }

    pub(super) async fn post_multipart_raw(
        &self,
        request: &ProviderRequest,
        path: &str,
        form: multipart::Form,
    ) -> Result<DeadlineResponse, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url(path)
            .map_err(map_endpoint_error)?;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        let wait = bounded_duration(
            self.config.timeouts.first_byte,
            remaining(attempt_deadline, TransportPhase::FirstByte)?,
        );
        let response = timeout(
            wait,
            client.post(url).headers(headers).multipart(form).send(),
        )
        .await
        .map_err(|_| ambiguous_multipart_timeout())?
        .map_err(map_ambiguous_send_error)?;
        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }
        Ok(DeadlineResponse::new(
            response,
            self.config.timeouts.first_byte,
            attempt_deadline,
        ))
    }

    pub(super) async fn post_unary_json(
        &self,
        request: &ProviderRequest,
        path: &str,
        body: Vec<u8>,
    ) -> Result<Vec<u8>, TransportError> {
        let started = Instant::now();
        let attempt_deadline = started + request.attempt.timeout.as_duration();
        let connect_timeout = bounded_duration(
            self.config.timeouts.connect,
            remaining(attempt_deadline, TransportPhase::Connect)?,
        );
        let client = self
            .config
            .endpoint
            .pinned_client(connect_timeout)
            .await
            .map_err(map_endpoint_error)?;
        let url = self
            .config
            .endpoint
            .resource_url(path)
            .map_err(map_endpoint_error)?;
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let mut headers = sanitize_forward_headers(&HeaderMap::new());
        self.attach_auth(&mut headers)?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(
            "x-request-id",
            HeaderValue::from_str(&request.metadata.request_id.to_string()).map_err(|_| {
                transport_error(
                    TransportPhase::Connect,
                    AttemptFailureClass::Protocol,
                    false,
                    "request ID cannot be represented as an HTTP header",
                )
            })?,
        );
        let send_wait = remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(first_byte_timeout)?;
        let response = timeout(
            send_wait,
            client.post(url).headers(headers).body(body).send(),
        )
        .await
        .map_err(|_| first_byte_timeout())?
        .map_err(map_send_error)?;
        if !response.status().is_success() {
            return Err(self.map_error_response(response, attempt_deadline).await);
        }
        require_content_type(&response, "application/json")?;
        read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes,
        )
        .await
    }

    pub(in crate::openai::transport) async fn map_error_response(
        &self,
        response: Response,
        attempt_deadline: Instant,
    ) -> TransportError {
        let status = response.status();
        let first_byte_deadline = Instant::now() + self.config.timeouts.first_byte;
        let message = match read_bounded_body(
            response,
            first_byte_deadline,
            attempt_deadline,
            self.config.timeouts.idle,
            self.config.max_response_bytes.min(64 * 1024),
        )
        .await
        {
            Ok(body) => safe_upstream_error_message(status, &body, self.api_key.expose()),
            Err(_) => format!("OpenAI returned HTTP {status}"),
        };
        let class = if status == StatusCode::REQUEST_TIMEOUT {
            AttemptFailureClass::Timeout
        } else if status == StatusCode::TOO_MANY_REQUESTS {
            AttemptFailureClass::RateLimit
        } else if status.is_server_error() {
            AttemptFailureClass::UpstreamServer
        } else {
            AttemptFailureClass::UpstreamClient
        };
        transport_error(TransportPhase::FirstByte, class, false, message)
    }
}
