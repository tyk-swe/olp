use olp_domain::{MediaSpool, TransportError, media_handle_from_inline_marker};
use olp_protocols::gemini::{Content, Part};

use crate::transport_common::read_inline_media;

pub(super) async fn hydrate_gemini_contents(
    contents: &mut [Content],
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
) -> Result<(), TransportError> {
    for content in contents {
        for part in &mut content.parts {
            let Part::InlineData(part) = part else {
                continue;
            };
            if media_handle_from_inline_marker(&part.inline_data.data).is_some() {
                part.inline_data.data = read_inline_media(&part.inline_data.data, spool).await?;
            }
        }
    }
    Ok(())
}
