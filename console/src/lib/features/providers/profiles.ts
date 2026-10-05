import { nativeObject } from '$lib/json/nativeJson';
import type {
  FieldSchema,
  ProviderProfile,
  ProviderProfilePlugin
} from './api/profiles';

export type PluginProfileGroup = {
  plugin: ProviderProfilePlugin;
  profiles: ProviderProfile[];
};

/**
 * The catalogue's plugin profiles grouped by the plugin build (digest) that
 * supplies them, in catalogue order. Only approved plugins appear there.
 */
export function pluginProfileGroups(
  profiles: readonly ProviderProfile[]
): PluginProfileGroup[] {
  const groups = new Map<string, PluginProfileGroup>();
  for (const profile of profiles) {
    if (profile.kind !== 'plugin' || !profile.plugin) continue;
    const group = groups.get(profile.plugin.digest) ?? {
      plugin: profile.plugin,
      profiles: []
    };
    group.profiles.push(profile);
    groups.set(profile.plugin.digest, group);
  }
  return [...groups.values()];
}

/**
 * Whether operators declare a provider's models instead of discovering them:
 * a plugin provider whose profile declares no model discovery. A profile the
 * catalogue does not (yet) list counts as declaring none.
 */
export function declaresModels(
  configuration: {
    kind: string;
    profile_id?: string | null;
    profile_revision?: string | null;
  },
  profiles: readonly ProviderProfile[] | undefined
): boolean {
  if (configuration.kind !== 'plugin') return false;
  const profile = profiles?.find(
    (candidate) =>
      candidate.kind === 'plugin' &&
      candidate.id === configuration.profile_id &&
      candidate.revision === configuration.profile_revision
  );
  return !profile?.model_discovery;
}

/** The generation dialects a vendor's reviewed contract can document. */
const GENERATION_DIALECTS = [
  'openai-chat',
  'openai-responses',
  'anthropic-messages',
  'gemini-generate-content',
  'bedrock-converse'
];

/**
 * Whether a vendor may use a profile: a generation profile must be in a
 * dialect the vendor's reviewed contract documents. A vendor the catalogue
 * does not describe is restricted only by its connector kind, as the server
 * restricts it.
 */
export function vendorSpeaks(
  profile: Pick<ProviderProfile, 'dialect'>,
  dialects: readonly string[] | undefined
): boolean {
  return (
    !dialects ||
    !GENERATION_DIALECTS.includes(profile.dialect) ||
    dialects.includes(profile.dialect)
  );
}

/** The client surface that speaks a generation dialect natively. */
export function dialectSurface(
  dialect: string | undefined
): 'openai' | 'anthropic' | 'gemini' {
  if (dialect === 'anthropic-messages') return 'anthropic';
  if (dialect === 'gemini-generate-content') return 'gemini';
  return 'openai';
}

export function schemaProperties(schema: unknown): Record<string, FieldSchema> {
  if (!nativeObject(schema) || !nativeObject(schema.properties)) return {};
  return schema.properties as Record<string, FieldSchema>;
}
/** One option a plugin profile declares, as the provider wizard edits it. */
export type PluginOptionField = {
  name: string;
  schema: FieldSchema;
  required: boolean;
};
/**
 * The options a plugin profile declares, in declared order, from the JSON
 * Schema of a provider's option values in its catalogue entry.
 */
export function pluginOptionFields(
  profile: Pick<ProviderProfile, 'options_schema'> | undefined
): PluginOptionField[] {
  const schema = profile?.options_schema;
  const required =
    nativeObject(schema) && Array.isArray(schema.required)
      ? schema.required
      : [];
  return Object.entries(schemaProperties(schema)).map(([name, field]) => ({
    name,
    schema: field,
    required: required.includes(name)
  }));
}
export function operationFields(
  profile: ProviderProfile,
  operation: string
): Record<string, FieldSchema> {
  return schemaProperties(
    schemaProperties(profile.default_schemas[operation]).values
  );
}
