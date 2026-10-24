import { describe, expect, it, vi } from 'vitest';
import { apiClient } from '$lib/api/client';
import { gatewayFetch } from '$lib/api/gateway';
import {
  listNativeEndpoints,
  nativeResourceByteLimit,
  nativeResourceFingerprint,
  nativeResourcePath,
  runNativeResourceRequest,
  type NativeEndpoint,
  type NativeResourceRequest
} from './nativeLifecycle';
import { ResourceRequestError } from './lifecycle';

vi.mock('$lib/api/client', () => ({ apiClient: { GET: vi.fn() } }));
vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

const local = (kind: string, digit = '1') => `${kind}_${digit.repeat(32)}`;
const endpoint: NativeEndpoint = {
  operation: 'fine_tuning',
  surface: 'openai',
  kind: 'fine_tuning_job',
  action: 'create',
  method: 'POST',
  pattern: '/v1/fine_tuning/jobs',
  ancestors: [],
  metered: true
};
const request = (
  changes: Partial<NativeResourceRequest> = {}
): NativeResourceRequest => ({
  endpoint,
  apiKey: 'inference-key',
  route: 'training',
  id: '',
  parents: [],
  body: '{}',
  query: '{}',
  multipart: false,
  files: [],
  submissionKey: 'submission',
  ...changes
});

