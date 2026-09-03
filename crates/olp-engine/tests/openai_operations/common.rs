use olp_engine::domain::canonical::{events::validate_event_sequence, requests::MediaHandle};
use olp_engine::protocols::openai::{
    client::Encoder as ResponseEncoder, media::BoundedMediaPart,
    responses::stream::Decoder as ResponseDecoder, video::OpenAiVideoObject,
};

pub(super) fn media_part(handle: &str, filename: &str, length: u64) -> BoundedMediaPart {
    BoundedMediaPart::new(
        MediaHandle::new(handle),
        filename,
        Some("application/octet-stream".into()),
        length,
        2 * 1024 * 1024,
    )
    .unwrap()
}

pub(super) fn video_object(id: &str, status: &str) -> OpenAiVideoObject {
    OpenAiVideoObject {
        id: id.into(),
        object: "video".into(),
        model: "sora-2".into(),
        status: status.into(),
        progress: Some(100.0),
        created_at: Some(1_800_000_000),
        completed_at: Some(1_800_000_010),
        expires_at: None,
        prompt: Some("a calm ocean".into()),
        seconds: Some("8".into()),
        size: Some("1280x720".into()),
        remixed_from_video_id: None,
        error: None,
        extra: Default::default(),
    }
}

pub(super) fn responses_stream_frames(wire: &str, client_model: &str) -> Vec<serde_json::Value> {
    let mut decoder = ResponseDecoder::new();
    let mut events = decoder.push(wire.as_bytes()).unwrap();
    events.extend(decoder.finish().unwrap());
    validate_event_sequence(&events).unwrap();
    let mut encoder = ResponseEncoder::new(client_model, "resp_fallback", 1_800_000_000);
    events
        .into_iter()
        .flat_map(|event| encoder.push(event).unwrap())
        .map(|frame| serde_json::from_str(&frame.data).unwrap())
        .collect()
}
