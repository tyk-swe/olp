//! Axum's `Router` cannot be enumerated at runtime and the management routes
//! are mounted as chained handler calls rather than a table, so the mounted
//! set is recovered by parsing the router sources. The parser is strict: it
//! panics on any `.route(...)` shape it does not understand and the test
//! asserts a lower bound on what it recovered, because a parser that quietly
//! matches nothing would pass forever.

use std::collections::{BTreeMap, BTreeSet};

use olp::management::openapi::document;

const ROUTER_SOURCES: [&str; 5] = [
    include_str!("../../src/management/mod.rs"),
    include_str!("../../src/management/configuration.rs"),
    include_str!("../../src/management/oidc.rs"),
    include_str!("../../src/management/operations.rs"),
    include_str!("../../src/management/playground.rs"),
];
const PUBLIC_AUTH_ROUTES_SOURCE: &str = include_str!("../../src/public_http/public_auth_routes.rs");
const HTTP_METHODS: [&str; 5] = ["get", "post", "put", "patch", "delete"];

#[test]
fn every_mounted_management_route_is_documented_in_the_openapi_contract() {
    let mounted = mounted_routes();
    assert!(
        mounted.len() >= 90,
        "the router parser recovered only {} routes, so it has stopped \
         matching how the management router mounts them",
        mounted.len()
    );

    let document = document();
    let documented: BTreeSet<(String, String)> = document["paths"]
        .as_object()
        .expect("generated OpenAPI has paths")
        .iter()
        .flat_map(|(path, item)| {
            item.as_object()
                .expect("an OpenAPI path item is an object")
                .keys()
                .map(|method| (method.clone(), path.clone()))
        })
        .collect();

    let undocumented: Vec<String> = mounted
        .difference(&documented)
        .map(|(method, path)| format!("{} {path}", method.to_uppercase()))
        .collect();
    assert!(
        undocumented.is_empty(),
        "mounted management routes missing from the OpenAPI document: {}",
        undocumented.join(", ")
    );
}

fn mounted_routes() -> BTreeSet<(String, String)> {
    let public_auth_paths = public_auth_route_paths();
    let mut routes = BTreeSet::new();
    for source in ROUTER_SOURCES {
        for arguments in route_call_arguments(source) {
            let (path_expression, handlers) = split_arguments(arguments);
            let path = resolve_path(path_expression, &public_auth_paths);
            for method in HTTP_METHODS {
                if calls(handlers, method) {
                    routes.insert((method.to_owned(), path.clone()));
                }
            }
        }
    }
    routes
}

fn route_call_arguments(source: &str) -> Vec<&str> {
    let mut arguments = Vec::new();
    let mut cursor = 0;
    while let Some(offset) = source[cursor..].find(".route(") {
        let start = cursor + offset + ".route(".len();
        let end = closing_parenthesis(source, start);
        arguments.push(&source[start..end]);
        cursor = end;
    }
    arguments
}

fn closing_parenthesis(source: &str, start: usize) -> usize {
    let mut depth = 1_usize;
    let mut in_string = false;
    let mut escaped = false;
    for (offset, character) in source[start..].char_indices() {
        if in_string {
            match character {
                _ if escaped => escaped = false,
                '\\' => escaped = true,
                '"' => in_string = false,
                _ => {}
            }
            continue;
        }
        match character {
            '"' => in_string = true,
            '(' => depth += 1,
            ')' => {
                depth -= 1;
                if depth == 0 {
                    return start + offset;
                }
            }
            _ => {}
        }
    }
    panic!("unbalanced `.route(` call in the management router sources");
}

fn split_arguments(arguments: &str) -> (&str, &str) {
    let mut depth = 0_usize;
    let mut in_string = false;
    let mut escaped = false;
    for (offset, character) in arguments.char_indices() {
        if in_string {
            match character {
                _ if escaped => escaped = false,
                '\\' => escaped = true,
                '"' => in_string = false,
                _ => {}
            }
            continue;
        }
        match character {
            '"' => in_string = true,
            '(' | '[' => depth += 1,
            ')' | ']' => depth -= 1,
            ',' if depth == 0 => {
                return (arguments[..offset].trim(), arguments[offset + 1..].trim());
            }
            _ => {}
        }
    }
    panic!("`.route({arguments})` does not separate a path from its handlers");
}

fn resolve_path(expression: &str, public_auth_paths: &BTreeMap<&str, &str>) -> String {
    if let Some(literal) = expression
        .strip_prefix('"')
        .and_then(|rest| rest.strip_suffix('"'))
    {
        return literal.to_owned();
    }
    let variant = expression
        .strip_prefix("PublicAuthRoute::")
        .and_then(|rest| rest.strip_suffix(".path()"))
        .unwrap_or_else(|| panic!("unrecognised route path expression `{expression}`"));
    (*public_auth_paths
        .get(variant)
        .unwrap_or_else(|| panic!("`PublicAuthRoute::{variant}` declares no path")))
    .to_owned()
}

fn calls(handlers: &str, method: &str) -> bool {
    handlers.match_indices(method).any(|(offset, _)| {
        handlers[offset + method.len()..].starts_with('(')
            && handlers[..offset]
                .chars()
                .next_back()
                .is_none_or(|character| !character.is_alphanumeric() && character != '_')
    })
}

fn public_auth_route_paths() -> BTreeMap<&'static str, &'static str> {
    let mut paths = BTreeMap::new();
    for line in PUBLIC_AUTH_ROUTES_SOURCE.lines() {
        let Some((variants, remainder)) = line.split_once("=> \"") else {
            continue;
        };
        let Some((path, _)) = remainder.split_once('"') else {
            continue;
        };
        for variant in variants.split('|') {
            if let Some(name) = variant.trim().strip_prefix("Self::") {
                paths.insert(name, path);
            }
        }
    }
    assert!(
        !paths.is_empty(),
        "no `PublicAuthRoute` paths were recovered from the source"
    );
    paths
}
