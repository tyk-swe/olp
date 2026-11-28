import type { beforeNavigate } from '$app/navigation';
import { flushSync } from 'svelte';
import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { emptyCursorHistory } from '#lib/listing/pagination.js';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  clickButton,
  findButton,
  mountWithState,
  setFieldValue
} from '#test/mount.js';
import type { ApiKey, UpdateApiKeyInput } from './api';
import ApiKeysPage from './ApiKeysPage.svelte';
import RoutingPolicyEditor from '#lib/features/routes/RoutingPolicyEditor.svelte';

type BeforeNavigate = Parameters<Parameters<typeof beforeNavigate>[0]>[0];

const navigation = vi.hoisted(() => ({
  guards: [] as ((event: BeforeNavigate) => void)[],
  navigated: [] as string[],
  currentPath: '/keys/key-1'
}));

vi.mock('$app/navigation', () => ({
  beforeNavigate: (guard: (event: BeforeNavigate) => void) => {
    navigation.guards.push(guard);
  },
  goto: vi.fn(async (destination: string) => {
    let cancelled = false;
    const event = {
      type: 'goto',
      shallow: false,
      from: { url: new URL(navigation.currentPath, 'http://console.test') },
      to: { url: new URL(destination, 'http://console.test') },
      cancel: () => {
        cancelled = true;
      }
    } as unknown as BeforeNavigate;
    for (const guard of navigation.guards) guard(event);
    if (!cancelled) navigation.navigated.push(destination);
  })
}));

vi.mock('$app/paths', () => ({
  resolve: (path: string) => (path.startsWith('/') ? path : `/${path}`)
}));

const expiryA = '2099-01-01T12:00';
const expiryB = '2099-02-01T12:00';
const originalExpiry = new Date(expiryA).toISOString();
const runtimeGeneration = { id: 'gen-1', sequence: 1 };

const baseKey: ApiKey = {
  id: 'key-1',
  lookup_id: 'lookup-1',
  name: 'browser-key',
  etag: 'etag-1',
  project_id: null,
  project_name: null,
  budget_group_id: null,
  created_by: 'owner',
  created_by_email: 'owner@example.test',
  created_at: '2026-01-01T00:00:00Z',
  expires_at: originalExpiry,
  allow_provider_state: false,
  allowed_attribution_keys: [],
  allowed_routes: [],
  response_metadata: false,
  regional_limits: {},
  scopes: ['inference'],
  budget: {
    daily: {
      accrued: '0',
      limit: null,
      window_ends_at: '2026-01-02T00:00:00Z'
    },
    monthly: {
      accrued: '0',
      limit: null,
      window_ends_at: '2026-02-01T00:00:00Z'
    },
    unpriced_attempts: 0
  }
};

afterEach(() => {
  navigation.guards.length = 0;
  navigation.navigated.length = 0;
  navigation.currentPath = `/keys/${baseKey.id}`;
});

function renderEditor(
  handlers: {
    onPatch?: (
      request: Request,
      body: UpdateApiKeyInput
    ) => Response | Promise<Response>;
    onGetKey?: (request: Request) => Response | Promise<Response>;
  } = {}
) {
  let currentKey = baseKey;
  const patches = vi.fn<(request: Request, body: UpdateApiKeyInput) => void>();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/auth/capabilities')
        return json({
          local_login_enabled: true,
          oidc_login_enabled: false,
          gateway_available: true,
          limits_enforced: true
        });
      if (path === `/api/v1/api-keys/${baseKey.id}`) {
        if (request.method === 'PATCH') {
          const body = (await request.json()) as UpdateApiKeyInput;
          patches(request, body);
          if (handlers.onPatch) return handlers.onPatch(request, body);
          const nextEtag = `etag-${patches.mock.calls.length + 1}`;
          currentKey = {
            ...currentKey,
            name: body.name ?? currentKey.name,
            expires_at:
              body.expires_at !== undefined
                ? (body.expires_at ?? undefined)
                : currentKey.expires_at,
            etag: nextEtag
          };
          return json({
            etag: nextEtag,
            runtime_generation: runtimeGeneration
          });
        }
        return handlers.onGetKey
          ? handlers.onGetKey(request)
          : json(currentKey);
      }
      if (path === `/api/v1/api-keys/${baseKey.id}/routing-policy`)
        return json({ etag: currentKey.etag, policy: {} });
      return json({ items: [] });
    },
    ['read', 'keys']
  );

  const confirm = vi.fn(() => false);
  vi.stubGlobal('confirm', confirm);
  const { target } = mountWithState(
    ApiKeysPage,
    {
      keyId: baseKey.id,
      listState: emptyCursorHistory(),
      RoutingPolicyEditor
    },
    {
      attach: true,
      configureClient: (client) =>
        client.setDefaultOptions({ queries: { retry: false } })
    }
  );
  const nameInput = () => {
    const input = target.querySelector<HTMLInputElement>('#key-name');
    if (!input) throw new Error('Missing key name input');
    return input;
  };
  const expiryInput = () => {
    const input = target.querySelector<HTMLInputElement>('#key-expiry');
    if (!input) throw new Error('Missing key expiry input');
    return input;
  };
  const saveButton = () => findButton(target, 'Save and publish');
  const submit = () => {
    const form = target.querySelector('form.key-form');
    if (!form) throw new Error('Missing API key form');
    form.dispatchEvent(
      new SubmitEvent('submit', { bubbles: true, cancelable: true })
    );
    flushSync();
  };
  return {
    target,
    patches,
    confirm,
    nameInput,
    expiryInput,
    saveButton,
    submit
  };
}

