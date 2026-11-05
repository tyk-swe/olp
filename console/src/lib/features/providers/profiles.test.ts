import { describe, expect, it } from 'vitest';
import type {
  ProviderProfile,
  ProviderProfilePlugin
} from './providerProfiles';
import {
  declaresModels,
  dialectSurface,
  operationFields,
  pluginOptionFields,
  pluginProfileGroups,
  vendorSpeaks
} from './profiles';

const plugin: ProviderProfilePlugin = {
  digest: 'digest-a',
  name: 'Connector',
  version: '1'
};
const profile: ProviderProfile = {
  authentication: ['api_key'],
  default_schemas: {},
  dialect: 'openai-chat',
  dialect_revision: '1',
  documentation: '',
  hosting: 'custom',
  id: 'profile-a',
  kind: 'plugin',
  label: 'Profile A',
  operation_dialects: {},
  operations: [],
  plugin,
  query_settings: [],
  revision: 'revision-a',
  semantic_headers: [],
  strict: true,
  transport: 'plugin'
};

describe('provider profiles', () => {
  it('groups plugin profiles by build while preserving catalog order', () => {
    const second = { ...profile, id: 'profile-b' };
    const other = {
      ...profile,
      id: 'profile-c',
      plugin: { ...plugin, digest: 'digest-b' }
    };
    expect(
      pluginProfileGroups([
        { ...profile, kind: 'builtin' },
        profile,
        other,
        second,
        { ...profile, plugin: undefined }
      ])
    ).toEqual([
      { plugin, profiles: [profile, second] },
      { plugin: other.plugin, profiles: [other] }
    ]);
  });

  it('requires manual plugin models unless the selected profile and revision provide discovery', () => {
    const configuration = {
      kind: 'plugin',
      profile_id: profile.id,
      profile_revision: profile.revision
    };
    expect(
      declaresModels(configuration, [{ ...profile, model_discovery: true }])
    ).toBe(false);
    expect(
      declaresModels(configuration, [{ ...profile, model_discovery: false }])
    ).toBe(true);
    expect(
      declaresModels({ ...configuration, profile_revision: 'other' }, [profile])
    ).toBe(true);
    expect(declaresModels(configuration, undefined)).toBe(true);
    expect(declaresModels({ ...configuration, kind: 'builtin' }, [])).toBe(
      false
    );
  });

  it('limits generation profiles to vendor-supported dialects', () => {
    expect(vendorSpeaks(profile, ['openai-chat'])).toBe(true);
    expect(vendorSpeaks(profile, ['anthropic-messages'])).toBe(false);
    expect(vendorSpeaks(profile, [])).toBe(false);
    expect(vendorSpeaks(profile, undefined)).toBe(true);
    expect(vendorSpeaks({ dialect: 'embeddings' }, [])).toBe(true);
  });

  it.each([
    ['anthropic-messages', 'anthropic'],
    ['gemini-generate-content', 'gemini'],
    ['openai-responses', 'openai'],
    [undefined, 'openai']
  ] as const)('uses the native surface for %s', (dialect, surface) => {
    expect(dialectSurface(dialect)).toBe(surface);
  });

  it('exposes declared plugin options and required fields in schema order', () => {
    expect(
      pluginOptionFields({
        options_schema: {
          properties: {
            region: { type: 'string', title: 'Region' },
            account: { type: 'string' }
          },
          required: ['account']
        }
      })
    ).toEqual([
      {
        name: 'region',
        schema: { type: 'string', title: 'Region' },
        required: false
      },
      { name: 'account', schema: { type: 'string' }, required: true }
    ]);
    expect(pluginOptionFields(undefined)).toEqual([]);
    expect(pluginOptionFields({ options_schema: { properties: [] } })).toEqual(
      []
    );
    expect(
      pluginOptionFields({
        options_schema: { properties: { key: {} }, required: 'key' }
      })
    ).toEqual([{ name: 'key', schema: {}, required: false }]);
  });

  it('uses the selected operation schema for configuration fields', () => {
    const configured = {
      ...profile,
      default_schemas: {
        generation: {
          properties: {
            values: { properties: { temperature: { type: 'number' } } }
          }
        },
        embeddings: {
          properties: {
            values: { properties: { dimensions: { type: 'integer' } } }
          }
        }
      }
    };
    expect(operationFields(configured, 'generation')).toEqual({
      temperature: { type: 'number' }
    });
    expect(operationFields(configured, 'embeddings')).toEqual({
      dimensions: { type: 'integer' }
    });
    expect(operationFields(configured, 'unknown')).toEqual({});
  });
});
