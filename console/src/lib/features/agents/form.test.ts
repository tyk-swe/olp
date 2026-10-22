import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import { publicationForm, publicationInput, toolPin } from './form';
import type { MCPServer } from './api';

const id = '0192cf87-d4ab-7f2e-a8b1-c2d3e4f50607';
const server: MCPServer = {
  id,
  project_id: id,
  name: 'Certified tools',
  transport: 'streamable_http',
  endpoint: 'https://tools.example.test/mcp',
  enabled: true,
  has_credential: false,
  etag: id,
  revision_id: id,
  revision: 1,
  certified_at: '2026-10-22T00:00:00Z',
  retired_at: null,
  catalog: {
    protocol_version: '2025-11-25',
    digest: 'a'.repeat(64),
    tools: [
      { name: 'lookup', description: '', input_schema: { type: 'object' } }
    ]
  }
};

describe('publication configuration', () => {
  it('builds one typed definition without credential material', () => {
    const form = publicationForm();
    Object.assign(form, {
      project: id,
      name: ' Greeting ',
      template: 'Hello {{name}}',
      variables: 'name'
    });
    expect(publicationInput('prompts', form)).toEqual({
      project_id: id,
      name: 'Greeting',
      prompt: { template: 'Hello {{name}}', variables: ['name'] }
    });
    form.toolsetRevisions = `${id},\n${id}`;
    const agent = publicationInput('agents', form);
    expect(agent.agent?.toolset_revisions).toEqual([id, id]);
    expect(agent).not.toHaveProperty('credential');
    expect(agent).not.toHaveProperty('prompt');
  });

  it('pins the exact certified revision and domain-stable alias', async () => {
    const pin = await toolPin(server, 'lookup');
    expect(pin).toEqual({
      server_id: id,
      revision_id: id,
      catalog_digest: 'a'.repeat(64),
      name: 'lookup',
      alias: `t_${id.replaceAll('-', '')}_${createHash('sha256').update('lookup').digest('hex').slice(0, 32)}`
    });
    const form = publicationForm();
    form.tools = [pin];
    const input = publicationInput('toolsets', form);
    input.toolset!.tools[0].name = 'changed';
    expect(form.tools[0].name).toBe('lookup');
  });

  it('refuses disabled, retired or undiscovered tools', async () => {
    await expect(
      toolPin({ ...server, enabled: false }, 'lookup')
    ).rejects.toThrow();
    await expect(
      toolPin({ ...server, retired_at: '2026-10-22T00:00:00Z' }, 'lookup')
    ).rejects.toThrow();
    await expect(toolPin(server, 'unknown')).rejects.toThrow();
  });
});
