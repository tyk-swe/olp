use super::*;

// ---------------------------------------------------------------------------
// Distributed limits
//
// docs/architecture.md "Distributed limit semantics": RPM, TPM and concurrency
// are decided by one atomic reservation against Valkey server time, which also
// derives `Retry-After`; "A rejection consumes no dimension".
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn a_key_over_its_request_limit_is_refused_with_a_retry_after() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("rpm probe", json!({"requests_per_minute": 1}))
            .await
            .expect("rate-limited key");

        // Issuing the key already spent a request against the gateway's model
        // listing, so drive the limit from a known state: send until a 429
        // arrives, bounded, and assert the shape of the refusal.
        let checkpoint = world.mock.checkpoint();
        let mut refusal = None;
        let mut accepted = 0;
        for _ in 0..4 {
            let response = world
                .gateway_post(
                    "/openai/v1/chat/completions",
                    json!({
                        "model": world::OPENAI_ROUTE,
                        "messages": [{"role": "user", "content": nonce("rpm")}]
                    }),
                    &key.secret,
                )
                .await
                .expect("chat completion");
            if response.status == 429 {
                refusal = Some(response);
                break;
            }
            assert_eq!(
                response.status, 200,
                "an in-limit request failed with {}: {}",
                response.status, response.text
            );
            accepted += 1;
        }

        let refusal = refusal.unwrap_or_else(|| {
            panic!("a key limited to one request per minute served {accepted} requests without refusing any")
        });
        assert!(
            accepted <= 1,
            "a key limited to one request per minute served {accepted} before refusing"
        );

        let retry_after = refusal
            .header("retry-after")
            .unwrap_or_else(|| panic!("the 429 carries no Retry-After: {}", refusal.text));
        let seconds: u64 = retry_after.parse().unwrap_or_else(|_| {
            panic!("Retry-After must be a delay in seconds; got {retry_after:?}")
        });
        assert!(
            (1..=60).contains(&seconds),
            "Retry-After is derived from the remaining fixed minute window, so \
             it must fall in 1..=60; got {seconds}"
        );

        // "A rejection consumes no dimension" — and a refused request must not
        // reach the provider at all.
        let upstream = world.mock.since(checkpoint);
        assert_eq!(
            upstream.len(),
            accepted,
            "{accepted} admitted requests produced {} upstream calls, so a \
             refused request still reached the provider",
            upstream.len()
        );
    });
}
