use crate::domain::{CanonicalEvent, ProviderEventStream, TransportError};
use crate::protocols::gemini::GeminiGenerateContentStreamDecoder;
use reqwest::Response;
use tokio::time::Instant;

use crate::providers::transport_io::{CanonicalEventDecoder, ProviderResponseIo};

use super::operations::GeminiConnector;

const RESPONSE_IO: ProviderResponseIo = ProviderResponseIo::new("Gemini");

impl CanonicalEventDecoder for GeminiGenerateContentStreamDecoder {
    type Error = crate::protocols::gemini::StreamError;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::push(self, bytes)
    }

    fn finish(&mut self) -> Result<Vec<CanonicalEvent>, Self::Error> {
        Self::finish(self)
    }
}

impl GeminiConnector {
    pub(super) async fn streaming_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        preserve_raw_frames: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        let decoder = GeminiGenerateContentStreamDecoder::with_max_event_bytes_and_raw_passthrough(
            self.config.max_event_bytes,
            preserve_raw_frames,
        );
        RESPONSE_IO
            .decoded_event_stream(
                response,
                first_byte_deadline,
                attempt_deadline,
                self.config.timeouts.idle,
                decoder,
            )
            .await
    }
}
