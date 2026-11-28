import { flushSync } from 'svelte';
import { describe, expect, it, vi } from 'vitest';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import { clickButton, findButton, mountWithState } from '#test/mount.js';
import ProviderBulkModels from './ProviderBulkModels.svelte';
import type { Provider } from '#lib/domain/providers/api.js';

type Reply = (write: string) => Response | Promise<Response> | undefined;

function connection(
  kind: 'openai' | 'openai_compatible',
  version: number
): Provider {
  return {
    capability_count: 0,
    certified_capability_count: 0,
    configuration: { kind, auth_mode: 'api_key' },
    connector_ready: false,
    created_at: '2026-01-01T00:00:00Z',
    enabled_model_count: 0,
    etag: `version-${version}`,
    id: 'provider-id',
    model_count: 2,
    name: 'Service',
    pending_activation: true,
    project_id: null,
    project_name: null,
    state: 'draft',
    updated_at: '2026-01-01T00:00:00Z'
  };
}

async function bulk(
  names: string[],
  options: { kind?: 'openai' | 'openai_compatible'; reply?: Reply } = {}
) {
  const kind = options.kind ?? 'openai_compatible';
  const writes: string[] = [];
  const server = { version: 1, probed: false };
  const current = () => ({
    ...connection(kind, server.version),
    ...(server.probed
      ? {
          last_probe_at: '2026-01-02T00:00:00Z',
          last_probe_status: 'succeeded'
        }
      : {})
  });
  const advance = () => ({ ETag: `version-${++server.version}` });
  serve(
    async (request) => {
      const path = new URL(request.url).pathname.replace(
        '/api/v1/providers/provider-id',
        ''
      );
      if (request.method === 'GET') {
        if (path === '/models')
          return json({
            items: names.map((name) => ({
              id: name,
              display_name: name,
              upstream_model: name,
              enabled: false,
              capabilities: []
            })),
            provider: current()
          });
        if (path.endsWith('/capabilities'))
          return json({
            provider_kind: kind,
            capabilities: [
              { operation: 'generation', surface: 'openai', mode: 'unary' }
            ]
          });
        return json(current());
      }
      const write = `${request.method} ${path} ${request.headers.get('If-Match')}`;
      writes.push(write);
      const reply = await options.reply?.(write);
      if (reply) return reply;
      if (path === '/probe') {
        server.probed = true;
        return Response.json(
          { succeeded: true, detail: 'Connection succeeded' },
          { headers: advance() }
        );
      }
      if (path.endsWith('/certify'))
        return Response.json(
          { results: [{ succeeded: true }] },
          { headers: advance() }
        );
      advance();
      return json(current());
    },
    ['read', 'configure']
  );
  const onChanged = vi.fn();
  const { target } = mountWithState(
    ProviderBulkModels,
    { provider: connection(kind, 1), canManage: true, onChanged },
    { attach: true }
  );
  const boxes = () => [
    ...target.querySelectorAll<HTMLInputElement>('input[type="checkbox"]')
  ];
  await eventually(() => expect(boxes()).toHaveLength(names.length));
  for (const box of boxes()) box.click();
  flushSync();
  const validate = () =>
    clickButton(target, `Validate ${names.length} selected models`);
  return { target, writes, onChanged, validate };
}

function refusal(status: number, detail: string) {
  return json({ status, title: detail, detail }, status);
}

describe('bulk model validation', () => {
  it('writes each model against the connection version the last write returned', async () => {
    const run = await bulk(['model-a', 'model-b'], { kind: 'openai' });

    run.validate();

    await eventually(() =>
      expect(run.target.textContent).toContain('Checked 2 models.')
    );
    expect(run.writes).toEqual([
      'POST /probe "version-1"',
      'PATCH /models/model-a "version-2"',
      'POST /models/model-a/certify "version-3"',
      'PATCH /models/model-b "version-4"',
      'POST /models/model-b/certify "version-5"'
    ]);
    expect(run.onChanged).toHaveBeenCalledOnce();
  });

  it('validates only the failed models again', async () => {
    let failing = true;
    const run = await bulk(['model-a', 'model-b'], {
      reply: (write) => {
        if (!failing || !write.includes('/model-b/certify')) return undefined;
        failing = false;
        return Response.json(
          { results: [{ succeeded: false }] },
          { headers: { ETag: 'version-5' } }
        );
      }
    });
    run.validate();
    await eventually(() =>
      expect(
        findButton(run.target, 'Retry 1 failed models')
      ).not.toBeUndefined()
    );
    expect(run.target.textContent).toContain(
      'model-b: one or more capabilities failed validation.'
    );
    run.writes.length = 0;

    clickButton(run.target, 'Retry 1 failed models');

    await eventually(() =>
      expect(run.target.textContent).toContain('Checked 1 models.')
    );
    expect(run.writes.map((write) => write.split(' ')[1])).toEqual([
      '/models/model-b',
      '/models/model-b/certify'
    ]);
    expect(run.target.textContent).not.toContain('failed validation');
  });

  it('goes on past a refused write and stops once one may have landed', async () => {
    const run = await bulk(['model-a', 'model-b', 'model-c'], {
      reply: (write) => {
        if (write.includes('/model-a ')) return refusal(422, 'Not allowed');
        if (write.includes('/model-b ')) return refusal(503, 'Unavailable');
        return undefined;
      }
    });

    run.validate();

    await eventually(() =>
      expect(run.target.textContent).toContain('Stopped after 1 models.')
    );
    expect(run.target.textContent).toContain('model-a: Not allowed');
    expect(run.target.textContent).toContain('model-b: Unavailable');
    expect(run.writes).toHaveLength(2);
    expect(findButton(run.target, 'Validate 3 selected models')?.disabled).toBe(
      true
    );
    expect(run.onChanged).not.toHaveBeenCalled();

    clickButton(run.target, 'Retry refresh');

    await eventually(() =>
      expect(findButton(run.target, 'Retry refresh')).toBeUndefined()
    );
    expect(run.onChanged).toHaveBeenCalledOnce();
  });

  it('writes nothing when the connection changed after the models were read', async () => {
    const run = await bulk(['model-a']);
    const fetched = globalThis.fetch;
    vi.stubGlobal('fetch', (input: Request) =>
      new URL(input.url).pathname === '/api/v1/providers/provider-id'
        ? Promise.resolve(json(connection('openai_compatible', 9)))
        : fetched(input)
    );

    run.validate();

    await eventually(() =>
      expect(run.target.textContent).toContain(
        'The connection changed. Refresh before validating these models.'
      )
    );
    expect(run.writes).toEqual([]);
    expect(findButton(run.target, 'Retry refresh')).not.toBeUndefined();
  });

  it('sends no further write after validation is stopped', async () => {
    const held = Promise.withResolvers<Response>();
    const run = await bulk(['model-a', 'model-b'], {
      reply: () => held.promise
    });
    run.validate();
    await eventually(() => expect(run.writes).toHaveLength(1));

    clickButton(run.target, 'Stop validation');
    held.resolve(json(connection('openai_compatible', 2)));

    await eventually(() =>
      expect(run.target.textContent).toContain('Stopped after 0 models.')
    );
    expect(run.writes).toHaveLength(1);
    expect(findButton(run.target, 'Stop validation')).toBeUndefined();
  });
});
