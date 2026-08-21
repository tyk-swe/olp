use std::collections::BTreeSet;

use axum::http::Method;
use olp_engine::domain::{
    auth::GatewayCapability,
    canonical::identity::{OperationKind, Surface},
};

use super::{
    classification::InferenceEndpoint,
    registry::{ENDPOINTS, EndpointMethod, EndpointSpec, PathMatcher, Policy},
};

fn representative_path(spec: &EndpointSpec) -> String {
    representative_path_for(spec, spec.route_path, spec.matcher)
}

fn representative_path_for(spec: &EndpointSpec, route_path: &str, matcher: PathMatcher) -> String {
    match matcher {
        PathMatcher::Exact => route_path.to_owned(),
        PathMatcher::SingleSegment { prefix, suffix } => {
            format!("{prefix}route-1{}", suffix.unwrap_or_default())
        }
        PathMatcher::Remainder { prefix } => {
            if matches!(spec.policy, Policy::GeminiAction) {
                format!("{prefix}route-1:generateContent")
            } else {
                format!("{prefix}route-1")
            }
        }
    }
}

fn method(spec: &EndpointSpec) -> Method {
    match spec.method {
        EndpointMethod::Get => Method::GET,
        EndpointMethod::Post => Method::POST,
        EndpointMethod::Delete => Method::DELETE,
    }
}

fn method_name(method: EndpointMethod) -> &'static str {
    match method {
        EndpointMethod::Get => "GET",
        EndpointMethod::Post => "POST",
        EndpointMethod::Delete => "DELETE",
    }
}

#[test]
fn registry_identities_and_routes_are_unique() {
    let mut identities = BTreeSet::new();
    let mut routes = BTreeSet::new();
    for spec in ENDPOINTS {
        assert!(
            identities.insert(spec.id),
            "duplicate identity: {:?}",
            spec.id
        );
        assert!(
            routes.insert((spec.method as u8, spec.route_path)),
            "duplicate route: {:?} {}",
            spec.method,
            spec.route_path
        );
        for alias in spec.aliases {
            assert!(
                routes.insert((spec.method as u8, alias.route_path)),
                "duplicate route: {:?} {}",
                spec.method,
                alias.route_path
            );
        }
    }
}

#[test]
fn openai_aliases_reuse_the_canonical_endpoint_spec() {
    for spec in ENDPOINTS {
        for alias in spec.aliases {
            let canonical_path = representative_path_for(spec, spec.route_path, spec.matcher);
            let alias_path = representative_path_for(spec, alias.route_path, alias.matcher);
            let canonical = InferenceEndpoint::classify(&method(spec), &canonical_path)
                .expect("canonical route is classified");
            let aliased = InferenceEndpoint::classify(&method(spec), &alias_path)
                .expect("alias route is classified");
            assert_eq!(canonical, aliased, "alias differs for {:?}", spec.id);
            assert_eq!(
                canonical.route_from_json(&canonical_path, b"{}"),
                aliased.route_from_json(&alias_path, b"{}"),
                "route extraction differs for {:?}",
                spec.id
            );
            let InferenceEndpoint::Registered {
                spec: canonical_spec,
                ..
            } = canonical
            else {
                panic!("canonical route classified as unknown: {:?}", spec.id);
            };
            let InferenceEndpoint::Registered {
                spec: aliased_spec, ..
            } = aliased
            else {
                panic!("alias classified as unknown: {:?}", spec.id);
            };
            assert!(std::ptr::eq(canonical_spec, aliased_spec));
        }
    }
}

#[test]
fn explicit_openai_v1_alias_catalog_is_complete() {
    let aliases = ENDPOINTS
        .iter()
        .flat_map(|spec| {
            spec.aliases
                .iter()
                .map(move |alias| (method_name(spec.method), alias.route_path))
        })
        .collect::<BTreeSet<_>>();
    assert_eq!(
        aliases,
        BTreeSet::from([
            ("POST", "/v1/chat/completions"),
            ("POST", "/v1/responses"),
            ("POST", "/v1/responses/input_tokens"),
            ("POST", "/v1/embeddings"),
            ("POST", "/v1/moderations"),
            ("POST", "/v1/images/generations"),
            ("POST", "/v1/images/edits"),
            ("POST", "/v1/images/variations"),
            ("POST", "/v1/audio/speech"),
            ("POST", "/v1/audio/transcriptions"),
            ("POST", "/v1/videos"),
            ("GET", "/v1/videos"),
            ("GET", "/v1/videos/{video_id}"),
            ("DELETE", "/v1/videos/{video_id}"),
            ("GET", "/v1/videos/{video_id}/content"),
            ("GET", "/v1/models"),
            ("GET", "/v1/models/{model_id}"),
        ])
    );
}

