use std::fmt;

use serde::{Deserialize, Deserializer, Serialize, Serializer, de};
use url::{Position, Url};

const VALIDATION_BASE: &str = "https://return.invalid/";
pub const MAX_RELATIVE_RETURN_TO_BYTES: usize = 2_048;
const LOGIN_LOOP_PATHS: &[&str] = &["/login", "/api/v1/oidc/login", "/api/v1/oidc/callback"];

/// A canonical same-origin absolute-path reference suitable for post-login
/// navigation. It may contain a query and fragment, but never an authority.
#[derive(Clone, Eq, Hash, PartialEq)]
pub struct RelativeReturnTo(String);

impl RelativeReturnTo {
    pub fn parse(value: &str) -> Result<Self, RelativeReturnToError> {
        if value.is_empty()
            || value.len() > MAX_RELATIVE_RETURN_TO_BYTES
            || !value.starts_with('/')
            || value.starts_with("//")
            || value.chars().any(char::is_control)
            || value.contains('\\')
            || !has_valid_percent_encoding(value.as_bytes())
        {
            return Err(RelativeReturnToError);
        }

        let decoded_prefix = percent_decode_prefix(value.as_bytes(), 3);
        if decoded_prefix.starts_with(b"//")
            || decoded_prefix
                .iter()
                .any(|byte| *byte == b'\\' || byte.is_ascii_control())
        {
            return Err(RelativeReturnToError);
        }

        let base = Url::parse(VALIDATION_BASE).expect("the validation base URL is valid");
        let parsed = base.join(value).map_err(|_| RelativeReturnToError)?;
        if parsed.origin() != base.origin()
            || !parsed.username().is_empty()
            || parsed.password().is_some()
        {
            return Err(RelativeReturnToError);
        }

        let decoded_value = percent_decode(value.as_bytes()).ok_or(RelativeReturnToError)?;
        let decoded_value =
            std::str::from_utf8(&decoded_value).map_err(|_| RelativeReturnToError)?;
        if decoded_value
            .chars()
            .any(|character| character == '\\' || character.is_control())
        {
            return Err(RelativeReturnToError);
        }
        let decoded_path = percent_decode(parsed.path().as_bytes()).ok_or(RelativeReturnToError)?;
        let decoded_path = std::str::from_utf8(&decoded_path).map_err(|_| RelativeReturnToError)?;
        let normalized_decoded_path = normalize_decoded_path(decoded_path);
        if normalized_decoded_path.starts_with("//") {
            return Err(RelativeReturnToError);
        }
        if LOGIN_LOOP_PATHS.iter().any(|path| {
            normalized_decoded_path == *path
                || normalized_decoded_path
                    .strip_prefix(path)
                    .is_some_and(|suffix| suffix.starts_with('/'))
        }) {
            return Err(RelativeReturnToError);
        }

        let canonical = parsed[Position::BeforePath..].to_owned();
        if canonical.len() > MAX_RELATIVE_RETURN_TO_BYTES {
            return Err(RelativeReturnToError);
        }
        Ok(Self(canonical))
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }

    #[must_use]
    pub fn into_string(self) -> String {
        self.0
    }
}

impl Default for RelativeReturnTo {
    fn default() -> Self {
        Self("/".to_owned())
    }
}

impl fmt::Debug for RelativeReturnTo {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_tuple("RelativeReturnTo")
            .field(&self.0)
            .finish()
    }
}

impl fmt::Display for RelativeReturnTo {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(self.as_str())
    }
}

impl Serialize for RelativeReturnTo {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        serializer.serialize_str(self.as_str())
    }
}

impl<'de> Deserialize<'de> for RelativeReturnTo {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        let value = String::deserialize(deserializer)?;
        Self::parse(&value).map_err(|_| de::Error::custom("invalid relative return destination"))
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RelativeReturnToError;

impl fmt::Display for RelativeReturnToError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("invalid relative return destination")
    }
}

impl std::error::Error for RelativeReturnToError {}

