import type { ExternalCredentialReference } from './credentials';

const component = /^[A-Za-z0-9_-]+$/;
const versionId = /^[A-Za-z0-9-]{32,64}$/;
const numericVersion = /^[1-9][0-9]{0,18}$/;
const awsRegion = /^[a-z]{2}(?:-[a-z]+)+-[0-9]+$/;

function clean(value: string | undefined, max: number): boolean {
  return (
    !!value &&
    new TextEncoder().encode(value).length <= max &&
    !/[\0\r\n\t]/.test(value) &&
    value.trim() === value
  );
}

function parseUrl(value: string): URL | null {
  try {
    return new URL(value);
  } catch {
    return null;
  }
}

function plainUrl(url: URL): boolean {
  return !url.username && !url.password && !url.search && !url.hash;
}

function isAzureSecret(secretId: string): boolean {
  const url = parseUrl(secretId);
  if (!url || url.protocol !== 'https:' || url.port || !plainUrl(url))
    return false;
  const [kind, name, ...rest] = url.pathname.slice(1).split('/');
  return (
    url.hostname.endsWith('.vault.azure.net') &&
    kind === 'secrets' &&
    component.test(name ?? '') &&
    rest.length === 0
  );
}

function isVaultSecret(secretId: string): boolean {
  const url = parseUrl(secretId);
  if (
    !url ||
    !url.host ||
    !['https:', 'http:'].includes(url.protocol) ||
    !plainUrl(url) ||
    !url.pathname.startsWith('/v1/')
  )
    return false;
  return url.pathname
    .slice('/v1/'.length)
    .split('/')
    .every((part) => component.test(part));
}

function isGcpSecret(secretId: string): boolean {
  const [projects, project, secrets, secret, ...rest] = secretId.split('/');
  return (
    projects === 'projects' &&
    secrets === 'secrets' &&
    component.test(project ?? '') &&
    component.test(secret ?? '') &&
    rest.length === 0
  );
}

function storeIssue(reference: ExternalCredentialReference): string | null {
  const { store, secret_id, version, region, field } = reference;
  if (!clean(secret_id, 2048) || !clean(version, 128))
    return 'Choose a supported store, secret ID and immutable version.';
  switch (store) {
    case 'aws':
      return awsRegion.test(region ?? '') && versionId.test(version) && !field
        ? null
        : 'AWS references require a region and immutable version ID.';
    case 'gcp':
      return isGcpSecret(secret_id) &&
        numericVersion.test(version) &&
        !region &&
        !field
        ? null
        : 'Google references require a project secret and numeric version.';
    case 'azure':
      return isAzureSecret(secret_id) &&
        component.test(version) &&
        version.length === 32 &&
        !region &&
        !field
        ? null
        : 'Azure references require a versioned vault secret.';
    case 'vault':
      return isVaultSecret(secret_id) &&
        numericVersion.test(version) &&
        clean(field, 128) &&
        !region
        ? null
        : 'Vault KV v2 references require a version and field.';
  }
}

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
  const issue = storeIssue(reference as ExternalCredentialReference);
  if (issue) throw new Error(issue);
  return reference as ExternalCredentialReference;
}
