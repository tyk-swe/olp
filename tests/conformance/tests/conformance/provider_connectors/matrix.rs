use std::collections::BTreeSet;

use olp_engine::domain::{OperationKind, ProviderKind, Surface, TransportMode};
use olp_engine::providers::{certifiable_capabilities, supports_capability_certification};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(super) enum Contract {
    EndpointAndAuthentication,
    SecretRedaction,
    UnaryGeneration,
    StreamingAndTerminalFraming,
    Usage,
    CachedInputUsage,
    ToolCalls,
    StructuredOutput,
    ProviderRequestIds,
    ErrorClassification,
    Retryability,
    RetryAfter,
    Deadlines,
    Cancellation,
    InvalidBodies,
    OversizedResponses,
    InvalidEventSequences,
    CapabilityCertification,
    MediaOrMultipart,
}

impl Contract {
    pub(super) const ALL: [Self; 19] = [
        Self::EndpointAndAuthentication,
        Self::SecretRedaction,
        Self::UnaryGeneration,
        Self::StreamingAndTerminalFraming,
        Self::Usage,
        Self::CachedInputUsage,
        Self::ToolCalls,
        Self::StructuredOutput,
        Self::ProviderRequestIds,
        Self::ErrorClassification,
        Self::Retryability,
        Self::RetryAfter,
        Self::Deadlines,
        Self::Cancellation,
        Self::InvalidBodies,
        Self::OversizedResponses,
        Self::InvalidEventSequences,
        Self::CapabilityCertification,
        Self::MediaOrMultipart,
    ];
}

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
    pub(super) retry_after: Disposition,
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
const NO_BEDROCK_RETRY_AFTER: &str =
    "the AWS SDK error path does not expose the upstream Retry-After header to the connector";
const NO_BEDROCK_MEDIA: &str =
    "the Bedrock Converse encoder accepts canonical text and tool parts but no media content part";

const fn shared_contracts(kind: ProviderKind) -> ProviderContractRow {
    ProviderContractRow {
        kind,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::SharedContract,
        request_ids: Disposition::SharedContract,
        retry_after: Disposition::SharedContract,
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
        retry_after: Disposition::Inapplicable(NO_BEDROCK_RETRY_AFTER),
        media: Disposition::Inapplicable(NO_BEDROCK_MEDIA),
        oversized_responses: Disposition::Inapplicable(NO_BEDROCK_RESPONSE_BOUND),
    },
    shared_contracts(ProviderKind::AzureOpenAi),
    shared_contracts(ProviderKind::OpenAiCompatible),
];

pub(super) type CapabilityTuple = (OperationKind, Surface, TransportMode);

macro_rules! capability {
    ($operation:ident, $surface:ident, $mode:ident) => {
        (
            OperationKind::$operation,
            Surface::$surface,
            TransportMode::$mode,
        )
    };
}

const SHARED_NATIVE_CAPABILITIES: [CapabilityTuple; 9] = [
    capability!(Generation, OpenAi, Unary),
    capability!(Generation, OpenAi, Streaming),
    capability!(Generation, Anthropic, Unary),
    capability!(Generation, Anthropic, Streaming),
    capability!(Generation, Gemini, Unary),
    capability!(Generation, Gemini, Streaming),
    capability!(TokenCount, OpenAi, Unary),
    capability!(TokenCount, Anthropic, Unary),
    capability!(TokenCount, Gemini, Unary),
];

const OPENAI_NATIVE_EXTRA_CAPABILITIES: [CapabilityTuple; 16] = [
    capability!(Embeddings, OpenAi, Unary),
    capability!(ImageGeneration, OpenAi, Unary),
    capability!(ImageGeneration, OpenAi, Streaming),
    capability!(ImageEdit, OpenAi, Unary),
    capability!(ImageEdit, OpenAi, Streaming),
    capability!(ImageVariation, OpenAi, Unary),
    capability!(Speech, OpenAi, Unary),
    capability!(Speech, OpenAi, Streaming),
    capability!(Transcription, OpenAi, Unary),
    capability!(Transcription, OpenAi, Streaming),
    capability!(VideoCreate, OpenAi, Async),
    capability!(VideoList, OpenAi, Unary),
    capability!(VideoGet, OpenAi, Unary),
    capability!(VideoContent, OpenAi, Unary),
    capability!(VideoDelete, OpenAi, Unary),
    capability!(Moderation, OpenAi, Unary),
];

