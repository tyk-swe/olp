use super::*;

// ---------------------------------------------------------------------------
// Data-safety invariants
//
// docs/architecture.md "Data-safety invariants": durable request, attempt and
// usage records hold "only identifiers, timing, token or media units, status,
// error classification, and pricing provenance — never prompts, responses,
// reasoning, tool arguments or results, uploads, raw headers, or credentials".
// ---------------------------------------------------------------------------

#[test]
#[ignore = "end-to-end; run via make e2e"]
fn no_durable_row_holds_prompt_text_or_a_credential() {
    runtime().block_on(async {
        let world = world();
        let key = world
            .issue_key("data-safety probe", json!({}))
            .await
            .expect("dedicated key");
        let prompt = nonce("secret-prompt");

        let response = world
            .gateway_post(
                "/openai/v1/chat/completions",
                json!({
                    "model": world::OPENAI_ROUTE,
                    "messages": [{"role": "user", "content": prompt}]
                }),
                &key.secret,
            )
            .await
            .expect("chat completion");
        assert_eq!(response.status, 200, "setup call failed: {}", response.text);

        // The record must exist before its absence of prompt text means
        // anything: scanning before ingestion would pass vacuously.
        world
            .await_request_rows(&key.id, &route_filter(), 1)
            .await
            .expect("the request is logged");

        let sightings = durable::rows_containing(&world.database_url, &prompt)
            .await
            .expect("database scan");
        assert!(
            sightings.is_empty(),
            "prompt text reached durable storage in {} table(s):\n{}",
            sightings.len(),
            durable::describe(&sightings)
        );

        // The proxy key secret is a credential; only its hash and lookup id may
        // be stored.
        let secret_sightings = durable::rows_containing(&world.database_url, &key.secret)
            .await
            .expect("database scan");
        assert!(
            secret_sightings.is_empty(),
            "the API key secret is stored in the clear in {} table(s):\n{}",
            secret_sightings.len(),
            durable::describe(&secret_sightings)
        );

        // Provider credentials are encrypted at rest, so a clean result here is
        // weak evidence — the scan cannot see through encryption. It is kept
        // for the direction that matters: it fails loudly if a change ever
        // writes one in the clear.
        let credential_sightings =
            durable::rows_containing(&world.database_url, mock_upstream::COMPAT_CREDENTIAL)
                .await
                .expect("database scan");
        assert!(
            credential_sightings.is_empty(),
            "a provider credential is stored in the clear in {} table(s):\n{}",
            credential_sightings.len(),
            durable::describe(&credential_sightings)
        );
    });
}
