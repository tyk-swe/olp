import { describe, expect, it, vi } from 'vitest';
import {
  agentSubmissionFingerprint,
  buildRetrievalBindingInput,
  draftRetainedEvidence,
  extractResourceJsonId,
  extractResourceResponseMetadata,
  isGatewayResourceId,
  parseRetainedEvidence,
  readBoundedResponseBytes,
  ResourceRequestError,
  runAgentRequest
} from './lifecycle';
import { gatewayFetch } from '#lib/management/gateway.js';

vi.mock('#lib/management/gateway.js', () => ({ gatewayFetch: vi.fn() }));

const hex32 = (digit = 'a') => digit.repeat(32);

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

  it('concatenates multi-chunk responses up to the byte limit and cancels above it', async () => {
    const withinLimit = new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(new Uint8Array([1, 2]));
          controller.enqueue(new Uint8Array([3, 4]));
          controller.close();
        }
      })
    );
    expect(await readBoundedResponseBytes(withinLimit, 4)).toEqual(
      new Uint8Array([1, 2, 3, 4])
    );

    const cancel = vi.fn();
    const overLimit = new Response(
      new ReadableStream({
        start(controller) {
          controller.enqueue(new Uint8Array([1, 2, 3]));
          controller.enqueue(new Uint8Array([4, 5]));
        },
        cancel
      })
    );
    await expect(readBoundedResponseBytes(overLimit, 4)).rejects.toThrow(
      'too large'
    );
    expect(cancel).toHaveBeenCalledOnce();
  });

  it('preserves native numbers and submission identity', async () => {
    vi.mocked(gatewayFetch).mockResolvedValue(
      new Response(`{"id":"agent_resource_${hex32()}"}`)
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

describe('resource metadata extraction and validation helpers', () => {
  it('validates gateway resource IDs and extracts IDs and pagination tokens safely', () => {
    const cacheId = `context_cache_${hex32('1')}`;
    const agentId = `agent_resource_${hex32('2')}`;
    expect(isGatewayResourceId('context_cache', cacheId)).toBe(true);
    expect(isGatewayResourceId('context_cache', agentId)).toBe(false);
    expect(isGatewayResourceId('BadKind', `BadKind_${hex32('1')}`)).toBe(false);

    expect(
      extractResourceResponseMetadata(
        JSON.stringify({ id: agentId, next_page_token: 'cursor-2' })
      )
    ).toEqual({ id: agentId, nextPageToken: 'cursor-2' });

    expect(
      extractResourceJsonId(
        JSON.stringify({ id: 'foreign', name: cacheId }),
        'context_cache'
      )
    ).toBe(cacheId);

    expect(
      extractResourceJsonId(JSON.stringify({ id: agentId }), 'context_cache')
    ).toBeNull();

    expect(
      extractResourceResponseMetadata(JSON.stringify({ name: cacheId }))
    ).toEqual({ id: null, nextPageToken: null });

    for (const bad of ['', 'not-json', '[]', '{"id":123}']) {
      expect(extractResourceResponseMetadata(bad)).toEqual({
        id: null,
        nextPageToken: null
      });
    }
  });

  it('drafts retained evidence templates per resource kind and validates int64 evidence JSON', () => {
    const drafted = JSON.parse(
      draftRetainedEvidence(
        {
          kind: 'eval_run',
          created_at: '2026-01-01T00:00:00Z',
          upstream_model: 'fallback-model',
          retained_settlement: {
            state: 'evidence_missing',
            missing_evidence: true,
            remote_terminated: false,
            next_observation_at: '2026-01-02T00:00:00Z',
            models: ['pinned-model']
          }
        },
        { idempotencyKey: 'fixed-key', now: '2026-01-02T00:00:00Z' }
      )
    );
    expect(drafted).toEqual({
      idempotency_key: 'fixed-key',
      evidence_digest: '',
      start: '2026-01-01T00:00:00Z',
      end: '2026-01-02T00:00:00Z',
      model: 'pinned-model',
      input_tokens: 0,
      output_tokens: 0,
      cached_input_tokens: 0,
      invocation_count: 0,
      terminal: false
    });

    const valid = parseRetainedEvidence(
      '{"idempotency_key":"k1","training_tokens":9007199254740993,"terminal":false}'
    );
    expect(valid).toEqual({
      ok: true,
      value:
        '{"idempotency_key":"k1","training_tokens":9007199254740993,"terminal":false}'
    });

    for (const invalid of ['', '   ', '[]', '"text"', '{broken']) {
      expect(parseRetainedEvidence(invalid)).toMatchObject({ ok: false });
    }
  });

  it('builds validated retrieval binding payloads without throwing on bad input', () => {
    const base = {
      project: ' proj-1 ',
      name: ' Docs KB ',
      route: ' docs-route ',
      target: ' target-1 ',
      backend: 'azure_ai_search' as const,
      resource: ' docs-index ',
      enabled: true,
      fields: JSON.stringify({
        text: 'content',
        id: 'doc_id',
        search: ['content', 'title'],
        metadata: { tenant: 'tenant_id' }
      }),
      grants: 'key-1\nkey-2, key-3'
    };
    expect(buildRetrievalBindingInput(base)).toEqual({
      ok: true,
      value: {
        project_id: 'proj-1',
        name: 'Docs KB',
        route: 'docs-route',
        target_id: 'target-1',
        backend: 'azure_ai_search',
        backend_resource: 'docs-index',
        enabled: true,
        fields: {
          text: 'content',
          id: 'doc_id',
          search: ['content', 'title'],
          metadata: { tenant: 'tenant_id' }
        },
        key_grants: ['key-1', 'key-2', 'key-3']
      }
    });

    expect(
      buildRetrievalBindingInput({ ...base, grants: 'key-1\nkey-1' })
    ).toEqual({
      ok: false,
      message: 'Each API-key grant must appear once.'
    });

    for (const badFields of [
      '{broken',
      '[]',
      '{"unknown":"x"}',
      '{"":"x","text":"content"}',
      '{"text":123}',
      '{"search":["ok",1]}',
      '{"metadata":{"tenant":42}}'
    ]) {
      expect(
        buildRetrievalBindingInput({ ...base, fields: badFields })
      ).toMatchObject({ ok: false });
    }
  });
});
