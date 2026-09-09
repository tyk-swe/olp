use crate::inference::transport::ProviderEventStream;
use crate::inference::transport::TransportError;
use crate::protocols::anthropic::stream::Decoder;
use crate::protocols::canonical::events::Event;
use reqwest::Response;
use tokio::time::Instant;

use crate::providers::anthropic::transport::errors::RESPONSE_IO;
use crate::providers::transport_io::event_stream::CanonicalEventDecoder;

use crate::providers::anthropic::transport::operations::Connector;

impl CanonicalEventDecoder for Decoder {
    type Error = crate::protocols::anthropic::stream::Error;

    fn push(&mut self, bytes: &[u8]) -> Result<Vec<Event>, Self::Error> {
        Self::push(self, bytes)
    }

    fn finish(&mut self) -> Result<Vec<Event>, Self::Error> {
        Self::finish(self)
    }
}

impl Connector {
    pub(crate) async fn streaming_response(
        &self,
        response: Response,
        first_byte_deadline: Instant,
        attempt_deadline: Instant,
        preserve_raw_frames: bool,
    ) -> Result<ProviderEventStream, TransportError> {
        let decoder = Decoder::with_max_event_bytes_and_raw_passthrough(
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