pub(crate) fn has_valid_percent_encoding(bytes: &[u8]) -> bool {
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%' {
            if index + 2 >= bytes.len()
                || !bytes[index + 1].is_ascii_hexdigit()
                || !bytes[index + 2].is_ascii_hexdigit()
            {
                return false;
            }
            index += 3;
        } else {
            index += 1;
        }
    }
    true
}

fn normalize_decoded_path(path: &str) -> String {
    let mut segments = Vec::new();
    for segment in path.strip_prefix('/').unwrap_or(path).split('/') {
        match segment {
            "." => {}
            ".." => {
                segments.pop();
            }
            _ => segments.push(segment),
        }
    }
    format!("/{}", segments.join("/"))
}

fn percent_decode(bytes: &[u8]) -> Option<Vec<u8>> {
    if !has_valid_percent_encoding(bytes) {
        return None;
    }
    let mut decoded = Vec::with_capacity(bytes.len());
    let mut index = 0;
    while index < bytes.len() {
        if bytes[index] == b'%' {
            let high = hex(bytes[index + 1])?;
            let low = hex(bytes[index + 2])?;
            decoded.push((high << 4) | low);
            index += 3;
        } else {
            decoded.push(bytes[index]);
            index += 1;
        }
    }
    Some(decoded)
}

fn percent_decode_prefix(bytes: &[u8], decoded_bytes: usize) -> Vec<u8> {
    let mut output = Vec::with_capacity(decoded_bytes);
    let mut index = 0;
    while index < bytes.len() && output.len() < decoded_bytes {
        if bytes[index] == b'%'
            && index + 2 < bytes.len()
            && let (Some(high), Some(low)) = (hex(bytes[index + 1]), hex(bytes[index + 2]))
        {
            output.push((high << 4) | low);
            index += 3;
            continue;
        }
        output.push(bytes[index]);
        index += 1;
    }
    output
}

fn hex(value: u8) -> Option<u8> {
    match value {
        b'0'..=b'9' => Some(value - b'0'),
        b'a'..=b'f' => Some(value - b'a' + 10),
        b'A'..=b'F' => Some(value - b'A' + 10),
        _ => None,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn accepts_and_canonicalizes_relative_destinations() {
        for (input, expected) in [
            ("/settings", "/settings"),
            ("/settings?tab=security", "/settings?tab=security"),
            ("/files/a%20b", "/files/a%20b"),
            (
                "/search?q=hello%20world&next=%2Fmodels",
                "/search?q=hello%20world&next=%2Fmodels",
            ),
            (
                "/settings?tab=security#sessions",
                "/settings?tab=security#sessions",
            ),
            ("/a/../settings", "/settings"),
        ] {
            assert_eq!(RelativeReturnTo::parse(input).unwrap().as_str(), expected);
        }
    }

    #[test]
    fn rejects_external_ambiguous_and_malformed_destinations() {
        for input in [
            "https://attacker.example/",
            "//attacker.example/",
            "/\\attacker.example/",
            "/%5c%5cattacker.example/",
            "/safe?next=%5c%5cattacker.example",
            "/safe%0d%0aheader",
            "/%2fattacker.example/",
            "/a/..//attacker.example/",
            "/%2e%2e//attacker.example/",
            "/%2e%2e%2f%2fattacker.example/",
            "/safe%2f..%2flogin",
            "/deep%2f..%2f..%2flogin",
            "/bad%encoding",
            "/control\u{0000}",
            "/encoded%C2%85control",
            "/invalid-utf8-%ff",
            "/login",
            "/login?return_to=%2Fsettings",
            "/a/../login",
            "/%6cogin",
            "/api/v1/oidc/callback",
        ] {
            assert!(RelativeReturnTo::parse(input).is_err(), "{input:?}");
        }
    }

    #[test]
    fn rejects_destinations_that_exceed_the_cookie_safe_bound() {
        let oversized = format!("/{}", "a".repeat(MAX_RELATIVE_RETURN_TO_BYTES));
        assert!(RelativeReturnTo::parse(&oversized).is_err());
    }
}
