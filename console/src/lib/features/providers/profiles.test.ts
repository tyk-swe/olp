import { describe, expect, it } from 'vitest';
import {
  dialectSurface,
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

  it('names the surface that speaks a dialect natively', () => {
    expect(dialectSurface('anthropic-messages')).toBe('anthropic');
    expect(dialectSurface('gemini-generate-content')).toBe('gemini');
    expect(dialectSurface('openai-responses')).toBe('openai');
    expect(dialectSurface(undefined)).toBe('openai');
  });
});
