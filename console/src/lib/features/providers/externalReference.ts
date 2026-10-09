import type { ExternalCredentialReference } from './api/credentials';

export function parseExternalReference(
  value: string
): ExternalCredentialReference {
  let reference: unknown;
  try {
    reference = JSON.parse(value);
  } catch {
    throw new Error(
      'Enter a JSON object with store, secret_id and immutable version.'
    );
  }
  if (!reference || typeof reference !== 'object' || Array.isArray(reference))
    throw new Error('Enter an external credential reference object.');
  const fields = reference as Record<string, unknown>;
  if (
    !['aws', 'gcp', 'azure', 'vault'].includes(String(fields.store)) ||
    typeof fields.secret_id !== 'string' ||
    !fields.secret_id.trim() ||
    typeof fields.version !== 'string' ||
    !fields.version.trim()
  )
    throw new Error(
      'Choose a supported store, secret ID and immutable version.'
    );
  if (
    Object.keys(fields).some(
      (key) =>
        !['store', 'secret_id', 'version', 'region', 'field'].includes(key)
    )
  )
    throw new Error(
      'References accept store, secret_id, version, region and field only.'
    );

  if (
    ['region', 'field'].some(
      (key) => fields[key] !== undefined && typeof fields[key] !== 'string'
    )
  )
    throw new Error('Optional region and field must be strings.');
  return reference as ExternalCredentialReference;
}