#[test]
fn canonical_openai_routes_remain_unchanged() {
    let canonical = ENDPOINTS
        .iter()
        .filter(|spec| spec.surface == Surface::OpenAi)
        .map(|spec| (method_name(spec.method), spec.route_path))
        .collect::<BTreeSet<_>>();
    assert_eq!(
        canonical,
        BTreeSet::from([
            ("POST", "/openai/v1/chat/completions"),
            ("POST", "/openai/v1/responses"),
            ("POST", "/openai/v1/responses/input_tokens"),
            ("POST", "/openai/v1/embeddings"),
            ("POST", "/openai/v1/moderations"),
            ("POST", "/openai/v1/images/generations"),
            ("POST", "/openai/v1/images/edits"),
            ("POST", "/openai/v1/images/variations"),
            ("POST", "/openai/v1/audio/speech"),
            ("POST", "/openai/v1/audio/transcriptions"),
            ("POST", "/openai/v1/videos"),
            ("GET", "/openai/v1/videos"),
            ("GET", "/openai/v1/videos/{video_id}"),
            ("DELETE", "/openai/v1/videos/{video_id}"),
            ("GET", "/openai/v1/videos/{video_id}/content"),
            ("GET", "/openai/v1/models"),
            ("GET", "/openai/v1/models/{id}"),
        ])
    );
}

#[test]
fn unknown_openai_v1_paths_are_capability_free() {
    let endpoint = InferenceEndpoint::classify(&Method::POST, "/v1/not-implemented")
        .expect("OpenAI v1 paths are classified for protocol errors");
    assert_eq!(endpoint.surface(), Surface::OpenAi);
    assert_eq!(endpoint.capability(), None);
    assert_eq!(endpoint.metadata(), None);
}

#[test]
fn every_registry_entry_drives_classification_and_policy() {
    for spec in ENDPOINTS {
        let path = representative_path(spec);
        let endpoint = InferenceEndpoint::classify(&method(spec), &path)
            .expect("a registered surface is classified");
        let InferenceEndpoint::Registered {
            spec: classified, ..
        } = endpoint
        else {
            panic!("registered endpoint classified as unknown: {:?}", spec.id);
        };
        assert_eq!(classified.id, spec.id);
        assert_eq!(endpoint.surface(), spec.surface);
        let metadata = endpoint
            .metadata()
            .expect("registered endpoint has routing policy");
        let expected_capability = match metadata.operation {
            OperationKind::ModelList | OperationKind::ModelGet => GatewayCapability::ModelsRead,
            _ => GatewayCapability::Inference,
        };
        assert_eq!(
            endpoint.capability(),
            Some(expected_capability),
            "operation did not resolve the expected capability for {:?}",
            spec.id
        );
    }
}

#[test]
fn every_supported_gemini_action_resolves_metadata_and_capability() {
    for version in ["v1", "v1beta"] {
        for (action, operation) in [
            ("generateContent", OperationKind::Generation),
            ("streamGenerateContent", OperationKind::Generation),
            ("countTokens", OperationKind::TokenCount),
        ] {
            for delimiter in [":", "%3A", "%3a"] {
                let path = format!("/gemini/{version}/models/route-1{delimiter}{action}");
                let endpoint = InferenceEndpoint::classify(&Method::POST, &path).unwrap();
                let metadata = endpoint.metadata().expect("supported action has metadata");
                assert_eq!(metadata.operation, operation);
                assert_eq!(endpoint.capability(), Some(GatewayCapability::Inference));
                assert_eq!(
                    endpoint.route_from_json(&path, b"{}"),
                    Some("route-1".to_owned())
                );
            }
        }
    }
}

#[test]
fn unsupported_gemini_actions_are_explicit_and_metadata_free() {
    let endpoint =
        InferenceEndpoint::classify(&Method::POST, "/gemini/v1/models/route-1:unsupported")
            .unwrap();
    assert_eq!(endpoint.surface(), Surface::Gemini);
    assert_eq!(endpoint.metadata(), None);
    assert_eq!(endpoint.capability(), None);
    assert_eq!(
        endpoint.route_from_json("/gemini/v1/models/route-1:unsupported", b"{}"),
        Some("route-1".to_owned())
    );

    let unknown = InferenceEndpoint::classify(&Method::POST, "/openai/v1/future-action")
        .expect("public gateway paths are classified for admission");
    assert_eq!(unknown.capability(), None);
}
