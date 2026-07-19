use olp_conformance_fixtures::read_json;
use olp_domain::{
    AttemptFailureClass, OperationKind, RouteSlug, RoutingError, RuntimeSnapshot, Surface,
    TransportError, TransportMode, TransportPhase, select_attempts,
};
use serde::Deserialize;

#[derive(Debug, Deserialize)]
struct RoutingFixture {
    snapshot: RuntimeSnapshot,
    cases: Vec<RoutingCase>,
}

#[derive(Debug, Deserialize)]
struct RoutingCase {
    name: String,
    route: String,
    operation: OperationKind,
    surface: Surface,
    mode: TransportMode,
    affinity: String,
    expected_provider_ids: Option<Vec<String>>,
    expected_error: Option<String>,
}

#[derive(Debug, Deserialize)]
struct RetryCase {
    name: String,
    phase: TransportPhase,
    class: AttemptFailureClass,
    response_committed: bool,
    allows_failover: bool,
}

#[test]
fn routing_fixture_is_valid_deterministic_and_capability_filtered() {
    let fixture: RoutingFixture = read_json("routing/attempt-order.json");
    fixture
        .snapshot
        .validate()
        .expect("snapshot fixture must be valid");

    for case in fixture.cases {
        let route = RouteSlug::parse(&case.route).expect("fixture route slug must be valid");
        let result = select_attempts(
            &fixture.snapshot,
            &route,
            case.operation,
            case.surface,
            case.mode,
            case.affinity.as_bytes(),
        );
        match (case.expected_provider_ids, case.expected_error) {
            (Some(expected), None) => {
                let first = result.expect("routing case must select targets");
                let second = select_attempts(
                    &fixture.snapshot,
                    &route,
                    case.operation,
                    case.surface,
                    case.mode,
                    case.affinity.as_bytes(),
                )
                .expect("repeated routing case must select targets");
                assert_eq!(first, second, "{} was not deterministic", case.name);
                assert_eq!(
                    first
                        .iter()
                        .map(|attempt| attempt.provider_id.to_string())
                        .collect::<Vec<_>>(),
                    expected,
                    "unexpected attempt order for {}",
                    case.name
                );
            }
            (None, Some(expected)) => {
                let error = result.expect_err("routing case must fail");
                let actual = match error {
                    RoutingError::RouteNotFound(_) => "route_not_found",
                    RoutingError::OperationNotSupported { .. } => "operation_not_supported",
                    RoutingError::NoEligibleTargets { .. } => "no_eligible_targets",
                };
                assert_eq!(actual, expected, "unexpected error for {}", case.name);
            }
            _ => panic!("{} must declare exactly one expected result", case.name),
        }
    }
}

#[test]
fn retry_taxonomy_only_allows_precommit_transient_failures() {
    let cases: Vec<RetryCase> = read_json("routing/retry-taxonomy.json");
    assert!(!cases.is_empty());
    for case in cases {
        let error = TransportError {
            phase: case.phase,
            class: case.class,
            response_committed: case.response_committed,
            message: case.name.clone(),
        };
        assert_eq!(
            error.allows_failover(),
            case.allows_failover,
            "unexpected retry classification for {}",
            case.name
        );
    }
}
