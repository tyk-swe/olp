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
  delegation: null,
  oauth: null,
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
  it('round trips structured prompt messages, declared string variables and defaults', () => {
    const input = publicationInput('prompts', {
      ...publicationForm(),
      project: id,
      name: 'Greeting',
      promptMode: 'messages',
      messages: [
        { role: 'system', content: 'Help {{name}}' },
        { role: 'user', content: 'Hello {{name}}' }
      ],
      variables: 'name',
      temperature: '0.2',
      topP: '0.8',
      maxOutputTokens: '128'
    });
    expect(input.prompt).toEqual({
      template: '',
      variables: [],
      messages: [
        { role: 'system', content: 'Help {{name}}' },
        { role: 'user', content: 'Hello {{name}}' }
      ],
      variable_schema: {
        type: 'object',
        properties: { name: { type: 'string' } },
        required: ['name'],
        additionalProperties: false
      },
      default_parameters: {
        temperature: 0.2,
        top_p: 0.8,
        max_output_tokens: 128
      }
    });
    const current: Publication = {
      id,
      project_id: id,
      kind: 'prompt',
      name: 'Greeting',
      etag: id,
      revision_id: id,
      retired: false,
      draft: input
    };
    expect(publicationInput('prompts', publicationForm(current))).toEqual(
      input
    );
  });

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
    const pinned = await toolPin(server, 'lookup');
    if (!pinned.ok) throw new Error(pinned.message);
    const pin = pinned.pin;
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
    for (const refused of [
      await toolPin({ ...server, enabled: false }, 'lookup'),
      await toolPin(
        { ...server, retired_at: '2026-10-22T00:00:00Z' },
        'lookup'
      ),
      await toolPin(server, 'unknown')
    ]) {
      expect(refused.ok).toBe(false);
      if (!refused.ok) expect(refused.message.length).toBeGreaterThan(0);
    }
  });

  it('copies reactive tool proxies into independent editor and request values', async () => {
    const pinned = await toolPin(server, 'lookup');
    if (!pinned.ok) throw new Error(pinned.message);
    const pin = pinned.pin;
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
    const result = serverPricingInput(server, form);
    if (!result.ok) throw new Error(result.message);
    const input = result.input;
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
    const unpriced = serverPricingInput(server, form);
    expect(unpriced.ok && unpriced.input.unit_price).toBeNull();
    form.unitPrice = '0';
    const free = serverPricingInput(server, form);
    expect(free.ok && free.input.unit_price).toBe('0');
    form.toolPrices = { lookup: '  ' };
    const blank = serverPricingInput(server, form);
    expect(blank.ok && blank.input.tool_prices).toEqual({});
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
      const result = serverPricingInput(server, { ...base, ...patch });
      expect(result.ok).toBe(false);
      if (!result.ok) expect(result.message.length).toBeGreaterThan(0);
    }
  });
});