describe('native resource lifecycle requests', () => {
  it('uses registry endpoint metadata and refuses guessed or foreign routes', async () => {
    vi.mocked(apiClient.GET).mockResolvedValue({
      data: {
        endpoints: [
          { ...endpoint, ancestors: undefined },
          { ...endpoint, pattern: 'https://foreign.test/v1/fine_tuning/jobs' },
          { ...endpoint, pattern: '/v1/fine_tuning/jobs/../models' },
          { ...endpoint, kind: undefined },
          { ...endpoint, operation: 'agent_resource' }
        ]
      },
      response: new Response('{}')
    } as never);
    expect(await listNativeEndpoints()).toEqual([endpoint]);
    vi.mocked(apiClient.GET).mockResolvedValue({
      data: {},
      response: new Response('{}')
    } as never);
    expect(await listNativeEndpoints()).toEqual([]);
  });
  it('binds each nested ID to its declared parent kind', () => {
    const nested: NativeEndpoint = {
      operation: 'eval',
      surface: 'openai',
      kind: 'eval_output_item',
      action: 'get',
      method: 'GET',
      pattern: '/v1/evals/{parent0}/runs/{parent1}/output_items/{id}',
      ancestors: ['eval', 'eval_run'],
      metered: false
    };
    expect(
      nativeResourcePath(nested, local('eval_output_item'), [
        local('eval'),
        local('eval_run')
      ])
    ).toBe(
      `/v1/evals/${local('eval')}/runs/${local('eval_run')}/output_items/${local('eval_output_item')}`
    );
    expect(() =>
      nativeResourcePath(nested, local('eval_output_item'), [
        local('eval_run'),
        local('eval')
      ])
    ).toThrow('gateway eval ID');
    expect(() =>
      nativeResourcePath(nested, 'native-id', [
        local('eval'),
        local('eval_run')
      ])
    ).toThrow('gateway eval output item ID');
    expect(() =>
      nativeResourcePath(
        { ...nested, pattern: 'https://foreign.test/x' },
        '',
        []
      )
    ).toThrow('registered lifecycle endpoint');
  });
  it('keeps submission identities stable across equivalent JSON edits without losing numbers', async () => {
    const first = request({
      body: '{"seed":9007199254740993,"metadata":{"b":2,"a":1}}',
      query: '{"limit":"10","order":"asc"}'
    });
    const second = request({
      route: ' training ',
      body: '{ "metadata": {"a":1,"b":2}, "seed":9007199254740993 }',
      query: '{"order":"asc","limit":"10"}'
    });
    expect(await nativeResourceFingerprint(first)).toBe(
      await nativeResourceFingerprint(second)
    );
    expect(await nativeResourceFingerprint(first)).not.toBe(
      await nativeResourceFingerprint({
        ...first,
        body: first.body.replace('9007199254740993', '9007199254740992')
      })
    );
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response(`{"id":"${local('fine_tuning_job')}"}`)
    );
    await runNativeResourceRequest(first);
    const [path, options] = vi.mocked(gatewayFetch).mock.calls.at(-1)!;
    expect(path).toBe('/v1/fine_tuning/jobs?limit=10&order=asc');
    expect(options?.body).toContain('9007199254740993');
    expect(options?.headers).toMatchObject({
      Authorization: 'Bearer inference-key',
      'X-OLP-Route': 'training',
      'Idempotency-Key': 'submission',
      'Content-Type': 'application/json'
    });
  });
  it('preserves unknown-acceptance resource IDs without retrying the mutation', async () => {
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response('unknown acceptance', {
        status: 409,
        headers: { 'X-OLP-Resource-ID': local('fine_tuning_job') }
      })
    );
    await expect(runNativeResourceRequest(request())).rejects.toMatchObject({
      resourceId: local('fine_tuning_job')
    });
    expect(vi.mocked(gatewayFetch)).toHaveBeenCalledOnce();
    await expect(runNativeResourceRequest(request())).rejects.toBeInstanceOf(
      ResourceRequestError
    );
  });
  it('uses bounded native multipart skill files and string metadata', async () => {
    const skill: NativeEndpoint = {
      operation: 'skill',
      surface: 'anthropic',
      kind: 'skill',
      action: 'create',
      method: 'POST',
      pattern: '/anthropic/v1/skills',
      ancestors: [],
      metered: false
    };
    const upload = request({
      endpoint: skill,
      multipart: true,
      body: '{"display_title":"My skill"}',
      files: [new File(['instructions'], 'SKILL.md', { type: 'text/markdown' })]
    });
    vi.mocked(gatewayFetch).mockResolvedValue(new Response('{}'));
    await runNativeResourceRequest(upload);
    const [, options] = vi.mocked(gatewayFetch).mock.calls.at(-1)!;
    expect(options?.headers).not.toHaveProperty('Content-Type');
    const form = options?.body as FormData;
    expect(form.get('display_title')).toBe('My skill');
    expect((form.get('files[]') as File).name).toBe('SKILL.md');
    const before = await nativeResourceFingerprint(upload);
    expect(
      await nativeResourceFingerprint({
        ...upload,
        files: [new File(['changed'], 'SKILL.md', { type: 'text/markdown' })]
      })
    ).not.toBe(before);
    const calls = vi.mocked(gatewayFetch).mock.calls.length;
    await expect(
      runNativeResourceRequest({
        ...upload,
        files: [new File([new Uint8Array(nativeResourceByteLimit + 1)], 'big')]
      })
    ).rejects.toThrow('limited to 4 MiB');
    expect(vi.mocked(gatewayFetch).mock.calls.length).toBe(calls);
  });
  it('downloads binary file content as a Blob and cancels oversized streams', async () => {
    const content: NativeEndpoint = {
      operation: 'container',
      surface: 'openai',
      kind: 'container_file',
      action: 'content',
      method: 'GET',
      pattern: '/v1/containers/{parent0}/files/{id}/content',
      ancestors: ['container'],
      metered: true
    };
    const input = request({
      endpoint: content,
      id: local('container_file'),
      parents: [local('container')]
    });
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response(new Uint8Array([0, 255, 7]))
    );
    const result = await runNativeResourceRequest(input);
    expect(result.text).toBe('');
    expect(new Uint8Array(await result.download!.arrayBuffer())).toEqual(
      new Uint8Array([0, 255, 7])
    );
    const cancel = vi.fn();
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(new Uint8Array(nativeResourceByteLimit + 1));
          },
          cancel
        })
      )
    );
    await expect(runNativeResourceRequest(input)).rejects.toThrow(
      '4 MiB console limit'
    );
    expect(cancel).toHaveBeenCalledOnce();
  });
  it('refuses invalid path IDs, missing routes and malformed controls before dispatch', async () => {
    for (const input of [
      request({ route: '' }),
      request({ query: '{"limit":10}' }),
      request({ body: '[]' }),
      request({ submissionKey: '' }),
      request({ endpoint: { ...endpoint, pattern: '/api/v1/settings' } })
    ]) {
      await expect(runNativeResourceRequest(input)).rejects.toThrow();
    }
    expect(vi.mocked(gatewayFetch)).not.toHaveBeenCalled();
  });
});
