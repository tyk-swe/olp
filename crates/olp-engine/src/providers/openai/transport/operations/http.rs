use crate::domain::ports::{ProviderRequest, TransportError, TransportPhase};
use http::{HeaderValue, header};
use reqwest::{Method, Response, multipart};
use tokio::time::{Instant, timeout};

use super::super::{Connector, errors::*, streams::*};
use crate::providers::transport_common::{request_id_header, upstream_response_error};
use crate::providers::transport_io::bounded_duration;

impl Connector {
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
        let mut headers = self.base_headers(request)?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        let response = RESPONSE_IO
            .send_before(
                client.post(url).headers(headers).body(body),
                Instant::now() + self.config.timeouts.first_byte,
                attempt_deadline,
                map_send_error,
            )
            .await?;
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
        let mut headers = self.base_headers(request)?;
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
        let mut headers = self.base_headers(request)?;
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
        let mut headers = self.base_headers(request)?;
        headers.insert(
            header::CONTENT_TYPE,
            HeaderValue::from_static("application/json"),
        );
        headers.insert(header::ACCEPT, HeaderValue::from_static("application/json"));
        headers.insert(
            "x-request-id",
            request_id_header(request.metadata.request_id)?,
        );
        let response = RESPONSE_IO
            .send_before(
                client.post(url).headers(headers).body(body),
                first_byte_deadline,
                attempt_deadline,
                map_send_error,
            )
            .await?;
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

    pub(in crate::providers::openai::transport) async fn map_error_response(
        &self,
        response: Response,
        attempt_deadline: Instant,
    ) -> TransportError {
        let status = response.status();
        let headers = response.headers().clone();
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
        upstream_response_error(TransportPhase::FirstByte, status, &headers, message)
    }
}