describe('publication editor boundaries', () => {
  it('starts a new editor with blank resource bindings and the supported default protocol', () => {
    expect(publicationForm()).toMatchObject({
      project: '',
      name: '',
      tools: [],
      template: '',
      variables: '',
      protocol: 'a2a-1.0.0',
      endpoint: '',
      cardDigest: '',
      pricingRevision: '',
      maxCost: '',
      messagePrice: '',
      toolsetRevisions: '',
      routeRevisions: ''
    });
  });

  it.each(['a2a-0.3.0', 'a2a-1.0.0'] as const)(
    'round trips %s agent bindings and trims edited identifiers',
    (protocol) => {
      const draft: Publication['draft'] = {
        project_id: id,
        name: 'Agent',
        agent: {
          protocol,
          endpoint: 'https://agent.example.test',
          card_digest: 'catalog',
          pricing_revision_id: 'price-revision',
          max_cost: '12.34',
          message_price: '1.50',
          toolset_revisions: ['tool-a', 'tool-b'],
          route_revisions: ['route-a', 'route-b']
        }
      };
      const current: Publication = {
        id,
        project_id: id,
        kind: 'agent',
        name: 'Agent',
        etag: id,
        revision_id: id,
        retired: false,
        draft
      };
      const form = publicationForm(current);
      expect(publicationInput('agents', form)).toEqual(draft);
      const edited = {
        ...form,
        name: ' Edited ',
        endpoint: ' https://other.example.test ',
        cardDigest: ' new-catalog ',
        pricingRevision: ' new-pricing ',
        maxCost: ' 0.25 ',
        messagePrice: ' 0 ',
        toolsetRevisions: ' tool-c,\ntool-d ',
        routeRevisions: ' route-c\t route-d '
      };
      expect(publicationInput('agents', edited)).toEqual({
        project_id: id,
        name: 'Edited',
        agent: {
          protocol,
          endpoint: 'https://other.example.test',
          card_digest: 'new-catalog',
          pricing_revision_id: 'new-pricing',
          max_cost: '0.25',
          message_price: '0',
          toolset_revisions: ['tool-c', 'tool-d'],
          route_revisions: ['route-c', 'route-d']
        }
      });
    }
  );

  it('keeps an omitted agent message price absent and includes a declared price for new agents', () => {
    const form = publicationForm();
    expect(publicationInput('agents', form).agent).not.toHaveProperty(
      'message_price'
    );
    expect(
      publicationInput('agents', { ...form, messagePrice: ' 0.35 ' }).agent
        ?.message_price
    ).toBe('0.35');
  });

  it('round trips prompt variables and removes empty separators from edited variables', () => {
    const draft: Publication['draft'] = {
      project_id: id,
      name: 'Prompt',
      prompt: {
        template: 'Hello {{name}} in {{language}}',
        variables: ['name', 'language']
      }
    };
    const current: Publication = {
      id,
      project_id: id,
      kind: 'prompt',
      name: 'Prompt',
      etag: id,
      revision_id: id,
      retired: false,
      draft
    };
    const form = publicationForm(current);
    expect(publicationInput('prompts', form)).toEqual(draft);
    expect(
      publicationInput('prompts', {
        ...form,
        variables: ' , name,\n\tlanguage , '
      }).prompt?.variables
    ).toEqual(['name', 'language']);
  });

  it('preserves a legacy template when switching to structured messages', () => {
    const current: Publication = {
      id,
      project_id: id,
      kind: 'prompt',
      name: 'Prompt',
      etag: id,
      revision_id: id,
      retired: false,
      draft: {
        project_id: id,
        name: 'Prompt',
        prompt: {
          template: 'Hello {{name}}',
          variables: ['name'],
          messages: []
        }
      }
    };
    const converted = publicationInput('prompts', {
      ...publicationForm(current),
      promptMode: 'messages'
    });
    expect(converted.prompt?.messages).toEqual([
      { role: 'user', content: 'Hello {{name}}' }
    ]);
    expect(converted.prompt?.variable_schema?.required).toEqual(['name']);
  });

  it('allows a catalog member when other certified tools have different names', async () => {
    const result = await toolPin(
      {
        ...server,
        catalog: {
          ...server.catalog,
          tools: [
            ...server.catalog.tools,
            { name: 'other', description: '', input_schema: { type: 'object' } }
          ]
        }
      },
      'lookup'
    );
    expect(result.ok && result.pin.name).toBe('lookup');
  });

  it('accepts inclusive installation limits and bounded multi-digit decimal prices', () => {
    const result = serverPricingInput(server, {
      ...serverPricingForm(server),
      unitPrice: ' 123456789012.123456789012 ',
      toolPrices: { lookup: ' 12.345678901234 ' },
      callsPerMinute: ' 60000 ',
      maxConcurrency: ' 1024 ',
      perKeyCallsPerMinute: ' 1 ',
      perKeyMaxConcurrency: ' 1 '
    });
    expect(result.ok && result.input).toEqual({
      name: server.name,
      endpoint: server.endpoint,
      enabled: server.enabled,
      unit_price: '123456789012.123456789012',
      tool_prices: { lookup: '12.345678901234' },
      calls_per_minute: 60000,
      max_concurrency: 1024,
      per_key_calls_per_minute: 1,
      per_key_max_concurrency: 1
    });
    for (const price of [
      '1234567890123',
      '0.1234567890123',
      '12x',
      '+12',
      '.5',
      '5.'
    ]) {
      expect(
        serverPricingInput(server, {
          ...serverPricingForm(server),
          unitPrice: price
        }).ok
      ).toBe(false);
      expect(
        serverPricingInput(server, {
          ...serverPricingForm(server),
          toolPrices: { lookup: price }
        }).ok
      ).toBe(false);
    }
  });
});

describe('pricing editor feedback', () => {
  it('prices a certified tool without requiring every other tool to share its name', () => {
    const catalog = {
      ...server,
      catalog: {
        ...server.catalog,
        tools: [
          ...server.catalog.tools,
          { name: 'other', description: '', input_schema: { type: 'object' } }
        ]
      }
    };
    const result = serverPricingInput(catalog, {
      ...serverPricingForm(catalog),
      toolPrices: { lookup: '12.5' }
    });
    expect(result.ok && result.input.tool_prices).toEqual({ lookup: '12.5' });
  });
  it.each([
    ['callsPerMinute', 'Calls per minute'],
    ['maxConcurrency', 'Max concurrency'],
    ['perKeyCallsPerMinute', 'Per-key calls per minute'],
    ['perKeyMaxConcurrency', 'Per-key max concurrency']
  ] as const)('identifies the invalid %s field', (field, label) => {
    const result = serverPricingInput(server, {
      ...serverPricingForm(server),
      [field]: 'invalid'
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.message).toContain(label);
  });
  it('keeps complete tool pins when reopening and submitting a publication', async () => {
    const result = await toolPin(server, 'lookup');
    expect(result.ok).toBe(true);
    if (!result.ok) return;
    const draft: Publication['draft'] = {
      project_id: id,
      name: 'Pinned',
      toolset: { tools: [result.pin] }
    };
    const current: Publication = {
      id,
      project_id: id,
      kind: 'toolset',
      name: 'Pinned',
      etag: id,
      revision_id: id,
      retired: false,
      draft
    };
    const form = publicationForm(current);
    expect(form.tools).toEqual([result.pin]);
    expect(publicationInput('toolsets', form)).toEqual(draft);
  });
});
