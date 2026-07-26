use std::future::ready;

use futures::{StreamExt, stream};
use olp_domain::{
    AttemptFailureClass, CanonicalEvent, ProviderEventStream, TransportError, TransportPhase,
};
use olp_protocols::anthropic::AnthropicMessagesStreamDecoder;
use reqwest::Response;
use tokio::time::{Instant, timeout};

use crate::transport_io::{
    CanonicalEventDecoder, DecodedEventStream, ProviderResponseIo, ReqwestByteStream,
};

use super::{errors::transport_error, operations::AnthropicConnector};

const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new("Anthropic");

impl CanonicalEventDecoder for AnthropicMessagesStreamDecoder {
    type Error = olp_protocols::anthropic::StreamError;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::push(self, bytes)
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::finish(self)
    }
}

impl AnthropicConnector {
    pub(super) async fn streaming_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        preserve_raw_frames: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        RESPONSE_IO.require_content_type(&response, "text/event-stream")?;
        let mut source: ReqwestByteStream = Box::pin(response.bytes_stream());
        let first_wait = RESPONSE_IO
            .remaining_until(first_byte_deadline, attempt_deadline)
            .ok_or_else(|| RESPONSE_IO.first_byte_timeout())?;
        let first = timeout(first_wait, source.next())
            .await
            .map_err(|_| RESPONSE_IO.first_byte_timeout())?
            .ok_or_else(|| {
                transport_error(
                    TransportPhase::FirstByte,
                    AttemptFailureClass::Protocol,
                    false,
                    "Anthropic stream ended before its first body byte",
                )
            })?
            .map_err(|error| RESPONSE_IO.map_first_body_error(error))?;
        let source = Box::pin(stream::once(ready(Ok(first))).chain(source));
        let bytes = RESPONSE_IO.after_first_byte_stream(
            source,
            self.config.timeouts.idle,
            attempt_deadline,
        );
        let decoder = AnthropicMessagesStreamDecoder::with_max_event_bytes_and_raw_passthrough(
            self.config.max_event_bytes,
            preserve_raw_frames,
        );
        Ok(Box::pin(DecodedEventStream::new(
            RESPONSE_IO,
            bytes,
            decoder,
        )))
    }
}
