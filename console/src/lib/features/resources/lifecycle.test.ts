import { describe, expect, it, vi } from 'vitest';
import { runAgentRequest, ResourceRequestError } from './lifecycle';
import { gatewayFetch } from '$lib/api/gateway';

vi.mock('$lib/api/gateway', () => ({ gatewayFetch: vi.fn() }));

describe('agent resource requests', () => {
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
