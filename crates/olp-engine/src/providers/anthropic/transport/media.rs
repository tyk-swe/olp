use crate::domain::{
    canonical::requests::media_handle_from_inline_marker,
    ports::{MediaSpool, TransportError},
};
use crate::protocols::anthropic::dto::{ContentBlock, Message, MessageContent};

use crate::providers::transport_common::{protocol_error, read_inline_media};

pub(super) async fn hydrate_anthropic_messages(
    messages: &mut [Message],
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
    maximum_bytes: usize,
) -> Result<(), TransportError> {
    for message in messages {
        let MessageContent::Blocks(blocks) = &mut message.content else {
            continue;
        };
        for block in blocks {
            hydrate_anthropic_block(block, spool, maximum_bytes).await?;
        }
    }
    Ok(())
}

async fn hydrate_anthropic_block(
    block: &mut ContentBlock,
    spool: Option<&std::sync::Arc<dyn MediaSpool>>,
    maximum_bytes: usize,
) -> Result<(), TransportError> {
    match block {
        ContentBlock::Image(image) if image.source.kind == "base64" => {
            let Some(marker) = image.source.data.as_deref() else {
                return Err(protocol_error("Anthropic base64 image omitted data"));
            };
            if media_handle_from_inline_marker(marker).is_some() {
                image.source.data = Some(read_inline_media(marker, spool, maximum_bytes).await?);
            }
        }
        ContentBlock::ToolResult(result) => {
            if let Some(crate::protocols::anthropic::dto::ToolResultContent::Blocks(blocks)) =
                &mut result.content
            {
                for block in blocks {
                    Box::pin(hydrate_anthropic_block(block, spool, maximum_bytes)).await?;
                }
            }
        }
        _ => {}
    }
    Ok(())
}
