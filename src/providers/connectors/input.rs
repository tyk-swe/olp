use crate::providers::configuration::ProviderConfiguration;
use crate::providers::connectors::configuration::Credential;
use crate::providers::connectors::configuration::CredentialKind;
use crate::providers::connectors::configuration::Error;
use zeroize::Zeroizing;

pub fn provider_credential(
    config: &ProviderConfiguration,
    plaintext: Option<&[u8]>,
) -> Result<Credential, Error> {
    match (
        crate::providers::connectors::configuration::credential_kind(config)?,
        plaintext,
    ) {
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
