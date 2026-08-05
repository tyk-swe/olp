use std::fmt;

use serde::{Deserialize, Serialize, Serializer};
use zeroize::Zeroize;

/// A request-only secret whose debug output is always redacted and whose
/// backing buffer is cleared on drop.
#[derive(Deserialize)]
pub(crate) struct WriteOnlySecret(pub(super) String);

impl Serialize for WriteOnlySecret {
    fn serialize<S>(&self, serializer: S) -> Result<S::Ok, S::Error>
    where
        S: Serializer,
    {
        serializer.serialize_str(&self.0)
    }
}

impl WriteOnlySecret {
    pub(crate) fn new(value: String) -> Self {
        Self(value)
    }

    pub(crate) fn expose(&self) -> &str {
        &self.0
    }
}

impl Drop for WriteOnlySecret {
    fn drop(&mut self) {
        self.0.zeroize();
    }
}

impl fmt::Debug for WriteOnlySecret {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("WriteOnlySecret([REDACTED])")
    }
}
