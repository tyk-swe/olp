use std::{fmt::Write as _, path::Path};

use axum::{
    Router,
    body::Body,
    extract::Request,
    http::{HeaderValue, StatusCode},
    response::IntoResponse as _,
};
use base64::Engine as _;
use sha2::{Digest as _, Sha256};
use tower::ServiceExt as _;
use tower_http::services::{ServeDir, ServeFile};

const CSP_PREFIX: &str = "default-src 'self'; base-uri 'none'; connect-src 'self'; font-src 'self'; form-action 'self'; frame-ancestors 'none'; img-src 'self' data:; object-src 'none'; script-src 'self'";
// SvelteKit creates its route announcer from a client-side template. Keep the
// framework's exact visually-hidden style working without admitting arbitrary
// style attributes or weakening the external stylesheet policy.
const SVELTEKIT_ANNOUNCER_STYLE: &str = "position: absolute; left: 0; top: 0; clip: rect(0 0 0 0); clip-path: inset(50%); overflow: hidden; white-space: nowrap; width: 1px; height: 1px";

/// Builds a strict CSP that admits only the exact inline bootstrap scripts in
/// the generated console entry point. SvelteKit cannot externalize this
/// bootstrap, and its content changes with each asset build, so a static hash
/// would break every new console release.
pub(crate) fn content_security_policy(console_dir: &Path) -> HeaderValue {
    let mut policy = String::from(CSP_PREFIX);
    if let Ok(index) = std::fs::read_to_string(console_dir.join("index.html")) {
        let mut remainder = index.as_str();
        while let Some(script_start) = remainder.find("<script") {
            remainder = &remainder[script_start + "<script".len()..];
            let Some(tag_end) = remainder.find('>') else {
                break;
            };
            remainder = &remainder[tag_end + 1..];
            let Some(script_end) = remainder.find("</script>") else {
                break;
            };
            let script = &remainder[..script_end];
            let digest = base64::engine::general_purpose::STANDARD.encode(Sha256::digest(script));
            write!(policy, " 'sha256-{digest}'").expect("writing to a String cannot fail");
            remainder = &remainder[script_end + "</script>".len()..];
        }
    }
    let announcer_digest =
        base64::engine::general_purpose::STANDARD.encode(Sha256::digest(SVELTEKIT_ANNOUNCER_STYLE));
    write!(
        policy,
        "; style-src 'self'; style-src-attr 'unsafe-hashes' 'sha256-{announcer_digest}'"
    )
    .expect("writing to a String cannot fail");
    HeaderValue::from_str(&policy).expect("generated console CSP must be a valid header")
}

pub(crate) fn spa_service(console_dir: &Path) -> ServeDir<Router> {
    let index = console_dir.join("index.html");
    let fallback = Router::new().fallback(move |request: Request| {
        let index = index.clone();
        async move {
            if is_asset_like(request.uri().path()) {
                return StatusCode::NOT_FOUND.into_response();
            }
            ServeFile::new(index)
                .oneshot(request)
                .await
                .map(|response| response.map(Body::new))
                .unwrap_or_else(|never| match never {})
        }
    });
    ServeDir::new(console_dir)
        .precompressed_br()
        .precompressed_gzip()
        .append_index_html_on_directories(true)
        .fallback(fallback)
}

fn is_asset_like(path: &str) -> bool {
    path.rsplit('/')
        .next()
        .is_some_and(|name| Path::new(name).extension().is_some())
}

#[cfg(test)]
mod tests {
    use axum::{body::Body, http::Request};
    use base64::Engine as _;
    use sha2::{Digest as _, Sha256};
    use tower::ServiceExt as _;
    use uuid::Uuid;

    use super::*;

    #[tokio::test]
    async fn deep_link_serves_spa_with_success_status() {
        let root = std::env::temp_dir().join(format!("olp-console-test-{}", Uuid::now_v7()));
        std::fs::create_dir(&root).unwrap();
        std::fs::write(root.join("index.html"), "<!doctype html><title>OLP</title>").unwrap();

        let response = spa_service(&root)
            .oneshot(
                Request::get("/providers/example")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::OK);

        std::fs::remove_dir_all(root).unwrap();
    }

    #[tokio::test]
    async fn missing_asset_is_not_spa_html() {
        let root = std::env::temp_dir().join(format!("olp-console-test-{}", Uuid::now_v7()));
        std::fs::create_dir(&root).unwrap();
        std::fs::write(root.join("index.html"), "<!doctype html><title>OLP</title>").unwrap();

        let response = spa_service(&root)
            .oneshot(
                Request::get("/_app/missing.js")
                    .body(Body::empty())
                    .unwrap(),
            )
            .await
            .unwrap();
        assert_eq!(response.status(), axum::http::StatusCode::NOT_FOUND);

        std::fs::remove_dir_all(root).unwrap();
    }

    #[test]
    fn csp_hashes_each_generated_inline_bootstrap() {
        let root = std::env::temp_dir().join(format!("olp-console-csp-test-{}", Uuid::now_v7()));
        std::fs::create_dir(&root).unwrap();
        let first = "window.first = true;";
        let second = "window.second = true;";
        std::fs::write(
            root.join("index.html"),
            format!(
                "<!doctype html><script>{first}</script><script type=\"module\">{second}</script>"
            ),
        )
        .unwrap();

        let policy = content_security_policy(&root).to_str().unwrap().to_owned();
        for script in [first, second] {
            let digest = base64::engine::general_purpose::STANDARD.encode(Sha256::digest(script));
            assert!(policy.contains(&format!("'sha256-{digest}'")));
        }
        assert!(policy.contains("script-src 'self'"));
        assert!(policy.contains("form-action 'self'"));
        assert!(policy.contains("; style-src 'self';"));
        assert!(policy.ends_with(
            "style-src-attr 'unsafe-hashes' 'sha256-S8qMpvofolR8Mpjy4kQvEm7m1q8clzU4dfDH0AmvZjo='"
        ));
        assert!(!policy.contains("'unsafe-inline'"));

        std::fs::remove_dir_all(root).unwrap();
    }
}
