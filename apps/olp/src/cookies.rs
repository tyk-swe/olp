//! Minimal request-cookie parsing shared by management and OIDC handlers.
//!
//! Cookie values are intentionally returned without decoding. Session and OIDC
//! tokens use URL-safe alphabets and are validated by their owning subsystem.

use axum::http::{HeaderMap, header};

pub(crate) fn find<'a>(headers: &'a HeaderMap, expected_name: &str) -> Option<&'a str> {
    let expected_name = expected_name.as_bytes();
    for field in headers.get_all(header::COOKIE).iter() {
        for pair in field.as_bytes().split(|byte| *byte == b';') {
            let pair = trim_ascii_whitespace(pair);
            let Some(separator) = pair.iter().position(|byte| *byte == b'=') else {
                continue;
            };
            if &pair[..separator] != expected_name {
                continue;
            }
            if let Ok(value) = std::str::from_utf8(&pair[separator + 1..]) {
                return Some(value);
            }
        }
    }
    None
}

fn trim_ascii_whitespace(mut value: &[u8]) -> &[u8] {
    while value.first().is_some_and(u8::is_ascii_whitespace) {
        value = &value[1..];
    }
    while value.last().is_some_and(u8::is_ascii_whitespace) {
        value = &value[..value.len() - 1];
    }
    value
}

#[cfg(test)]
mod tests {
    use axum::http::HeaderValue;

    use super::*;

    #[test]
    fn searches_all_cookie_fields_and_matches_names_exactly() {
        let mut headers = HeaderMap::new();
        headers.append(
            header::COOKIE,
            HeaderValue::from_static("__Host-olp_session_shadow=wrong; theme=dark"),
        );
        headers.append(
            header::COOKIE,
            HeaderValue::from_static("other=1; __Host-olp_session=secret=="),
        );

        assert_eq!(find(&headers, "__Host-olp_session"), Some("secret=="));
        assert_eq!(find(&headers, "missing"), None);
    }

    #[test]
    fn malformed_pairs_do_not_hide_valid_cookie_values() {
        let mut headers = HeaderMap::new();
        headers.append(
            header::COOKIE,
            HeaderValue::from_bytes(b"malformed; session=value; bad=\x80")
                .expect("obs-text is a legal header value"),
        );

        assert_eq!(find(&headers, "session"), Some("value"));
        assert_eq!(find(&headers, "bad"), None);
    }
}
