import { describe, expect, it } from 'vitest';
import {
  declaresModels,
  dialectSurface,
  pluginOptionFields,
  pluginProfileGroups,
  type ProviderProfile
} from './profiles';

function profile(overrides: Partial<ProviderProfile>): ProviderProfile {
  return {
    id: 'openai-chat',
    revision: '1',
    label: 'OpenAI Chat Completions',
    kind: 'openai',
    dialect: 'openai-chat',
    dialect_revision: 'unversioned-2026-09-22',
    hosting: 'direct-openai',
    authentication: ['api_key'],
    transport: 'http',
    operations: ['generation'],
    operation_dialects: { generation: 'openai-chat' },
    default_schemas: {},
    semantic_headers: [],
    query_settings: [],
    documentation: '',
    strict: true,
    ...overrides
  };
}

describe('plugin profile catalogue', () => {
  it('groups plugin profiles by the plugin build that supplies them', () => {
    const first = { digest: 'a'.repeat(64), name: 'acme', version: '1.0.0' };
    const second = { ...first, digest: 'b'.repeat(64), version: '1.1.0' };
    const plugin = {
      kind: 'plugin',
      hosting: 'plugin',
      authentication: ['static_credential']
    };
    const chat = profile({
      ...plugin,
      id: 'acme-chat',
      revision: first.digest,
      plugin: first
    });
    const messages = profile({
      ...plugin,
      id: 'acme-messages',
      revision: first.digest,
      dialect: 'anthropic-messages',
      plugin: first
    });
    const upgraded = profile({
      ...plugin,
      id: 'acme-chat',
      revision: second.digest,
      plugin: second
    });
    expect(
      pluginProfileGroups([profile({}), chat, upgraded, messages])
    ).toEqual([
      { plugin: first, profiles: [chat, messages] },
      { plugin: second, profiles: [upgraded] }
    ]);
  });

  it('has operators declare the models of a plugin profile without discovery', () => {
    const digest = 'a'.repeat(64);
    const pinned = {
      kind: 'plugin',
      profile_id: 'acme-chat',
      profile_revision: digest
    };
    const plugin = profile({
      kind: 'plugin',
      id: 'acme-chat',
      revision: digest
    });
    const discovering = { ...plugin, model_discovery: true };
    expect(declaresModels(pinned, [plugin])).toBe(true);
    expect(declaresModels(pinned, [discovering])).toBe(false);
    // Another build of the plugin discovers nothing for this one.
    expect(
      declaresModels(pinned, [{ ...discovering, revision: 'b'.repeat(64) }])
    ).toBe(true);
    expect(declaresModels(pinned, undefined)).toBe(true);
    expect(declaresModels({ kind: 'openai' }, undefined)).toBe(false);
  });

  it('names the surface that speaks a dialect natively', () => {
    expect(dialectSurface('anthropic-messages')).toBe('anthropic');
    expect(dialectSurface('gemini-generate-content')).toBe('gemini');
    expect(dialectSurface('openai-responses')).toBe('openai');
    expect(dialectSurface(undefined)).toBe('openai');
  });
});

describe('plugin profile options', () => {
  it('lists the declared options in order, marking those a provider must set', () => {
    const workspace = {
      options_schema: {
        type: 'object',
        additionalProperties: false,
        required: ['workspace'],
        properties: {
          workspace: { type: 'string', title: 'Workspace' },
          region: { type: 'string', title: 'Region', enum: ['us', 'eu'] }
        }
      }
    };
    expect(pluginOptionFields(workspace)).toEqual([
      {
        name: 'workspace',
        schema: { type: 'string', title: 'Workspace' },
        required: true
      },
      {
        name: 'region',
        schema: { type: 'string', title: 'Region', enum: ['us', 'eu'] },
        required: false
      }
    ]);
    expect(pluginOptionFields(profile({}))).toEqual([]);
    expect(pluginOptionFields(undefined)).toEqual([]);
  });
});
