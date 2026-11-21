export async function webauthnCredential(
  publicKey: unknown,
  mode: 'create' | 'get',
  signal: AbortSignal
): Promise<Record<string, unknown>> {
  if (typeof PublicKeyCredential === 'undefined' || !window.isSecureContext)
    throw new Error(
      'Security keys require a supported browser and a secure origin.'
    );
  const result =
    mode === 'create'
      ? await navigator.credentials.create({
          publicKey: PublicKeyCredential.parseCreationOptionsFromJSON(
            publicKey as PublicKeyCredentialCreationOptionsJSON
          ),
          signal
        })
      : await navigator.credentials.get({
          publicKey: PublicKeyCredential.parseRequestOptionsFromJSON(
            publicKey as PublicKeyCredentialRequestOptionsJSON
          ),
          signal
        });
  if (!(result instanceof PublicKeyCredential))
    throw new Error('No security-key response was received.');
  return { ...result.toJSON() };
}
