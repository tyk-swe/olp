use crate::domain::{
    canonical::requests::media_handle_from_inline_marker,
    ports::{MediaSpool, TransportError},
};
use crate::protocols::gemini::dto::{Content, Part};

use crate::providers::transport_common::read_inline_media;

pub(super) async fn hydrate_gemini_contents(
    contents: &mut [Content],
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
    maximum_bytes: usize,
) -> Result<(), TransportError> {
    for content in contents {
        for part in &mut content.parts {
            let Part::InlineData(part) = part else {
                continue;
            };
            if media_handle_from_inline_marker(&part.inline_data.data).is_some() {
                part.inline_data.data =
                    read_inline_media(&part.inline_data.data, spool, maximum_bytes).await?;
            }
        }
    }
    Ok(())
}
