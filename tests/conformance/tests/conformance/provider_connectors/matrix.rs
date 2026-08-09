use std::collections::BTreeSet;

use olp_domain::{OperationKind, ProviderKind, Surface, TransportMode};
use olp_providers::{certifiable_capabilities, supports_capability_certification};

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

pub(super) const ROWS: [ProviderContractRow; 7] = [
    ProviderContractRow {
        kind: ProviderKind::OpenAi,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::SharedContract,
        request_ids: Disposition::SharedContract,
        media: Disposition::SharedContract,
        oversized_responses: Disposition::SharedContract,
    },
    ProviderContractRow {
        kind: ProviderKind::Anthropic,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::Inapplicable(NO_ANTHROPIC_STRUCTURED_OUTPUT),
        request_ids: Disposition::SharedContract,
        media: Disposition::SharedContract,
        oversized_responses: Disposition::SharedContract,
    },
    ProviderContractRow {
        kind: ProviderKind::Gemini,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::SharedContract,
        request_ids: Disposition::SharedContract,
        media: Disposition::SharedContract,
        oversized_responses: Disposition::SharedContract,
    },
    ProviderContractRow {
        kind: ProviderKind::VertexAi,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::SharedContract,
        request_ids: Disposition::SharedContract,
        media: Disposition::SharedContract,
        oversized_responses: Disposition::SharedContract,
    },
    ProviderContractRow {
        kind: ProviderKind::Bedrock,
        cached_usage: Disposition::Inapplicable(NO_BEDROCK_CACHED_USAGE),
        structured_output: Disposition::Inapplicable(NO_BEDROCK_STRUCTURED_OUTPUT),
        request_ids: Disposition::Inapplicable(NO_BEDROCK_REQUEST_ID),
        media: Disposition::Inapplicable(NO_BEDROCK_MEDIA),
        oversized_responses: Disposition::Inapplicable(NO_BEDROCK_RESPONSE_BOUND),
    },
    ProviderContractRow {
        kind: ProviderKind::AzureOpenAi,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::SharedContract,
        request_ids: Disposition::SharedContract,
        media: Disposition::SharedContract,
        oversized_responses: Disposition::SharedContract,
    },
    ProviderContractRow {
        kind: ProviderKind::OpenAiCompatible,
        cached_usage: Disposition::SharedContract,
        structured_output: Disposition::SharedContract,
        request_ids: Disposition::SharedContract,
        media: Disposition::SharedContract,
        oversized_responses: Disposition::SharedContract,
    },
];

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
        Contract::RetryAfter => Disposition::Inapplicable(
            "TransportError does not carry upstream Retry-After metadata; retryability is the current contract",
        ),
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
        let reviewed = certifiable_capabilities(kind).collect::<BTreeSet<_>>();
        assert!(!reviewed.is_empty(), "{kind:?} has no reviewed tuples");
        for operation in OperationKind::ALL {
            for surface in Surface::ALL {
                for mode in TransportMode::ALL {
                    assert_eq!(
                        reviewed.contains(&(operation, surface, mode)),
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
