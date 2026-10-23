import { describe, expect, it, vi } from 'vitest';
import {
  agentSubmissionFingerprint,
  runAgentRequest,
  ResourceRequestError
} from './lifecycle';
import { gatewayFetch } from '$lib/api/gateway';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

describe('agent resource requests', () => {
  it('keeps unresolved submission identity across equivalent JSON edits', () => {
    const body =
      '{"description":"agent","agent_config":{"seed":9007199254740993,"items":[{"b":2,"a":1}]}}';
    const fingerprint = agentSubmissionFingerprint(' route ', body);
    expect(
      agentSubmissionFingerprint(
        'route',
        '{ "agent_config": { "items": [{ "a": 1, "b": 2 }], "seed": 9007199254740993 }, "description": "agent" }'
      )
    ).toBe(fingerprint);
    expect(fingerprint).toContain('9007199254740993');
    expect(
      agentSubmissionFingerprint(
        'route',
        body.replace('9007199254740993', '9007199254740992')
      )
    ).not.toBe(fingerprint);
    expect(agentSubmissionFingerprint('another-route', body)).not.toBe(
      fingerprint
    );
  });

  it('bounds streamed results before buffering an entire response', async () => {
    const cancel = vi.fn();
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response(
        new ReadableStream({
          start(controller) {
            controller.enqueue(new Uint8Array(4 * 1024 * 1024 + 1));
          },
          cancel
        })
      )
    );
    await expect(
      runAgentRequest({ action: 'list', apiKey: 'key' })
    ).rejects.toThrow('too large');
    expect(cancel).toHaveBeenCalledOnce();
  });
  it('preserves native numbers and submission identity', async () => {
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response('{"id":"agent_resource_123"}')
    );
    await runAgentRequest({
      action: 'create',
      apiKey: 'key',
      route: 'route',
      submissionKey: 'submission',
      body: '{"agent_config":{"seed":9007199254740993}}'
    });
    const [path, options] = vi.mocked(gatewayFetch).mock.calls.at(-1)!;
    expect(path).toBe('/gemini/v1beta/agents');
    expect(options?.body).toContain('9007199254740993');
    expect(options?.headers).toMatchObject({
      Authorization: 'Bearer key',
      'Idempotency-Key': 'submission',
      'X-OLP-Route': 'route'
    });
  });

  it('refuses native provider IDs before dispatch', async () => {
    const before = vi.mocked(gatewayFetch).mock.calls.length;
    await expect(
      runAgentRequest({
        action: 'delete',
        apiKey: 'key',
        id: 'agents/provider-id'
      })
    ).rejects.toThrow('gateway agent resource ID');
    expect(vi.mocked(gatewayFetch).mock.calls.length).toBe(before);
  });

  it('retains the recovery identity without retrying a failed create', async () => {
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response('unknown acceptance', {
        status: 409,
        headers: { 'X-OLP-Resource-ID': 'agent_resource_local' }
      })
    );
    const before = vi.mocked(gatewayFetch).mock.calls.length;
    try {
      await runAgentRequest({
        action: 'create',
        apiKey: 'key',
        route: 'route',
        submissionKey: 'same',
        body: '{}'
      });
      throw new Error('accepted');
    } catch (error) {
      expect(error).toBeInstanceOf(ResourceRequestError);
      expect((error as ResourceRequestError).resourceId).toBe(
        'agent_resource_local'
      );
    }
    expect(vi.mocked(gatewayFetch).mock.calls.length).toBe(before + 1);
  });
});
