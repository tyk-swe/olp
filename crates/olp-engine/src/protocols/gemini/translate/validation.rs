use super::super::dto::CountTokensRequest;
use super::errors::CountTokensError;

pub fn validate_count_tokens_request(request: &CountTokensRequest) -> Result<(), CountTokensError> {
    let has_contents = !request.contents.is_empty();
    let has_generate_request = request.generate_content_request.is_some();
    if has_contents == has_generate_request {
        return Err(CountTokensError::ExactlyOneInput);
    }
    Ok(())
}
