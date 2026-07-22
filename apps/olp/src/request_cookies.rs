use axum::http::{HeaderMap, header};

use crate::Problem;

pub(crate) const SESSION_COOKIE: &str = "__Host-olp_session";
pub(crate) const CSRF_COOKIE: &str = "__Host-olp_csrf";
pub(crate) const RECENT_AUTH_COOKIE: &str = "__Host-olp_recent_auth";
pub(crate) const LEGACY_OIDC_FLOW_COOKIE: &str = "__Host-olp_oidc_flow";
pub(crate) const LEGACY_OIDC_LOGIN_FLOW_COOKIE: &str = "__Host-olp_oidc_login_flow";
pub(crate) const OIDC_LOGIN_FLOW_COOKIE_PREFIX: &str = "__Host-olp_oidc_login_";
pub(crate) const OIDC_LINK_FLOW_COOKIE_PREFIX: &str = "__Host-olp_oidc_link_";

#[derive(Clone, Copy, Debug)]
struct RequestCookie<'a> {
    name: &'a str,
    value: &'a str,
}

/// One request-cookie view shared by management and OIDC handlers. Repeated
/// `Cookie` fields are parsed in wire order. Conflicting duplicate values for
/// authentication cookies are rejected instead of selecting one implicitly.
#[derive(Debug)]
pub(crate) struct RequestCookies<'a> {
    cookies: Vec<RequestCookie<'a>>,
}

impl<'a> RequestCookies<'a> {
    pub(crate) fn parse(headers: &'a HeaderMap) -> Result<Self, Problem> {
        let mut cookies = Vec::new();
        for field in headers.get_all(header::COOKIE).iter() {
            let field = field.to_str().map_err(|_| malformed_cookie_header())?;
            for segment in field.split(';') {
                if let Some(cookie) = parse_cookie_pair(segment) {
                    if security_sensitive(cookie.name)
                        && cookies.iter().any(|existing: &RequestCookie<'_>| {
                            existing.name == cookie.name && existing.value != cookie.value
                        })
                    {
                        return Err(Problem::bad_request(
                            "conflicting_cookie_values",
                            "Conflicting values were supplied for an authentication cookie.",
                        ));
                    }
                    if !cookies.iter().any(|existing: &RequestCookie<'_>| {
                        existing.name == cookie.name && existing.value == cookie.value
                    }) {
                        cookies.push(cookie);
                    }
                }
            }
        }
        Ok(Self { cookies })
    }

    #[must_use]
    pub(crate) fn get(&self, expected_name: &str) -> Option<&'a str> {
        self.cookies
            .iter()
            .find_map(|cookie| (cookie.name == expected_name).then_some(cookie.value))
    }

    pub(crate) fn names_with_prefix(
        &'a self,
        prefix: &'a str,
    ) -> impl Iterator<Item = &'a str> + 'a {
        self.cookies
            .iter()
            .filter_map(move |cookie| cookie.name.starts_with(prefix).then_some(cookie.name))
    }
}

fn parse_cookie_pair(segment: &str) -> Option<RequestCookie<'_>> {
    let segment = segment.trim_matches([' ', '\t']);
    let (name, raw_value) = segment.split_once('=')?;
    let name = name.trim_matches([' ', '\t']);
    let raw_value = raw_value.trim_matches([' ', '\t']);
    if !valid_cookie_name(name.as_bytes()) {
        return None;
    }
    let value = if raw_value.len() >= 2 && raw_value.starts_with('"') && raw_value.ends_with('"') {
        &raw_value[1..raw_value.len() - 1]
    } else {
        raw_value
    };
    valid_cookie_value(value.as_bytes()).then_some(RequestCookie { name, value })
}

fn valid_cookie_name(name: &[u8]) -> bool {
    !name.is_empty()
        && name.iter().all(|byte| {
            byte.is_ascii()
                && !byte.is_ascii_control()
                && !matches!(
                    byte,
                    b' ' | b'('
                        | b')'
                        | b'<'
                        | b'>'
                        | b'@'
                        | b','
                        | b';'
                        | b':'
                        | b'\\'
                        | b'"'
                        | b'/'
                        | b'['
                        | b']'
                        | b'?'
                        | b'='
                        | b'{'
                        | b'}'
                )
        })
}

fn valid_cookie_value(value: &[u8]) -> bool {
    value
        .iter()
        .all(|byte| matches!(byte, 0x21 | 0x23..=0x2b | 0x2d..=0x3a | 0x3c..=0x5b | 0x5d..=0x7e))
}

