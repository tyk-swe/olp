import type { ProviderKindCapability } from '../api/models';
import type { ProviderProfile } from '../api/profiles';

/** The provider kind an installed plugin's profiles are offered under. */
export const pluginSpec: ProviderKindCapability = {
  kind: 'plugin',
  label: 'Provider plugin',
  description: 'A profile an installed provider plugin supplies.',
  default_auth_mode: 'static_credential',
  auth_modes: [
    {
      mode: 'static_credential',
      label: 'Static credential',
      credential: 'required'
    },
    { mode: 'grant', label: 'Grant', credential: 'grant' }
  ],
  fields: [],
  presets: []
};

/** A profile of the reference plugin build with this digest. */
export function referenceProfile(
  digest: string,
  overrides: Partial<ProviderProfile> = {}
): ProviderProfile {
  return {
    id: 'reference-chat',
    revision: digest,
    label: 'Reference Chat Completions',
    kind: 'plugin',
    dialect: 'openai-chat',
    dialect_revision: 'unversioned-2026-09-22',
    hosting: 'plugin',
    authentication: ['static_credential'],
    transport: 'http',
    operations: ['generation'],
    operation_dialects: { generation: 'openai-chat' },
    default_schemas: {},
    semantic_headers: ['Openai-Beta'],
    query_settings: [],
    documentation: '',
    strict: true,
    plugin: { digest, name: 'reference', version: '0.1.0' },
    ...overrides
  };
}
