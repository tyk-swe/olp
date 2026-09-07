use olp::protocols::canonical::events::FinishReason;
use olp::protocols::canonical::events::Kind;
use olp::protocols::canonical::identity::Surface;
use olp::protocols::canonical::requests::MessageRole;
use serde_json::Value;

pub(super) fn sse(event: &str, data: Value) -> String {
    format!("event: {event}\ndata: {data}\n\n")
}

pub(super) fn assistant_events(
    usage: olp::protocols::canonical::events::Usage,
    reason: FinishReason,
    extensions: Vec<(&str, Value)>,
) -> Vec<olp::protocols::canonical::events::Event> {
    use olp::protocols::canonical::events::Event;
    let mut events = vec![
        Event::new(
            0,
            Kind::ResponseStart {
                response_id: Some("msg_1".into()),
                provider_model: Some("upstream".into()),
            },
        ),
        Event::new(
            1,
            Kind::MessageStart {
                output_index: 0,
                role: MessageRole::Assistant,
            },
        ),
        Event::new(
            2,
            Kind::TextDelta {
                output_index: 0,
                text: "hello".into(),
            },
        ),
        Event::new(3, Kind::Usage { usage }),
    ];
    let mut next = 4;
    if !extensions.is_empty() {
        events.push(Event::new(
            next,
            Kind::SourceExtension {
                extensions: olp::protocols::canonical::requests::SourceExtensions::new(
                    Surface::Anthropic,
                    extensions
                        .into_iter()
                        .map(|(path, value)| (path.to_owned(), value))
                        .collect(),
                ),
            },
        ));
        next += 1;
    }
    events.push(Event::new(
        next,
        Kind::Finish {
            output_index: 0,
            reason,
        },
    ));
    events.push(Event::new(next + 1, Kind::Done));
    events
}