fn security_sensitive(name: &str) -> bool {
    matches!(
        name,
        SESSION_COOKIE
            | CSRF_COOKIE
            | RECENT_AUTH_COOKIE
            | LEGACY_OIDC_FLOW_COOKIE
            | LEGACY_OIDC_LOGIN_FLOW_COOKIE
    ) || name.starts_with(OIDC_LOGIN_FLOW_COOKIE_PREFIX)
        || name.starts_with(OIDC_LINK_FLOW_COOKIE_PREFIX)
}

fn malformed_cookie_header() -> Problem {
    Problem::bad_request(
        "malformed_cookie_header",
        "A Cookie header contains bytes that are not valid HTTP cookie text.",
    )
}

#[cfg(test)]
mod tests {
    use axum::http::HeaderValue;

    use super::*;

    #[test]
    fn parses_one_and_repeated_cookie_fields_identically() {
        let mut one = HeaderMap::new();
        one.insert(
            header::COOKIE,
            HeaderValue::from_static(
                "theme=dark; __Host-olp_session=session; __Host-olp_csrf=csrf",
            ),
        );
        let one = RequestCookies::parse(&one).unwrap();
        assert_eq!(one.get(SESSION_COOKIE), Some("session"));
        assert_eq!(one.get(CSRF_COOKIE), Some("csrf"));

        let mut repeated = HeaderMap::new();
        repeated.append(
            header::COOKIE,
            HeaderValue::from_static("theme=dark; __Host-olp_session=session"),
        );
        repeated.append(
            header::COOKIE,
            HeaderValue::from_static("broken; __Host-olp_csrf=csrf"),
        );
        let repeated = RequestCookies::parse(&repeated).unwrap();
        assert_eq!(repeated.get(SESSION_COOKIE), Some("session"));
        assert_eq!(repeated.get(CSRF_COOKIE), Some("csrf"));
    }

    #[test]
    fn permits_identical_but_rejects_conflicting_security_duplicates() {
        let mut identical = HeaderMap::new();
        identical.append(
            header::COOKIE,
            HeaderValue::from_static("__Host-olp_session=same"),
        );
        identical.append(
            header::COOKIE,
            HeaderValue::from_static("__Host-olp_session=same"),
        );
        assert_eq!(
            RequestCookies::parse(&identical)
                .unwrap()
                .get(SESSION_COOKIE),
            Some("same")
        );

        let mut conflicting = HeaderMap::new();
        conflicting.append(
            header::COOKIE,
            HeaderValue::from_static("__Host-olp_session=first"),
        );
        conflicting.append(
            header::COOKIE,
            HeaderValue::from_static("__Host-olp_session=second"),
        );
        assert_eq!(RequestCookies::parse(&conflicting).unwrap_err().status, 400);
    }

    #[test]
    fn rejects_conflicting_scoped_oidc_cookie_values() {
        let name = "__Host-olp_oidc_login_018f47a5-3b2c-7d1e-8f90-123456789abc";
        let mut headers = HeaderMap::new();
        headers.append(
            header::COOKIE,
            HeaderValue::from_str(&format!("{name}=same")).unwrap(),
        );
        headers.append(
            header::COOKIE,
            HeaderValue::from_str(&format!("{name}=same")).unwrap(),
        );
        assert_eq!(
            RequestCookies::parse(&headers).unwrap().get(name),
            Some("same")
        );

        headers.append(
            header::COOKIE,
            HeaderValue::from_str(&format!("{name}=different")).unwrap(),
        );
        assert_eq!(RequestCookies::parse(&headers).unwrap_err().status, 400);
    }

    #[test]
    fn rejects_conflicting_recent_authentication_cookie_values() {
        let mut headers = HeaderMap::new();
        headers.append(
            header::COOKIE,
            HeaderValue::from_static("__Host-olp_recent_auth=first"),
        );
        headers.append(
            header::COOKIE,
            HeaderValue::from_static("__Host-olp_recent_auth=second"),
        );
        assert_eq!(RequestCookies::parse(&headers).unwrap_err().status, 400);
    }

    #[test]
    fn malformed_pairs_do_not_hide_adjacent_security_cookies() {
        let mut headers = HeaderMap::new();
        headers.insert(
            header::COOKIE,
            HeaderValue::from_static(
                "bad pair; invalid=contains,comma; __Host-olp_session=secret; another",
            ),
        );
        assert_eq!(
            RequestCookies::parse(&headers).unwrap().get(SESSION_COOKIE),
            Some("secret")
        );
    }
}
