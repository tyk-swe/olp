use serde::{Deserialize, Serialize};

use crate::usage::emitter::Event;

const VERSION: u64 = 1;

#[derive(Serialize)]
struct Envelope<'a> {
    version: u64,
    #[serde(flatten)]
    event: &'a Event,
}

pub(super) fn encode(event: &Event) -> Result<String, serde_json::Error> {
    serde_json::to_string(&Envelope {
        version: VERSION,
        event,
    })
}

pub(crate) enum Decoded {
    Event(Box<Event>),
    /// Written by a newer wire version; retain it for a compatible reader.
    Unsupported,
}

pub(crate) fn decode(payload: &[u8]) -> Result<Decoded, serde_json::Error> {
    // The version scan skips every other field without allocating, so it is
    // far cheaper than the event parse it guards.
    #[derive(Deserialize)]
    struct Version {
        version: Option<u64>,
    }
    let envelope: Version = serde_json::from_slice(payload)?;
    if envelope.version.is_some_and(|version| version != VERSION) {
        return Ok(Decoded::Unsupported);
    }
    serde_json::from_slice(payload).map(|event| Decoded::Event(Box::new(event)))
}

#[cfg(test)]
mod tests {
    use super::{Decoded, decode};

    #[test]
    fn legacy_and_current_metadata_are_readable_but_future_versions_are_retained() {
        assert!(decode(br#"{"request_id":"legacy"}"#).is_err());
        assert!(decode(br#"{"version":1,"future_optional_field":true}"#).is_err());
        assert!(matches!(
            decode(br#"{"version":2,"unknown_required_semantics":true}"#),
            Ok(Decoded::Unsupported)
        ));
        assert!(matches!(
            decode(br#"{"version":0}"#),
            Ok(Decoded::Unsupported)
        ));
        assert!(decode(b"invalid json").is_err());
    }
}