const OPENAI_COMPATIBLE_CAPABILITIES: [CapabilityTuple; 5] = [
    capability!(Generation, OpenAi, Unary),
    capability!(Generation, OpenAi, Streaming),
    capability!(Embeddings, OpenAi, Unary),
    capability!(TokenCount, OpenAi, Unary),
    capability!(Moderation, OpenAi, Unary),
];

const AZURE_OPENAI_EXTRA_CAPABILITIES: [CapabilityTuple; 2] = [
    capability!(Embeddings, OpenAi, Unary),
    capability!(Moderation, OpenAi, Unary),
];

pub(super) fn expected_certifiable_capabilities(kind: ProviderKind) -> BTreeSet<CapabilityTuple> {
    match kind {
        ProviderKind::OpenAi => capability_set(
            SHARED_NATIVE_CAPABILITIES
                .into_iter()
                .chain(OPENAI_NATIVE_EXTRA_CAPABILITIES),
        ),
        ProviderKind::Anthropic
        | ProviderKind::Gemini
        | ProviderKind::VertexAi
        | ProviderKind::Bedrock => capability_set(SHARED_NATIVE_CAPABILITIES),
        ProviderKind::AzureOpenAi => capability_set(
            SHARED_NATIVE_CAPABILITIES
                .into_iter()
                .chain(AZURE_OPENAI_EXTRA_CAPABILITIES),
        ),
        ProviderKind::OpenAiCompatible => capability_set(OPENAI_COMPATIBLE_CAPABILITIES),
    }
}

fn capability_set(tuples: impl IntoIterator<Item = CapabilityTuple>) -> BTreeSet<CapabilityTuple> {
    tuples.into_iter().collect()
}

pub(super) fn row_for(kind: ProviderKind) -> ProviderContractRow {
    *ROWS
        .iter()
        .find(|row| row.kind == kind)
        .expect("every ProviderKind must have one conformance row")
}

pub(super) fn disposition(row: ProviderContractRow, contract: Contract) -> Disposition {
    match contract {
        Contract::CachedInputUsage => row.cached_usage,
        Contract::StructuredOutput => row.structured_output,
        Contract::ProviderRequestIds => row.request_ids,
        Contract::RetryAfter => row.retry_after,
        Contract::MediaOrMultipart => row.media,
        Contract::OversizedResponses => row.oversized_responses,
        Contract::EndpointAndAuthentication
        | Contract::SecretRedaction
        | Contract::UnaryGeneration
        | Contract::StreamingAndTerminalFraming
        | Contract::Usage
        | Contract::ToolCalls
        | Contract::ErrorClassification
        | Contract::Retryability
        | Contract::Deadlines
        | Contract::Cancellation
        | Contract::InvalidBodies
        | Contract::InvalidEventSequences
        | Contract::CapabilityCertification => Disposition::SharedContract,
    }
}

#[test]
fn conformance_matrix_is_closed_and_has_no_empty_opt_outs() {
    let kinds = ROWS.iter().map(|row| row.kind).collect::<BTreeSet<_>>();
    assert_eq!(kinds, ProviderKind::ALL.into_iter().collect());
    assert_eq!(kinds.len(), ROWS.len(), "provider rows must be unique");

    for row in ROWS {
        for contract in Contract::ALL {
            if let Disposition::Inapplicable(reason) = disposition(row, contract) {
                assert!(
                    reason.len() >= 24 && !reason.contains("unsupported"),
                    "{row:?} {contract:?} needs a narrow technical reason"
                );
            }
        }
    }
}

#[test]
fn every_certifiable_tuple_is_in_the_shared_certification_contract() {
    for kind in ProviderKind::ALL {
        let expected = expected_certifiable_capabilities(kind);
        let reviewed = certifiable_capabilities(kind).collect::<BTreeSet<_>>();
        assert!(!expected.is_empty(), "{kind:?} has no reviewed tuples");
        assert_eq!(reviewed, expected, "{kind:?} certification matrix drift");
        for operation in OperationKind::ALL {
            for surface in Surface::ALL {
                for mode in TransportMode::ALL {
                    assert_eq!(
                        expected.contains(&(operation, surface, mode)),
                        supports_capability_certification(kind, operation, surface, mode),
                        "matrix drift for {kind:?} {operation:?} {surface:?} {mode:?}"
                    );
                }
            }
        }
        assert_eq!(
            disposition(row_for(kind), Contract::CapabilityCertification),
            Disposition::SharedContract,
        );
    }
}
