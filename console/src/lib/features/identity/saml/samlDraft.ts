import { captureResult } from '#lib/editing/result.js';
import type { Validation } from '#lib/editing/validation.js';
import { errorMessage } from '#lib/management/http.js';
import type { getSAML, SAMLWrite } from './api';

export type SamlDraft = {
  enabled: boolean;
  metadata: string;
  email: string;
  groups: string;
  display: string;
  defaultRole: string;
  emailMappings: string;
  groupMappings: string;
  rotate: boolean;
};
export type SamlField = 'url' | Exclude<keyof SamlDraft, 'enabled' | 'rotate'>;

export const samlServerFields: Readonly<Record<string, SamlField>> = {
  metadata_xml: 'metadata',
  email_attribute: 'email',
  groups_attribute: 'groups',
  name_attribute: 'display',
  default_role: 'defaultRole',
  email_role_mappings: 'emailMappings',
  group_role_mappings: 'groupMappings'
};
export const samlImportServerFields: Readonly<Record<string, SamlField>> = {
  url: 'url'
};

export function samlDraft(
  saved?: NonNullable<Awaited<ReturnType<typeof getSAML>>>
): SamlDraft {
  return {
    enabled: saved?.enabled ?? false,
    metadata: saved?.metadata_xml ?? '',
    email: saved?.email_attribute ?? 'email',
    groups: saved?.groups_attribute ?? 'groups',
    display: saved?.name_attribute ?? 'name',
    defaultRole: saved?.default_role ?? '',
    emailMappings: JSON.stringify(saved?.email_role_mappings ?? [], null, 2),
    groupMappings: JSON.stringify(saved?.group_role_mappings ?? [], null, 2),
    rotate: false
  };
}

function parseMappings(text: string) {
  return captureResult((): SAMLWrite['email_role_mappings'] =>
    JSON.parse(text)
  );
}

export function validateSaml(
  draft: SamlDraft
): Validation<SAMLWrite, SamlField> {
  const email = parseMappings(draft.emailMappings);
  const groups = parseMappings(draft.groupMappings);
  if (!email.ok || !groups.ok)
    return {
      ok: false,
      fieldErrors: {
        emailMappings: email.ok ? undefined : errorMessage(email.error),
        groupMappings: groups.ok ? undefined : errorMessage(groups.error)
      }
    };
  return {
    ok: true,
    value: {
      enabled: draft.enabled,
      metadata_xml: draft.metadata,
      email_attribute: draft.email,
      groups_attribute: draft.groups,
      name_attribute: draft.display,
      default_role: (draft.defaultRole || null) as SAMLWrite['default_role'],
      email_role_mappings: email.value,
      group_role_mappings: groups.value,
      rotate_signing_key: draft.rotate
    }
  };
}