describe('ApiKeysPage editing lifecycle', () => {
  it('navigates cleanly to /keys after a save without prompting for unsaved changes', async () => {
    const editor = renderEditor();
    await eventually(() => expect(editor.saveButton()).toBeDefined());
    setFieldValue(editor.nameInput(), 'browser-key-saved');

    editor.submit();

    await eventually(() => expect(navigation.navigated).toEqual(['/keys']));
    expect(editor.patches).toHaveBeenCalledOnce();
    expect(editor.confirm).not.toHaveBeenCalled();
  });

  it('keeps concurrent edits made during an in-flight save and submits them with the updated etag and expiry', async () => {
    const firstPatchStarted = Promise.withResolvers<void>();
    const releaseFirstPatch = Promise.withResolvers<void>();
    let currentKey = baseKey;
    let patchCount = 0;

    onTestFinished(() => releaseFirstPatch.resolve());

    const editor = renderEditor({
      onGetKey: () => json(currentKey),
      onPatch: async (_request, body) => {
        patchCount += 1;
        const etag = `etag-${patchCount + 1}`;
        if (patchCount === 1) {
          firstPatchStarted.resolve();
          await releaseFirstPatch.promise;
        }
        currentKey = {
          ...currentKey,
          name: body.name ?? currentKey.name,
          expires_at:
            body.expires_at !== undefined
              ? (body.expires_at ?? undefined)
              : currentKey.expires_at,
          etag
        };
        return json({ etag, runtime_generation: runtimeGeneration });
      }
    });

    await eventually(() => expect(editor.saveButton()).toBeDefined());
    setFieldValue(editor.nameInput(), 'browser-key-submitted');
    setFieldValue(editor.expiryInput(), expiryB);
    editor.submit();

    await firstPatchStarted.promise;
    setFieldValue(editor.nameInput(), 'browser-key-newer-edit');
    setFieldValue(editor.expiryInput(), expiryA);
    releaseFirstPatch.resolve();

    await eventually(() => expect(editor.saveButton()?.disabled).toBe(false));
    expect(editor.nameInput().value).toBe('browser-key-newer-edit');
    expect(editor.expiryInput().value).toBe(expiryA);
    expect(navigation.navigated).toEqual([]);

    clickButton(editor.target, 'Cancel');
    expect(editor.confirm).toHaveBeenCalledTimes(1);
    expect(editor.confirm).toHaveBeenCalledWith('Discard unsaved changes?');
    expect(navigation.navigated).toEqual([]);

    editor.submit();
    await eventually(() => expect(navigation.navigated).toEqual(['/keys']));
    expect(editor.patches).toHaveBeenCalledTimes(2);
    const [secondRequest, secondBody] = editor.patches.mock.calls[1];
    expect(secondRequest.headers.get('If-Match')).toBe('"etag-2"');
    expect(secondBody.name).toBe('browser-key-newer-edit');
    expect(secondBody.expires_at).toBe(originalExpiry);
    expect(editor.confirm).toHaveBeenCalledTimes(1);
  });

  it('shows a refresh alert when post-save refresh fails and allows clean cancel without confirmation', async () => {
    let saved = false;
    const editor = renderEditor({
      onGetKey: () =>
        saved ? json({ title: 'Try again', status: 503 }, 503) : json(baseKey),
      onPatch: () => {
        saved = true;
        return json({ etag: 'etag-2', runtime_generation: runtimeGeneration });
      }
    });

    await eventually(() => expect(editor.saveButton()).toBeDefined());
    setFieldValue(editor.nameInput(), 'browser-key-refresh-failed');
    editor.submit();

    await eventually(() =>
      expect(
        editor.target.querySelector('[role="alert"]')?.textContent
      ).toContain('API key saved, but refreshing failed.')
    );
    expect(navigation.navigated).toEqual([]);

    clickButton(editor.target, 'Cancel');
    await eventually(() => expect(navigation.navigated).toEqual(['/keys']));
    expect(editor.confirm).not.toHaveBeenCalled();
  });

  it('keeps the form dirty when saving fails so cancel prompts and stays on the page when dismissed', async () => {
    const editor = renderEditor({
      onPatch: () => json({ title: 'Try again', status: 503 }, 503)
    });

    await eventually(() => expect(editor.saveButton()).toBeDefined());
    setFieldValue(editor.nameInput(), 'browser-key-unsaved');
    editor.submit();

    await eventually(() =>
      expect(
        editor.target.querySelector('[role="alert"]')?.textContent
      ).toContain('Try again')
    );
    expect(navigation.navigated).toEqual([]);

    clickButton(editor.target, 'Cancel');
    expect(editor.confirm).toHaveBeenCalledTimes(1);
    expect(editor.confirm).toHaveBeenCalledWith('Discard unsaved changes?');
    expect(navigation.navigated).toEqual([]);
  });
});
