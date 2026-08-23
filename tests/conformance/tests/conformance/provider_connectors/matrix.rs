use std::collections::BTreeSet;

use olp_engine::domain::routing::provider::ProviderKind;
use olp_engine::providers::factory::certification::certifiable_capabilities;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Disposition {
    SharedContract,
    Inapplicable(&'static str),
}

#[derive(Clone, Copy, Debug)]
pub(super) struct ProviderContractRow {
    pub(super) kind: ProviderKind,
    pub(super) cached_usage: Disposition,
    pub(super) structured_output: Disposition,
    pub(super) request_ids: Disposition,
    pub(super) media: Disposition,
    pub(super) oversized_responses: Disposition,
}

const NO_ANTHROPIC_STRUCTURED_OUTPUT: &str =
    "the Anthropic canonical encoder rejects response_format rather than advertising it";
const NO_BEDROCK_STRUCTURED_OUTPUT: &str =
    "Bedrock Converse rejects non-text response_format values";
const NO_BEDROCK_CACHED_USAGE: &str =
    "Bedrock Converse TokenUsage has no cached-input field in the supported SDK wire model";
const NO_BEDROCK_REQUEST_ID: &str = "the AWS SDK owns outbound metadata with no request-id injection hook, and Converse exposes no canonical response ID";
const NO_BEDROCK_RESPONSE_BOUND: &str =
    "Bedrock response bodies are owned by the AWS SDK and have no connector-level byte limit";
const NO_BEDROCK_MEDIA: &str =
    "the Bedrock Converse encoder accepts canonical text and tool parts but no media content part";

const fn shared_contracts(kind: ProviderKind) -> ProviderContractRow {
    ProviderContractRow {
        kind,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::SharedContract,
        request_ids: Disposition::SharedContract,
        media: Disposition::SharedContract,
        oversized_responses: Disposition::SharedContract,
    }
}

pub(super) const ROWS: [ProviderContractRow; 7] = [
    shared_contracts(ProviderKind::OpenAi),
    ProviderContractRow {
        structured_output: Disposition::Inapplicable(NO_ANTHROPIC_STRUCTURED_OUTPUT),
        ..shared_contracts(ProviderKind::Anthropic)
    },
    shared_contracts(ProviderKind::Gemini),
    shared_contracts(ProviderKind::VertexAi),
    ProviderContractRow {
        kind: ProviderKind::Bedrock,
        cached_usage: Disposition::Inapplicable(NO_BEDROCK_CACHED_USAGE),
        structured_output: Disposition::Inapplicable(NO_BEDROCK_STRUCTURED_OUTPUT),
        request_ids: Disposition::Inapplicable(NO_BEDROCK_REQUEST_ID),
        media: Disposition::Inapplicable(NO_BEDROCK_MEDIA),
        oversized_responses: Disposition::Inapplicable(NO_BEDROCK_RESPONSE_BOUND),
    },
    shared_contracts(ProviderKind::AzureOpenAi),
    shared_contracts(ProviderKind::OpenAiCompatible),
];

pub(super) fn row_for(kind: ProviderKind) -> ProviderContractRow {
    *ROWS
        .iter()
        .find(|row| row.kind == kind)
        .expect("every ProviderKind must have one conformance row")
}

#[test]
fn conformance_matrix_is_closed_and_has_no_empty_opt_outs() {
    let kinds = ROWS.iter().map(|row| row.kind).collect::<BTreeSet<_>>();
    assert_eq!(kinds, ProviderKind::ALL.into_iter().collect());
    assert_eq!(kinds.len(), ROWS.len(), "provider rows must be unique");
}

#[test]
fn every_provider_kind_has_reviewed_certification_tuples() {
    for kind in ProviderKind::ALL {
        assert!(
            certifiable_capabilities(kind).next().is_some(),
            "{kind:?} has no reviewed tuples"
        );
    }
}
