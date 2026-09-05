pub const MAX_HTTP_HEADER_COUNT: usize = 100;
pub const MAX_HTTP_HEADER_BYTES: usize = 32 * 1024;

/// Request body caps enforced at the public boundary. Header caps stay
/// constant because they are tied to the Hyper parser configuration.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct BodyLimits {
    pub json_body_bytes: usize,
    pub media_body_bytes: usize,
    pub inline_media_items: usize,
    pub inline_media_item_bytes: usize,
    pub inline_media_total_bytes: usize,
}

impl Default for BodyLimits {
    fn default() -> Self {
        Self {
            json_body_bytes: 2 * 1024 * 1024,
            media_body_bytes: 64 * 1024 * 1024,
            inline_media_items: 4,
            inline_media_item_bytes: 1024 * 1024,
            inline_media_total_bytes: 2 * 1024 * 1024,
        }
    }
}

impl BodyLimits {
    /// Multipart admission budgets half the spool, so a media cap above that
    /// would make every multipart request fail with 503.
    pub fn validate(self, spool_capacity_bytes: u64) -> Result<Self, String> {
        if self.inline_media_item_bytes > self.inline_media_total_bytes {
            return Err(
                "OLP_HTTP_MAX_INLINE_MEDIA_ITEM_BYTES must not exceed OLP_HTTP_MAX_INLINE_MEDIA_TOTAL_BYTES"
                    .to_owned(),
            );
        }
        if self.inline_media_total_bytes > self.json_body_bytes {
            return Err(
                "OLP_HTTP_MAX_INLINE_MEDIA_TOTAL_BYTES must not exceed OLP_HTTP_MAX_JSON_BODY_BYTES"
                    .to_owned(),
            );
        }
        if self.media_body_bytes as u64 > spool_capacity_bytes / 2 {
            return Err(format!(
                "OLP_HTTP_MAX_MEDIA_BODY_BYTES must not exceed half of OLP_MEDIA_SPOOL_CAPACITY_BYTES ({})",
                spool_capacity_bytes / 2
            ));
        }
        Ok(self)
    }
}
