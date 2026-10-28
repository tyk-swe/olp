import { createHash } from 'node:crypto';
import { describe, expect, it } from 'vitest';
import {
  publicationForm,
  publicationInput,
  serverPricingForm,
  serverPricingInput,
  toolPin,
  type ServerPricingForm
} from './form';
import type { MCPServer, Publication } from './api';

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
  unit_price: '0.25',
  tool_prices: { lookup: '0.5' },
  calls_per_minute: 90,
  max_concurrency: 3,
  per_key_calls_per_minute: 45,
  per_key_max_concurrency: 2,
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

  it('copies reactive tool proxies into independent editor and request values', async () => {
    const pin = await toolPin(server, 'lookup');
    const tools = new Proxy([new Proxy(pin, {})], {});
    const current: Publication = {
      id,
      project_id: id,
      kind: 'toolset',
      name: 'Tools',
      etag: id,
      revision_id: id,
      retired: false,
      draft: { project_id: id, name: 'Tools', toolset: { tools } }
    };
    const form = publicationForm(current);
    form.tools[0].name = 'edited';
    expect(pin.name).toBe('lookup');
    form.tools = new Proxy([new Proxy(form.tools[0], {})], {});
    const input = publicationInput('toolsets', form);
    expect(input.toolset?.tools[0].name).toBe('edited');
    input.toolset!.tools[0].name = 'request';
    expect(form.tools[0].name).toBe('edited');
    expect(structuredClone(input)).toEqual(input);
  });

  it('round trips server pricing and limits without credentials', () => {
    const form = serverPricingForm(server);
    expect(form).toEqual({
      unitPrice: '0.25',
      toolPrices: { lookup: '0.5' },
      callsPerMinute: '90',
      maxConcurrency: '3',
      perKeyCallsPerMinute: '45',
      perKeyMaxConcurrency: '2'
    });
    const input = serverPricingInput(server, form);
    expect(input).toEqual({
      name: server.name,
      endpoint: server.endpoint,
      enabled: true,
      unit_price: '0.25',
      tool_prices: { lookup: '0.5' },
      calls_per_minute: 90,
      max_concurrency: 3,
      per_key_calls_per_minute: 45,
      per_key_max_concurrency: 2
    });
    expect(input).not.toHaveProperty('credential');
    form.toolPrices.lookup = '9';
    expect(server.tool_prices.lookup).toBe('0.5');
  });

  it('keeps unpriced, free and blank tool prices distinct', () => {
    const form = serverPricingForm({
      ...server,
      unit_price: null,
      tool_prices: {}
    });
    expect(form.unitPrice).toBe('');
    expect(serverPricingInput(server, form).unit_price).toBeNull();
    form.unitPrice = '0';
    expect(serverPricingInput(server, form).unit_price).toBe('0');
    form.toolPrices = { lookup: '  ' };
    expect(serverPricingInput(server, form).tool_prices).toEqual({});
  });

  it('refuses malformed prices and limits outside installation bounds', () => {
    const base = serverPricingForm(server);
    const patches: Partial<ServerPricingForm>[] = [
      { unitPrice: '-1' },
      { unitPrice: '1e3' },
      { toolPrices: { lookup: '-0.5' } },
      { toolPrices: { absent: '1' } },
      { callsPerMinute: '0' },
      { callsPerMinute: '60001' },
      { maxConcurrency: '1025' },
      { perKeyCallsPerMinute: '1.5' },
      { perKeyMaxConcurrency: '' }
    ];
    for (const patch of patches) {
      expect(() => serverPricingInput(server, { ...base, ...patch })).toThrow();
    }
  });
});
