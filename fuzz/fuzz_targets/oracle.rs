//! Shared oracles for the fuzz targets.
//!
//! Included with `mod oracle;` from each target that needs it.

use serde::Serialize;
use serde::de::DeserializeOwned;

/// Drives one decode/encode pair through the two invariants a codec owes its
/// callers.
///
/// 1. **Self-compatibility.** Whatever the encoder emits must be accepted by
///    the matching decoder. An encoder that produces a document its own
///    decoder rejects cannot round-trip through a provider either.
/// 2. **Idempotence.** Encoding a value that already came from the encoder
///    must reproduce it exactly. Drift on the second pass means the
///    translation is lossy or non-deterministic.
///
/// The *first* encode is deliberately excluded from the comparison: it
/// substitutes the upstream model name, so `encode(decode(x)) == x` is false
/// by design. Comparing the second pass to the first isolates real loss.
///
/// `decode` returning `None` is not a failure — the fuzzer's bytes merely
/// parsed as some other DTO. Only behaviour *after* a successful decode is
/// asserted. Likewise a failing first encode is legitimate: the canonical
/// value may carry something this surface cannot express.
///
/// Values are compared as `serde_json::Value` rather than through `PartialEq`
/// so the oracle applies uniformly to DTOs that do not derive it, and so a
/// failure prints the offending document.
pub fn roundtrip<Dto, Canonical, Decode, Encode, Error>(
    data: &[u8],
    label: &str,
    decode: Decode,
    encode: Encode,
) where
    Dto: Serialize + DeserializeOwned,
    Decode: Fn(Dto) -> Option<Canonical>,
    Encode: Fn(&Canonical) -> Result<Dto, Error>,
{
    let Ok(parsed) = serde_json::from_slice::<Dto>(data) else {
        return;
    };
    let Some(canonical) = decode(parsed) else {
        return;
    };
    let Ok(first) = encode(&canonical) else {
        return;
    };
    let first_json = serde_json::to_value(&first)
        .unwrap_or_else(|error| panic!("{label}: encoder output is not serialisable: {error}"));

    let reparsed: Dto = serde_json::from_value(first_json.clone()).unwrap_or_else(|error| {
        panic!("{label}: encoder emitted a document its own decoder rejects: {error}\n{first_json}")
    });
    let Some(round_tripped) = decode(reparsed) else {
        panic!(
            "{label}: decoding the encoder's own output yielded a different operation\n{first_json}"
        )
    };
    let second = encode(&round_tripped).unwrap_or_else(|_| {
        panic!("{label}: re-encoding a round-tripped value failed\n{first_json}")
    });
    let second_json = serde_json::to_value(&second)
        .unwrap_or_else(|error| panic!("{label}: re-encoded output is not serialisable: {error}"));

    assert_eq!(
        first_json, second_json,
        "{label}: encoding is not idempotent; the second pass differs from the first"
    );
}
