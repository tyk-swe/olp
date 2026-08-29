use axum::{
    Router,
    extract::DefaultBodyLimit,
    routing::{MethodRouter, on},
};

use crate::bootstrap::mode_dependencies::GatewayState;
use crate::bootstrap::state::BodyLimits;

use super::super::{anthropic, chat, gemini, media, openai_models, responses, videos};
use super::registry::{BodyAdmission, ENDPOINTS, EndpointSpec, Handler};

pub(in crate::gateway) fn router(limits: BodyLimits) -> Router<GatewayState> {
    ENDPOINTS
        .iter()
        .fold(Router::new(), |router, spec| register(router, spec, limits))
}

fn register(
    router: Router<GatewayState>,
    spec: &'static EndpointSpec,
    limits: BodyLimits,
) -> Router<GatewayState> {
    let filter = spec.method.filter();
    let method_router: MethodRouter<GatewayState> = match spec.handler {
        Handler::OpenAiChatCompletions => on(filter, chat::chat_completions),
        Handler::OpenAiResponses => on(filter, responses::responses),
        Handler::OpenAiResponseInputTokens => on(filter, responses::response_input_tokens),
        Handler::OpenAiEmbeddings => on(filter, media::embeddings),
        Handler::OpenAiModerations => on(filter, media::moderations),
        Handler::OpenAiImageGenerations => on(filter, media::image_generations),
        Handler::OpenAiImageEdits => on(filter, media::image_edits),
        Handler::OpenAiImageVariations => on(filter, media::image_variations),
        Handler::OpenAiSpeech => on(filter, media::speech),
        Handler::OpenAiTranscriptions => on(filter, media::transcriptions),
        Handler::OpenAiVideoCreate => on(filter, videos::video_create),
        Handler::OpenAiVideoList => on(filter, videos::video_list),
        Handler::OpenAiVideoGet => on(filter, videos::video_get),
        Handler::OpenAiVideoDelete => on(filter, videos::video_delete),
        Handler::OpenAiVideoContent => on(filter, videos::video_content),
        Handler::OpenAiModelList => on(filter, openai_models::list_models),
        Handler::OpenAiModelGet => on(filter, openai_models::get_model),
        Handler::AnthropicMessages => on(filter, anthropic::messages),
        Handler::AnthropicCountTokens => on(filter, anthropic::count_tokens),
        Handler::AnthropicModelList => on(filter, anthropic::models),
        Handler::AnthropicModelGet => on(filter, anthropic::model),
        Handler::GeminiModelList => on(filter, gemini::models),
        Handler::GeminiModelGet => on(filter, gemini::model),
        Handler::GeminiModelAction => on(filter, gemini::action),
    };
    let method_router = match spec.body_admission {
        BodyAdmission::Multipart { share } => method_router.layer(DefaultBodyLimit::max(
            usize::try_from(share.reservation_bytes(limits.media_body_bytes))
                .expect("multipart reservation fits usize"),
        )),
        BodyAdmission::Standard | BodyAdmission::Media => method_router,
    };
    let router = router.route(spec.route_path, method_router.clone());
    spec.aliases.iter().fold(router, |router, alias| {
        router.route(alias.route_path, method_router.clone())
    })
}
