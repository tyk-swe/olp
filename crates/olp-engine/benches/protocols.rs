use std::{fs, hint::black_box, path::PathBuf};

use criterion::{BatchSize, BenchmarkId, Criterion, Throughput, criterion_group, criterion_main};
use olp_engine::{
    domain::canonical::{
        events::{Event, FinishReason, Kind},
        requests::{MessageRole, Operation},
    },
    protocols::{
        anthropic, gemini,
        openai::{
            chat::{CompletionRequest, client::ChatCompletionStreamEncoder, decode},
            client::Encoder as ResponsesStreamEncoder,
        },
        sse::Decoder as SseDecoder,
    },
};
use serde_json::{Value, json};

fn protocol_benches(criterion: &mut Criterion) {
    sse_decoder(criterion);
    request_translation(criterion);
    stream_encoders(criterion);
    json_codecs(criterion);
}

fn sse_decoder(criterion: &mut Criterion) {
    let corpus = corpus_files("fuzz/corpus/sse_decoder");
    let total_bytes = corpus.iter().map(Vec::len).sum::<usize>();
    let mut group = criterion.benchmark_group("sse_decoder_fuzz_corpus");
    group.throughput(Throughput::Bytes(total_bytes as u64));
    group.bench_function("all_seeds", |bencher| {
        bencher.iter(|| {
            for bytes in &corpus {
                let mut decoder = SseDecoder::default();
                let _ = black_box(decoder.push(black_box(bytes)));
                let _ = black_box(decoder.finish());
            }
        });
    });
    group.finish();
}

fn request_translation(criterion: &mut Criterion) {
    let source = json!({
        "model": "chat-route",
        "messages": [
            {"role": "system", "content": "Answer precisely."},
            {"role": "user", "content": "Summarize the benchmark."}
        ],
        "max_completion_tokens": 256,
        "temperature": 0.2,
        "stream": true
    });
    criterion.bench_function("openai_to_canonical_to_anthropic_and_gemini", |bencher| {
        bencher.iter_batched(
            || source.clone(),
            |source| {
                let wire: CompletionRequest = serde_json::from_value(source).unwrap();
                let Operation::Generation(canonical) = decode::chat_completion(wire).unwrap()
                else {
                    unreachable!();
                };
                let anthropic =
                    anthropic::translate::encode::request(&canonical, "bench-model").unwrap();
                let gemini = gemini::translate::encode::request(&canonical).unwrap();
                black_box((anthropic, gemini));
            },
            BatchSize::SmallInput,
        );
    });
}

fn stream_encoders(criterion: &mut Criterion) {
    let events = generation_events();
    let mut group = criterion.benchmark_group("openai_stream_encoders");
    group.bench_function("chat", |bencher| {
        bencher.iter_batched(
            || events.clone(),
            |events| {
                let mut encoder =
                    ChatCompletionStreamEncoder::new(uuid::Uuid::nil(), "bench-route", true, 0);
                for event in events {
                    black_box(encoder.push(event).unwrap());
                }
            },
            BatchSize::SmallInput,
        );
    });
    group.bench_function("responses", |bencher| {
        bencher.iter_batched(
            || events.clone(),
            |events| {
                let mut encoder = ResponsesStreamEncoder::new("bench-route", "resp_bench", 0);
                for event in events {
                    black_box(encoder.push(event).unwrap());
                }
            },
            BatchSize::SmallInput,
        );
    });
    group.finish();
}

fn json_codecs(criterion: &mut Criterion) {
    let fixtures = [
        (
            "routing_attempt_order",
            fixture("tests/fixtures/routing/attempt-order.json"),
        ),
        (
            "selected_operation_families",
            fixture("tests/fixtures/protocols/selected-operation-families.json"),
        ),
    ];
    let mut group = criterion.benchmark_group("largest_conformance_json_codecs");
    for (name, bytes) in fixtures {
        group.throughput(Throughput::Bytes(bytes.len() as u64));
        group.bench_with_input(
            BenchmarkId::from_parameter(name),
            &bytes,
            |bencher, bytes| {
                bencher.iter(|| {
                    let value: Value = serde_json::from_slice(bytes).unwrap();
                    black_box(serde_json::to_vec(&value).unwrap());
                });
            },
        );
    }
    group.finish();
}

fn generation_events() -> Vec<Event> {
    let mut kinds = vec![
        Kind::ResponseStart {
            response_id: Some("chatcmpl-bench".to_owned()),
            provider_model: Some("bench-model".to_owned()),
        },
        Kind::MessageStart {
            output_index: 0,
            role: MessageRole::Assistant,
        },
    ];
    kinds.extend((0..50).map(|index| Kind::TextDelta {
        output_index: 0,
        text: format!("token-{index} "),
    }));
    kinds.extend([
        Kind::Finish {
            output_index: 0,
            reason: FinishReason::Stop,
        },
        Kind::Done,
    ]);
    kinds
        .into_iter()
        .enumerate()
        .map(|(sequence, kind)| Event::new(sequence as u64, kind))
        .collect()
}

fn corpus_files(path: &str) -> Vec<Vec<u8>> {
    let mut paths = fs::read_dir(workspace_path(path))
        .unwrap()
        .map(|entry| entry.unwrap().path())
        .collect::<Vec<_>>();
    paths.sort();
    paths
        .into_iter()
        .map(|path| fs::read(path).unwrap())
        .collect()
}

fn fixture(path: &str) -> Vec<u8> {
    fs::read(workspace_path(path)).unwrap()
}

fn workspace_path(path: &str) -> PathBuf {
    PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .join(path)
}

criterion_group!(benches, protocol_benches);
criterion_main!(benches);
