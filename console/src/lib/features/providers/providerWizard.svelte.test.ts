import { describe, expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, route, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  clickButton,
  findButton,
  mountWithState,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import type {
  ProviderKindCapability,
  Provider
} from '#lib/domain/providers/api.js';
import type { ProviderProbe } from './providers';
import ProviderWizard from './ProviderWizard.svelte';

const spec: ProviderKindCapability = {
  kind: 'openai_compatible',
  label: 'Compatible provider',
  description: '',
  default_auth_mode: 'api_key',
  auth_modes: [{ mode: 'api_key', label: 'API key', credential: 'required' }],
  fields: [{ field: 'endpoint', label: 'Endpoint', required: true }],
  presets: []
};

const provider: Provider = {
  id: 'provider-id',
  etag: 'provider-version',
  name: 'Service',
  configuration: {
    kind: 'openai_compatible',
    auth_mode: 'api_key',
    endpoint: 'https://example.test/v1'
  },
  capability_count: 0,
  certified_capability_count: 0,
  enabled_model_count: 0,
  model_count: 0,
  connector_ready: false,
  pending_activation: true,
  project_id: null,
  project_name: null,
  state: 'draft',
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z'
};

const activatable: Provider = {
  ...provider,
  capability_count: 1,
  certified_capability_count: 1,
  enabled_model_count: 1,
  model_count: 1,
  connector_ready: true,
  last_probe_at: '2026-01-02T00:00:00Z',
  last_probe_status: 'succeeded'
};

type Override = (request: Request) => Response | Promise<Response> | undefined;

function probeResponse(succeeded: boolean): Response {
  const body: ProviderProbe = {
    checked_at: '2026-01-01T00:00:00Z',
    detail: succeeded ? 'Connection succeeded' : 'Connection failed',
    probe_type: 'connection',
    provider_id: provider.id,
    succeeded
  };
  return new Response(JSON.stringify(body), {
    headers: { 'Content-Type': 'application/json', ETag: provider.etag }
  });
}

function refusal(status: number, field: string, message: string): Response {
  return json(
    {
      title: 'Refused',
      status,
      detail: message,
      errors: { [field]: [{ code: 'validation_failed', message }] }
    },
    status
  );
}

function serveWizard({
  probe = vi.fn(() => probeResponse(true)),
  saved = () => provider,
  override
}: {
  probe?: () => Response;
  saved?: () => Provider;
  override?: Override;
} = {}) {
  const create = vi.fn(async (request: Request): Promise<Response> => {
    route(request, 'POST', '/api/v1/providers');
    return json({ id: provider.id });
  });
  const handler = (request: Request) => {
    const response = override?.(request);
    if (response) return response;
    const path = new URL(request.url).pathname;
    if (path === '/api/v1/provider-kinds') return json({ items: [spec] });
    if (path === '/api/v1/providers' && request.method === 'POST')
      return create(request);
    if (path === `/api/v1/providers/${provider.id}/models`)
      return json({ items: [], provider: saved() });
    if (path === `/api/v1/providers/${provider.id}/probe`) {
      route(request, 'POST', path);
      expect(request.headers.get('If-Match')).toBe(`"${provider.etag}"`);
      return probe();
    }
    if (path === '/api/v1/provider-kinds/openai_compatible/capabilities')
      return json({ provider_kind: spec.kind, capabilities: [] });
    return json({ title: `Unexpected request ${request.method} ${path}` }, 404);
  };
  serve(handler, ['read', 'configure']);
  return { create, probe };
}

function reportedError(control: Element): string | undefined {
  if (control.getAttribute('aria-invalid') !== 'true') return undefined;
  const [id] = (control.getAttribute('aria-describedby') ?? '').split(' ');
  return control.ownerDocument.getElementById(id)?.textContent?.trim();
}

async function openWizard() {
  const { target } = mountWithState(ProviderWizard, {}, { attach: true });
  const page = document.createElement('main');
  document.body.append(page);
  page.append(target);
  const field = (label: string) => {
    const control = [...target.querySelectorAll('label')].find(
      (item) => item.textContent?.trim() === label
    )?.control;
    if (
      !(control instanceof HTMLInputElement) &&
      !(control instanceof HTMLTextAreaElement)
    )
      throw new Error(`Missing field ${label}`);
    return control;
  };
  const wizard = {
    target,
    field,
    type: (label: string, value: string) => setFieldValue(field(label), value),
    summary: () =>
      target.querySelector('[data-error-summary]')?.textContent?.trim(),
    step: () => target.querySelector('[aria-current="step"]')?.textContent,
    shows: (text: string) =>
      eventually(() => expect(target.textContent).toContain(text)),
    async press(label: string) {
      await eventually(() =>
        expect(findButton(target, label)?.disabled).toBe(false)
      );
      clickButton(target, label);
    },
    async fill(credential = 'secret') {
      wizard.type('Provider name', 'Service');
      wizard.type('Endpoint', 'https://example.test/v1');
      wizard.type('Credential', credential);
    },
    async save() {
      await wizard.fill();
      submitForm(target);
      await wizard.shows('Discover upstream models');
    }
  };
  await eventually(() => expect(wizard.field('Endpoint')).toBeDefined());
  flushSync();
  return wizard;
}

function requestIds(send: { mock: { calls: [Request][] } }) {
  return send.mock.calls.map(([request]) =>
    request.headers.get('Idempotency-Key')
  );
}

describe('provider wizard', () => {
  it('reports each missing connection value at its field', async () => {
    const { create } = serveWizard();
    const wizard = await openWizard();

    submitForm(wizard.target);

    await eventually(() =>
      expect(document.activeElement).toBe(wizard.field('Provider name'))
    );
    expect(reportedError(wizard.field('Provider name'))).toBe(
      'Enter a provider name.'
    );
    expect(reportedError(wizard.field('Endpoint'))).toBe('Enter the endpoint.');
    expect(reportedError(wizard.field('Credential'))).toBe(
      'Enter the credential.'
    );
    expect(wizard.summary()).toBeUndefined();
    expect(create).not.toHaveBeenCalled();
  });

  it('reports an invalid external credential reference at its field', async () => {
    const { create } = serveWizard();
    const wizard = await openWizard();
    await wizard.fill();
    wizard.target
      .querySelector<HTMLInputElement>('input[type="checkbox"]')
      ?.click();
    flushSync();
    const reference = wizard.target.querySelector<HTMLTextAreaElement>(
      '[aria-label="Pinned external credential reference"]'
    );
    if (!reference) throw new Error('Missing reference field');
    setFieldValue(reference, '{broken');

    submitForm(wizard.target);

    await eventually(() =>
      expect(reportedError(reference)).toBe(
        'Enter a JSON object with store, secret_id and immutable version.'
      )
    );
    expect(create).not.toHaveBeenCalled();
  });

  it('saves the connection, tests it and discards the pasted credential', async () => {
    const { create, probe } = serveWizard();
    const wizard = await openWizard();

    await wizard.save();

    expect(await create.mock.calls[0][0].clone().json()).toMatchObject({
      name: 'Service',
      credential: 'secret',
      configuration: {
        kind: 'openai_compatible',
        endpoint: 'https://example.test/v1',
        auth_mode: 'api_key'
      }
    });
    expect(probe).toHaveBeenCalledOnce();
    expect(wizard.step()).toContain('Models and capabilities');
    await wizard.press('Back');
    expect(wizard.field('Provider name').value).toBe('Service');
    expect(wizard.field('Credential').value).toBe('');
  });

  it('retries a failed connection test without creating the saved provider again', async () => {
    const probe = vi
      .fn(() => probeResponse(true))
      .mockReturnValueOnce(probeResponse(false));
    const { create } = serveWizard({ probe });
    const wizard = await openWizard();
    await wizard.fill();

    submitForm(wizard.target);
    await eventually(() => expect(wizard.summary()).toBe('Connection failed'));
    expect(wizard.step()).toContain('Connection');
    expect(wizard.field('Credential').value).toBe('');
    submitForm(wizard.target);
    await wizard.shows('Discover upstream models');

    expect(create).toHaveBeenCalledOnce();
    expect(probe).toHaveBeenCalledTimes(2);
    expect(wizard.summary()).toBeUndefined();
  });

  it('retries an uncertain save with the original values and request identity', async () => {
    const { create } = serveWizard();
    create.mockResolvedValueOnce(
      json({ title: 'Unavailable', status: 503 }, 503)
    );
    const wizard = await openWizard();
    await wizard.fill();

    submitForm(wizard.target);
    await eventually(() => expect(wizard.summary()).toBe('Unavailable'));
    wizard.type('Provider name', 'Edited');
    submitForm(wizard.target);
    await eventually(() =>
      expect(wizard.summary()).toContain('Retry its original fields')
    );
    expect(create).toHaveBeenCalledOnce();

    wizard.type('Provider name', 'Service');
    submitForm(wizard.target);
    await wizard.shows('Discover upstream models');
    const [first, second] = requestIds(create);
    expect(first).toBeTruthy();
    expect(second).toBe(first);
  });

  it('shows a refused value at its field and saves the correction as a new request', async () => {
    const { create } = serveWizard();
    create.mockResolvedValueOnce(
      refusal(422, 'name', 'That name is already used.')
    );
    const wizard = await openWizard();
    await wizard.fill();

    submitForm(wizard.target);
    await eventually(() =>
      expect(reportedError(wizard.field('Provider name'))).toBe(
        'That name is already used.'
      )
    );
    expect(wizard.summary()).toBeUndefined();
    wizard.type('Provider name', 'Second service');
    submitForm(wizard.target);
    await wizard.shows('Discover upstream models');

    const [first, second] = requestIds(create);
    expect(first).toBeTruthy();
    expect(second).not.toBe(first);
  });

  it('lists a refusal that no field of the form can show', async () => {
    const { create } = serveWizard();
    create.mockResolvedValueOnce(
      refusal(
        422,
        'configuration.options.models',
        'A model override is invalid.'
      )
    );
    const wizard = await openWizard();
    await wizard.fill();

    submitForm(wizard.target);

    await eventually(() =>
      expect(wizard.summary()).toContain('A model override is invalid.')
    );
    expect(wizard.summary()).toContain('validation_failed');
  });

  it('starts a blank connection after the copied one is activated', async () => {
    let state: Provider['state'] = 'draft';
    serveWizard({
      saved: () => ({ ...activatable, state }),
      override: (request) => {
        const path = new URL(request.url).pathname;
        if (path === '/api/v1/providers/source')
          return json({ ...provider, id: 'source', name: 'Original' });
        if (!path.endsWith('/activate')) return undefined;
        state = 'active';
        return json({ runtime_generation: { sequence: 7 } });
      }
    });
    window.history.replaceState({}, '', '?copy=source');
    const wizard = await openWizard();
    await eventually(() =>
      expect(wizard.field('Provider name').value).toBe('Original copy')
    );
    wizard.type('Credential', 'secret');
    submitForm(wizard.target);
    await wizard.press('Continue to activation');
    await wizard.press('Activate provider');
    await wizard.shows('Now build a stable route slug.');

    await wizard.press('Add another connection');

    await eventually(() =>
      expect(wizard.field('Provider name').value).toBe('')
    );
    expect(wizard.field('Endpoint').value).toBe('');
    expect(wizard.field('Credential').value).toBe('');
    expect(wizard.step()).toContain('Connection');
    expect(wizard.target.textContent).not.toContain('runtime generation');
    window.history.replaceState({}, '', window.location.pathname);
  });

  it('offers manual model declaration when discovery returns no models', async () => {
    const payload = vi.fn<(body: unknown) => void>();
    const discovery = vi.fn(async (request: Request) => {
      route(request, 'POST', `/api/v1/providers/${provider.id}/discovery`);
      payload(await request.json());
      return json({
        ...provider,
        model_count: payload.mock.calls.length === 1 ? 0 : 2
      });
    });
    serveWizard({
      override: (request) =>
        new URL(request.url).pathname.endsWith('/discovery')
          ? discovery(request)
          : undefined
    });
    const wizard = await openWizard();
    await wizard.save();

    await wizard.press('Discover upstream models');
    await eventually(() =>
      expect(wizard.summary()).toContain('Use the manual identifier fallback')
    );
    await wizard.press('Add identifiers for review');
    await eventually(() =>
      expect(reportedError(wizard.field('Upstream model identifiers'))).toBe(
        'Enter at least one upstream model identifier.'
      )
    );
    expect(discovery).toHaveBeenCalledOnce();
    wizard.type('Upstream model identifiers', 'model-a\nmodel-b');
    await wizard.press('Add identifiers for review');
    await eventually(() =>
      expect(wizard.field('Upstream model identifiers').value).toBe('')
    );

    expect(payload).toHaveBeenNthCalledWith(1, { models: [] });
    expect(payload).toHaveBeenNthCalledWith(2, {
      models: [
        { upstream_model: 'model-a', display_name: 'model-a' },
        { upstream_model: 'model-b', display_name: 'model-b' }
      ]
    });
    expect(wizard.summary()).toBeUndefined();
  });

  it('retries an uncertain activation before allowing connection changes', async () => {
    let state: Provider['state'] = 'draft';
    const activate = vi
      .fn((request: Request) => {
        route(request, 'POST', `/api/v1/providers/${provider.id}/activate`);
        state = 'active';
        return json({ runtime_generation: { sequence: 42 } });
      })
      .mockReturnValueOnce(json({ title: 'Unavailable', status: 503 }, 503));
    serveWizard({
      saved: () => ({ ...activatable, state }),
      override: (request) =>
        new URL(request.url).pathname.endsWith('/activate')
          ? activate(request)
          : undefined
    });
    const wizard = await openWizard();
    await wizard.save();
    await wizard.press('Continue to activation');

    await wizard.press('Activate provider');
    await eventually(() => expect(wizard.summary()).toBe('Unavailable'));
    await wizard.press('Back');
    await eventually(() =>
      expect(wizard.summary()).toContain(
        'Activation may already have committed'
      )
    );
    expect(wizard.step()).toContain('Activation');
    await wizard.press('Activate provider');
    await wizard.shows('Now build a stable route slug.');

    const [first, second] = requestIds(activate);
    expect(first).toBeTruthy();
    expect(second).toBe(first);
    expect(wizard.target.textContent).toContain('runtime generation 42');
  });

  it('retries refresh after confirmed activation without activating twice', async () => {
    let state: Provider['state'] = 'draft';
    let refreshUnavailable = true;
    const activate = vi.fn(() => {
      state = 'active';
      return json({ runtime_generation: { sequence: 42 } });
    });
    serveWizard({
      saved: () => ({ ...activatable, state }),
      override: (request) => {
        const path = new URL(request.url).pathname;
        if (path.endsWith('/activate')) return activate();
        if (
          state === 'active' &&
          refreshUnavailable &&
          path.endsWith('/models')
        )
          return json({ title: 'Refresh unavailable', status: 503 }, 503);
        return undefined;
      }
    });
    const wizard = await openWizard();
    await wizard.save();
    await wizard.press('Continue to activation');

    await wizard.press('Activate provider');
    await eventually(() =>
      expect(wizard.summary()).toContain('Refresh unavailable')
    );
    refreshUnavailable = false;
    await wizard.press('Activate provider');
    await wizard.shows('Now build a stable route slug.');

    expect(activate).toHaveBeenCalledOnce();
    expect(wizard.summary()).toBeUndefined();
  });
});
