use super::{
    assembly::Factory,
    configuration::{Config, Credential, CredentialKind, Error},
};
use crate::domain::{
    provider_configuration::{Configuration, validate},
    routing::provider::ProviderKind,
};
use zeroize::Zeroizing;
pub fn provider_config(fields: Configuration<'_>) -> Result<Config, Error> {
    if let Some(violation) = validate(fields).into_iter().next() {
        return Err(Error::Configuration(violation.detail.to_owned()));
    }

    let required = |value: Option<&str>, message: &'static str| {
        value
            .map(str::to_owned)
            .ok_or_else(|| Error::Configuration(message.to_owned()))
    };

    Ok(match fields.kind {
        ProviderKind::OpenAi => Config::OpenAi {
            endpoint: fields.endpoint.map(str::to_owned),
        },
        ProviderKind::OpenAiCompatible => Config::OpenAiCompatible {
            endpoint: required(fields.endpoint, "OpenAI-compatible endpoint is missing")?,
        },
        ProviderKind::Anthropic => Config::Anthropic {
            endpoint: fields.endpoint.map(str::to_owned),
            api_version: fields.api_version.map(str::to_owned),
        },
        ProviderKind::Gemini => Config::Gemini {
            endpoint: fields.endpoint.map(str::to_owned),
        },
        ProviderKind::VertexAi => Config::VertexAi {
            project: required(fields.cloud_project, "Vertex AI project is missing")?,
            location: required(fields.cloud_region, "Vertex AI location is missing")?,
            probe_model: required(fields.model, "Vertex AI probe model is missing")?,
            auth_mode: fields.auth_mode,
        },
        ProviderKind::Bedrock => Config::Bedrock {
            region: required(fields.cloud_region, "Bedrock AWS region is missing")?,
            auth_mode: fields.auth_mode,
        },
        ProviderKind::AzureOpenAi => Config::AzureOpenAi {
            endpoint: required(fields.endpoint, "Azure OpenAI endpoint is missing")?,
            deployment: required(fields.deployment, "Azure OpenAI deployment is missing")?,
            api_version: required(fields.api_version, "Azure OpenAI API version is missing")?,
        },
    })
}

pub fn provider_credential(config: &Config, plaintext: Option<&[u8]>) -> Result<Credential, Error> {
    match (Factory::credential_kind(config)?, plaintext) {
        (CredentialKind::None, _) | (_, None) => Ok(Credential::None),
        (CredentialKind::ApiKey, Some(plaintext)) => Ok(Credential::ApiKey(Zeroizing::new(
            secret_text(plaintext)?.to_owned(),
        ))),
        (CredentialKind::ServiceAccountJson, Some(plaintext)) => Ok(
            Credential::ServiceAccountJson(Zeroizing::new(secret_text(plaintext)?.to_owned())),
        ),
        (CredentialKind::AwsStatic, Some(plaintext)) => {
            Ok(Credential::AwsStatic(Zeroizing::new(plaintext.to_vec())))
        }
    }
}

fn secret_text(secret: &[u8]) -> Result<&str, Error> {
    std::str::from_utf8(secret)
        .map_err(|_| Error::Credential("provider credential is not valid UTF-8".to_owned()))
}
