use super::*;

// ---------------------------------------------------------------------------
// README examples
//
// README.md "Your first request" is the first thing a new operator runs, so it
// is executed here rather than paraphrased: each fenced block is extracted from
// the README by a stable marker comment and handed to `bash` unchanged.
//
// Only the two placeholders the section documents — the default origin and the
// example route slug — are substituted, so any other drift between the README
// and the product is a build failure. A trailing `--write-out` argument is
// appended solely to observe the status code curl would otherwise discard; it
// adds nothing the reader is expected to type.
// ---------------------------------------------------------------------------

const UNARY_MARKER: &str = "<!-- readme:first-request:curl -->";
const STREAM_MARKER: &str = "<!-- readme:first-request:curl-stream -->";
/// The origin README.md "Quick start" publishes, and "Your first request" uses.
const DOCUMENTED_ORIGIN: &str = "http://localhost:8080";
/// The example route slug the section tells the reader to replace.
const DOCUMENTED_ROUTE: &str = "my-route";

/// The `bash` block that follows `marker` in README.md, verbatim.
fn readme_block(marker: &str) -> String {
    let path = repo_root().join("README.md");
    let readme = fs::read_to_string(&path)
        .unwrap_or_else(|error| panic!("README.md must be readable at {path:?}: {error}"));

    let mut lines = readme.lines();
    assert!(
        lines.any(|line| line.trim() == marker),
        "README.md has no {marker} line; the \"Your first request\" section \
         must keep the markers this suite extracts its examples by"
    );

    let opening = lines
        .find(|line| !line.trim().is_empty())
        .unwrap_or_else(|| panic!("README.md ends right after {marker}"));
    assert_eq!(
        opening.trim(),
        "```bash",
        "the block after {marker} in README.md \"Your first request\" must be \
         a bash fence"
    );

    let mut block = Vec::new();
    let mut closed = false;
    for line in lines {
        if line.trim() == "```" {
            closed = true;
            break;
        }
        block.push(line);
    }
    assert!(
        closed,
        "the bash fence after {marker} in README.md \"Your first request\" is \
         never closed"
    );
    assert!(
        !block.is_empty(),
        "the bash fence after {marker} in README.md \"Your first request\" is \
         empty"
    );
    block.join("\n")
}

/// Runs the README block at `marker` against the harness, returning its status
/// code and body.
fn run_block(marker: &str) -> (String, String) {
    let world = world();
    let block = readme_block(marker);
    for placeholder in [DOCUMENTED_ORIGIN, DOCUMENTED_ROUTE] {
        assert!(
            block.contains(placeholder),
            "the block after {marker} in README.md no longer contains \
             {placeholder:?}; update the placeholder contract here and in \
             README.md together"
        );
    }

    let script = block
        .replace(DOCUMENTED_ORIGIN, world.origin())
        .replace(DOCUMENTED_ROUTE, world::OPENAI_ROUTE);
    let script = format!("{} --write-out '\\n%{{http_code}}'", script.trim_end());

    let output = std::process::Command::new("bash")
        .arg("-c")
        .arg(&script)
        .env("OLP_API_KEY", &world.api_key)
        .output()
        .unwrap_or_else(|error| panic!("running the README block at {marker}: {error}"));
    let stdout = String::from_utf8_lossy(&output.stdout).into_owned();
    let stderr = String::from_utf8_lossy(&output.stderr).into_owned();
    assert!(
        output.status.success(),
        "the README block at {marker} exited with {}:\nstdout: {stdout}\n\
         stderr: {stderr}",
        output.status
    );

    let (body, status) = stdout.rsplit_once('\n').unwrap_or_else(|| {
        panic!("the README block at {marker} produced no status line: {stdout:?}")
    });
    (status.to_owned(), body.to_owned())
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_readme_curl_example_answers_with_the_documented_shape() {
    // README.md "Your first request": the documented curl call answers with an
    // OpenAI chat completion whose text is in `choices[0].message.content`,
    // whose `finish_reason` is `stop` for a completed answer, and whose `usage`
    // carries the accounted prompt, completion and total token counts.
    let world = world();
    let checkpoint = world.mock.checkpoint();

    let (status, body) = run_block(UNARY_MARKER);
    assert_eq!(
        status, "200",
        "the README curl example returned {status}: {body}"
    );

    let answer: Value = serde_json::from_str(&body).unwrap_or_else(|error| {
        panic!("the README curl example answered non-JSON: {error}: {body}")
    });
    assert_eq!(
        answer["choices"][0]["message"]["content"],
        json!(mock_upstream::PLAIN_TEXT),
        "the reply text is not where README.md says it is: {answer}"
    );
    assert_eq!(
        answer["choices"][0]["finish_reason"],
        json!("stop"),
        "README.md documents `finish_reason` `stop` for a completed answer: \
         {answer}"
    );
    assert_eq!(
        answer["usage"]["prompt_tokens"],
        json!(mock_upstream::PROMPT_TOKENS),
        "usage.prompt_tokens: {answer}"
    );
    assert_eq!(
        answer["usage"]["completion_tokens"],
        json!(mock_upstream::COMPLETION_TOKENS),
        "usage.completion_tokens: {answer}"
    );
    assert_eq!(
        answer["usage"]["total_tokens"],
        json!(mock_upstream::TOTAL_TOKENS),
        "usage.total_tokens: {answer}"
    );

    assert_eq!(
        world.mock.since(checkpoint).len(),
        1,
        "the documented call must produce exactly one upstream call"
    );
}

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn the_readme_streaming_example_ends_with_done() {
    // README.md "Your first request": the streaming variant answers an event
    // stream whose `data:` chunks carry `choices[0].delta` fragments of the
    // reply, in which exactly one chunk reports a non-null `finish_reason` —
    // `stop` — before the `data: [DONE]` sentinel closes the stream.
    let (status, body) = run_block(STREAM_MARKER);
    assert_eq!(
        status, "200",
        "the README streaming example returned {status}: {body}"
    );

    let stream = sse::decode(body.as_bytes()).expect("the documented stream decodes");
    assert!(
        stream.undispatched_tail.is_empty(),
        "the stream ended mid-event, leaving {:?} undispatched",
        stream.undispatched_tail
    );
    let data: Vec<&str> = stream
        .events
        .iter()
        .map(|event| event.data.as_str())
        .collect();
    assert_eq!(
        data.last(),
        Some(&"[DONE]"),
        "README.md documents the [DONE] sentinel as the last event: {data:?}"
    );

    let chunks: Vec<Value> = data[..data.len() - 1]
        .iter()
        .map(|payload| {
            serde_json::from_str(payload)
                .unwrap_or_else(|error| panic!("chunk {payload:?} is not JSON: {error}"))
        })
        .collect();
    assert!(!chunks.is_empty(), "the stream carried no chunks");

    let finishes: Vec<&Value> = chunks
        .iter()
        .map(|chunk| &chunk["choices"][0]["finish_reason"])
        .filter(|reason| !reason.is_null())
        .collect();
    assert_eq!(
        finishes.len(),
        1,
        "README.md documents exactly one chunk with a finish_reason; saw \
         {finishes:?}"
    );
    assert_eq!(*finishes[0], json!("stop"));

    let text: String = chunks
        .iter()
        .filter_map(|chunk| chunk["choices"][0]["delta"]["content"].as_str())
        .collect();
    assert_eq!(
        text,
        mock_upstream::PLAIN_TEXT,
        "the concatenated deltas do not reconstruct the reply README.md \
         promises"
    );
}
