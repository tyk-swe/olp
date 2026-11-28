import {
  validated,
  type FieldErrors,
  type Validation
} from '#lib/editing/validation.js';
import { captureResult } from '#lib/editing/result.js';
import type {
  ProviderKindCapability,
  ProviderVendor
} from '#lib/domain/providers/api.js';
import { parseExternalReference } from '../externalReference';
import {
  buildCreateProviderInput,
  type ProviderDraft
} from '../providerEditor';
import {
  requiresCredential,
  requiresProbeModel,
  showsEndpoint
} from '../providerKindSpec';
import type { ProviderProfile } from '../providerProfiles';
import type { CreateProviderInput } from '../providers';

export type ConnectionField =
  | 'name'
  | 'authMode'
  | 'profileId'
  | 'endpoint'
  | 'apiVersion'
  | 'cloudRegion'
  | 'cloudProject'
  | 'deployment'
  | 'model'
  | 'credentialHeaders'
  | 'credential'
  | 'externalCredentialReference'
  | 'configuration';

type Context = {
  credentialAlreadyStored: boolean;
  profiles?: readonly ProviderProfile[];
  vendors?: readonly ProviderVendor[];
};

const kindFields = {
  endpoint: { field: 'endpoint', missing: 'Enter the endpoint.' },
  api_version: { field: 'apiVersion', missing: 'Enter the API version.' },
  cloud_region: { field: 'cloudRegion', missing: 'Enter the cloud region.' },
  cloud_project: { field: 'cloudProject', missing: 'Enter the cloud project.' },
  deployment: { field: 'deployment', missing: 'Enter the deployment.' },
  model: {
    field: 'model',
    missing: 'Enter the model the connection test uses.'
  }
} as const;

function isKindField(field: string): field is keyof typeof kindFields {
  return Object.hasOwn(kindFields, field);
}

// Versioned profiles own connection requirements and the server validates their
// hosting adapter. Kind fields describe Automatic providers, not profiles.
function missingKindFields(
  draft: ProviderDraft,
  spec: ProviderKindCapability
): FieldErrors<ConnectionField> {
  return Object.fromEntries(
    spec.fields.flatMap(({ field, required }) => {
      if (!required || !isKindField(field)) return [];
      if (draft.profileId && field !== 'model') return [];
      const kind = kindFields[field];
      return draft[kind.field].trim() ? [] : [[kind.field, kind.missing]];
    })
  );
}

function credentialErrors(
  draft: ProviderDraft,
  spec: ProviderKindCapability,
  alreadyStored: boolean
): FieldErrors<ConnectionField> {
  if (!requiresCredential(spec, draft.authMode)) return {};
  if (!draft.useExternalCredential)
    return alreadyStored || draft.credential.trim()
      ? {}
      : { credential: 'Enter the credential.' };
  const reference = captureResult(() =>
    parseExternalReference(draft.externalCredentialReference ?? '')
  );
  if (reference.ok) return {};
  return {
    externalCredentialReference:
      reference.error instanceof Error
        ? reference.error.message
        : 'Enter a valid external credential reference.'
  };
}

export function validateConnection(
  draft: ProviderDraft,
  spec: ProviderKindCapability,
  { credentialAlreadyStored, profiles, vendors }: Context
): Validation<CreateProviderInput, ConnectionField> {
  const issue = draft.document?.issue;
  if (issue) return { ok: false, fieldErrors: { configuration: issue } };
  const probeModelMissing =
    requiresProbeModel(draft, spec, profiles, vendors) && !draft.model.trim();
  return validated<CreateProviderInput, ConnectionField>(
    {
      name: draft.name.trim() ? undefined : 'Enter a provider name.',
      profileId:
        draft.kind === 'plugin' && !draft.profileId
          ? 'Choose a plugin profile.'
          : undefined,
      ...(probeModelMissing ? { model: kindFields.model.missing } : {}),
      ...missingKindFields(draft, spec),
      ...credentialErrors(draft, spec, credentialAlreadyStored)
    },
    () => buildCreateProviderInput(draft, spec)
  );
}

export function connectionServerFields(
  draft: ProviderDraft,
  spec: ProviderKindCapability
): Readonly<Record<string, ConnectionField>> {
  return {
    name: 'name',
    model: 'model',
    display_name: 'name',
    'configuration.auth_mode': 'authMode',
    ...(showsEndpoint(draft, spec)
      ? { 'configuration.endpoint': 'endpoint' }
      : {}),
    ...(draft.kind === 'plugin'
      ? {
          'configuration.profile_id': 'profileId',
          'configuration.profile_revision': 'profileId'
        }
      : {}),
    ...(draft.authMode === 'headers'
      ? { 'configuration.options.credential_headers': 'credentialHeaders' }
      : {}),
    ...(requiresCredential(spec, draft.authMode)
      ? draft.useExternalCredential
        ? { credential_reference: 'externalCredentialReference' }
        : { credential: 'credential' }
      : {})
  };
}
