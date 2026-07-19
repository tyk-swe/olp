use std::{fmt::Write as _, path::Path};

use axum::http::HeaderValue;
use base64::Engine as _;
use sha2::{Digest as _, Sha256};
use tower_http::services::{ServeDir, ServeFile};

const CSP_PREFIX: &str = "default-src 'self'; base-uri 'none'; connect-src 'self'; font-src 'self'; frame-ancestors 'none'; img-src 'self' data:; object-src 'none'; script-src 'self'";
const CSP_SUFFIX: &str = "; style-src 'self'";

/// Builds a strict CSP that admits only the exact inline bootstrap scripts in
/// the generated console entry point. SvelteKit cannot externalize this
/// bootstrap, and its content changes with each asset build, so a static hash
/// would break every new console release.
pub fn content_security_policy(console_dir: &Path) -> HeaderValue {
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
    policy.push_str(CSP_SUFFIX);
    HeaderValue::from_str(&policy).expect("generated console CSP must be a valid header")
}

pub fn service(console_dir: &Path) -> ServeDir<ServeFile> {
    ServeDir::new(console_dir)
        .precompressed_br()
        .precompressed_gzip()
        .append_index_html_on_directories(true)
        // SPA routes are real console entry points. `not_found_service` forces
        // a 404 even when it serves this file, which breaks direct deep links.
        .fallback(ServeFile::new(console_dir.join("index.html")))
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

        let response = service(&root)
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
        assert!(policy.ends_with("style-src 'self'"));

        std::fs::remove_dir_all(root).unwrap();
    }
}
