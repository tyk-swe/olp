use crate::inference::transport::MediaSpool;
use crate::inference::transport::TransportError;
use crate::protocols::canonical::requests::media_handle_from_inline_marker;
use crate::protocols::gemini::dto::Content;
use crate::protocols::gemini::dto::Part;

use crate::providers::transport_common::read_inline_media;

pub(crate) async fn hydrate_gemini_contents(
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
