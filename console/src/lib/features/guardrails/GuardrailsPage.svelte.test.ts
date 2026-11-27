import { expect, it, onTestFinished, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import {
  clickButton,
  findButton,
  mountWithState,
  setFieldValue
} from '#test/mount.js';
import GuardrailsPage from './GuardrailsPage.svelte';

const original = {
  id: 'original',
  policy_id: 'original',
  name: 'Original',
  project_id: 'project',
  type: 'builtin.keywords',
  configuration: { words: ['restricted'] },
  timeout_ms: 1000,
  on_error: 'fail_closed',
  entries: [],
  revision: 1,
  revision_id: 'original-revision',
  etag: 'original-etag'
};
const other = {
  ...original,
  id: 'other',
  policy_id: 'other',
  name: 'Other',
  etag: 'other-etag'
};

for (const editor of [
  {
    tab: 'checks',
    path: 'guardrails',
    create: 'New check',
    save: 'Save draft'
  },
  {
    tab: 'policies',
    path: 'guardrail-policies',
    create: 'New policy',
    save: 'Save policy draft'
  }
] as const) {
  it.each([
    'unchanged',
    'new',
    'existing',
    'same',
    'round-trip',
    'new-from-new',
    'same-untouched',
    'round-trip-untouched',
    'save-error',
    'save-error-new'
  ])(
    `preserves the %s editor after a delayed ${editor.tab} save`,
    async (destination) => {
      const pending = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
      const savedOriginal = vi.fn(() => original);
      const writes =
        vi.fn<(request: Request, body: { name: string }) => void>();
      serve(
        async (request) => {
          const path = new URL(request.url).pathname;
          if (request.method === 'GET') {
            if (path === '/api/v1/projects')
              return json({ items: [{ id: 'project', name: 'Project' }] });
            if (path === '/api/v1/guardrail-attachments')
              return json({ items: [] });
            return json({ items: [savedOriginal(), other] });
          }
          const body = (await request.json()) as { name: string };
          writes(request, body);
          if (writes.mock.calls.length === 1) {
            pending.resolve();
            await release.promise;
            if (destination.startsWith('save-error'))
              return json(
                { title: 'Save failed. Try again.', status: 500 },
                500
              );
          }
          const id =
            request.method === 'POST'
              ? 'created'
              : (path.split('/').at(-1) ?? '');
          if (
            id === 'original' &&
            request.headers.get('If-Match') !== `"${savedOriginal().etag}"`
          )
            return json(
              {
                title: 'The guardrail changed. Reload before saving.',
                status: 412
              },
              412
            );
          const saved = {
            ...original,
            ...body,
            id,
            policy_id: id,
            etag: `${id}-saved-etag`,
            revision: 2
          };
          if (id === 'original') savedOriginal.mockReturnValue(saved);
          return json(saved);
        },
        ['configure']
      );
      onTestFinished(() => release.resolve());
      const { target } = mountWithState(GuardrailsPage, { tab: editor.tab });
      const button = (name: string) => {
        const found = findButton(target, name);
        if (!found) throw new Error(`Missing button ${name}`);
        return found;
      };
      const click = (name: string) => clickButton(target, name);
      const rename = (value: string) => {
        const input = target.querySelector('input');
        if (!input) throw new Error('Missing name input');
        setFieldValue(input, value);
      };
      const save = () => {
        const form = target.querySelector('form');
        if (!form) throw new Error('Missing editor form');
        form.dispatchEvent(
          new SubmitEvent('submit', { bubbles: true, cancelable: true })
        );
        flushSync();
      };
      await eventually(() => expect(button('Original')).toBeDefined());
      click('Original');
      if (destination === 'new-from-new') click(editor.create);
      rename('First save');
      const configuration = target.querySelector('textarea');
      if (configuration) {
        configuration.value = '{"words":["secret"]}';
        configuration.dispatchEvent(new Event('input', { bubbles: true }));
        flushSync();
      }
      save();
      await pending.promise;
      if (
        destination === 'new' ||
        destination === 'new-from-new' ||
        destination === 'save-error-new'
      )
        click(editor.create);
      if (destination === 'existing' || destination.startsWith('round-trip'))
        click('Other');
      if (
        destination.startsWith('same') ||
        destination.startsWith('round-trip')
      )
        click('Original');
      if (!destination.endsWith('untouched')) rename('Current edits');
      release.resolve();
      await eventually(() => expect(button(editor.save).disabled).toBe(false));
      expect(target.querySelector('input')?.value).toBe(
        destination.endsWith('untouched') ? 'Original' : 'Current edits'
      );
      if (destination.startsWith('save-error'))
        expect(target.querySelector('[role=alert]')?.textContent).toContain(
          'Save failed'
        );
      if (destination !== 'unchanged')
        expect(target.textContent).not.toMatch(/draft saved/i);
      save();
      await eventually(() => expect(writes).toHaveBeenCalledTimes(2));
      const [request, body] = writes.mock.calls[1];
      const newDestination =
        destination === 'new' ||
        destination === 'new-from-new' ||
        destination === 'save-error-new';
      const id = destination === 'existing' ? 'other' : 'original';
      expect(request.method).toBe(newDestination ? 'POST' : 'PUT');
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/${editor.path}${newDestination ? '' : `/${id}`}`
      );
      const reselected =
        destination.startsWith('same') || destination.startsWith('round-trip');
      expect(request.headers.get('If-Match')).toBe(
        newDestination
          ? null
          : `"${id}${destination === 'existing' || reselected || destination === 'save-error' ? '' : '-saved'}-etag"`
      );
      expect(body.name).toBe(
        destination.endsWith('untouched') ? 'Original' : 'Current edits'
      );
      await eventually(() => expect(button(editor.save).disabled).toBe(false));
      if (reselected) {
        expect(target.querySelector('[role=alert]')?.textContent).toContain(
          'Reload before saving'
        );
        expect(savedOriginal().name).toBe('First save');
        expect(target.querySelector('input')?.value).toBe(
          destination.endsWith('untouched') ? 'Original' : 'Current edits'
        );
        click('First save');
        expect(target.querySelector('input')?.value).toBe('First save');
        if (configuration)
          expect(target.querySelector('textarea')?.value).toContain('secret');
      }
      rename('Another edit');
      save();
      await eventually(() => expect(writes).toHaveBeenCalledTimes(3));
      expect(writes.mock.calls[2][0].headers.get('If-Match')).toBe(
        `"${newDestination ? 'created' : id}-saved-etag"`
      );
    }
  );
}

it('replaces an attachment atomically and retains its displayed revision when publication is rejected', async () => {
  const writes = vi.fn();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (request.method === 'PUT') {
        writes(
          request.method,
          path,
          request.headers.get('If-Match'),
          await request.json()
        );
        return json(
          { title: 'Incompatible policy revision', status: 422 },
          422
        );
      }
      if (request.method === 'DELETE') {
        writes(request.method);
        return new Response(null, { status: 204 });
      }
      if (path === '/api/v1/guardrail-attachments')
        return json({
          items: [
            {
              id: 'attachment',
              etag: 'version',
              scope: 'project',
              scope_id: 'project',
              policy_id: 'original',
              policy_revision_id: 'pinned',
              active_revision_id: 'next',
              order: 0
            }
          ]
        });
      if (path === '/api/v1/guardrail-policies')
        return json({
          items: [{ ...original, active_revision_id: 'next', retired_at: null }]
        });
      if (path === '/api/v1/projects')
        return json({ items: [{ id: 'project', name: 'Project' }] });
      return json({ items: [] });
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(GuardrailsPage, { tab: 'attachments' });
  const click = (name: string) => clickButton(target, name);
  await eventually(() =>
    expect(target.textContent).toContain('Attached revision pinned')
  );
  const select = target.querySelector('select');
  expect(select).not.toBeNull();
  if (!select) return;
  select.value = 'next';
  select.dispatchEvent(new Event('change', { bubbles: true }));
  flushSync();
  click('Replace attachment');
  click('Confirm replace attachment');
  await eventually(() =>
    expect(target.textContent).toContain('Incompatible policy revision')
  );
  expect(writes).toHaveBeenCalledExactlyOnceWith(
    'PUT',
    '/api/v1/guardrail-attachments/attachment',
    '"version"',
    { policy_revision_id: 'next' }
  );
  expect(target.textContent).toContain('Attached revision pinned');
});

it.each(['loaded', 'pending'])(
  'retires %s inspected policy data when replacing an attachment',
  async (inspection) => {
    const writes = vi.fn();
    const pending = Promise.withResolvers<Response>();
    const reads = vi.fn();
    const oldPolicy = { name: 'Previous policy contents' };
    serve(
      (request) => {
        const path = new URL(request.url).pathname;
        if (request.method === 'PUT') {
          writes();
          return json({});
        }
        if (path.includes('/revisions/')) {
          reads(path);
          return path.endsWith('/next')
            ? json({ name: 'Replacement policy contents' })
            : inspection === 'pending'
              ? pending.promise
              : json(oldPolicy);
        }
        if (path === '/api/v1/guardrail-attachments')
          return json({
            items: [
              {
                id: 'attachment',
                etag: writes.mock.calls.length ? 'replaced' : 'original',
                scope: 'project',
                scope_id: 'project',
                policy_id: 'original',
                order: 0,
                policy_revision_id: writes.mock.calls.length
                  ? 'next'
                  : 'pinned',
                active_revision_id: 'next'
              }
            ]
          });
        if (path === '/api/v1/guardrail-policies')
          return json({
            items: [
              { ...original, active_revision_id: 'next', retired_at: null }
            ]
          });
        return json({ items: [] });
      },
      ['read', 'configure']
    );
    const { target } = mountWithState(GuardrailsPage, { tab: 'attachments' });
    const click = (label: string) => clickButton(target, label);
    await eventually(() =>
      expect(target.textContent).toContain('Attached revision pinned')
    );
    click('Inspect pinned revision');
    await eventually(() => expect(reads).toHaveBeenCalledOnce());
    if (inspection === 'loaded')
      await eventually(() =>
        expect(target.textContent).toContain(oldPolicy.name)
      );
    const select = target.querySelector('select');
    expect(select).not.toBeNull();
    if (!select) return;
    select.value = 'next';
    select.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    click('Replace attachment');
    click('Confirm replace attachment');
    await eventually(() =>
      expect(target.textContent).toContain('Attached revision next')
    );
    pending.resolve(json(oldPolicy));
    await pending.promise;
    await eventually(() =>
      expect(
        [...target.querySelectorAll('button')].find(
          (item) => item.textContent?.trim() === 'Inspect pinned revision'
        )?.disabled
      ).toBe(false)
    );
    expect(target.textContent).not.toContain(oldPolicy.name);
    click('Inspect pinned revision');
    await eventually(() =>
      expect(target.textContent).toContain('Replacement policy contents')
    );
    expect(target.textContent).not.toContain(oldPolicy.name);
  }
);

it.each(
  (['checks', 'policies'] as const).flatMap((tab) =>
    ['other', 'new', 'selected'].map((destination) => ({ tab, destination }))
  )
)(
  'preserves the $destination $tab draft when retirement completes',
  async ({ tab, destination }) => {
    const retirement = Promise.withResolvers<Response>();
    const retired = vi.fn();
    const writes = vi.fn<(request: Request) => void>();
    const path = tab === 'checks' ? 'guardrails' : 'guardrail-policies';
    serve(
      (request) => {
        if (request.method === 'DELETE') {
          retired(new URL(request.url).pathname);
          return retirement.promise;
        }
        if (request.method !== 'GET') {
          writes(request);
          return json(other);
        }
        if (new URL(request.url).pathname === '/api/v1/projects')
          return json({ items: [{ id: 'project', name: 'Project' }] });
        if (new URL(request.url).pathname === `/api/v1/${path}`)
          return json({
            items: [
              {
                ...original,
                retired_at: retired.mock.calls.length ? 'retired' : null
              },
              other
            ]
          });
        return json({ items: [] });
      },
      ['read', 'configure']
    );
    onTestFinished(() =>
      retirement.resolve(new Response(null, { status: 204 }))
    );
    const { target } = mountWithState(GuardrailsPage, { tab });
    const button = (label: string) => findButton(target, label);
    const click = (label: string) => clickButton(target, label);
    await eventually(() => expect(button('Original')).toBeDefined());
    const kind = tab === 'checks' ? 'check' : 'policy';
    click(`Retire ${kind}`);
    click(`Confirm retire ${kind}`);
    await eventually(() =>
      expect(retired).toHaveBeenCalledExactlyOnceWith(
        `/api/v1/${path}/original`
      )
    );
    if (destination === 'selected') click('Original');
    if (destination === 'other') click('Other');
    if (destination === 'new') click(`New ${kind}`);
    const input = target.querySelector('form input');
    expect(input).toBeInstanceOf(HTMLInputElement);
    if (!(input instanceof HTMLInputElement)) return;
    input.value = 'Unsaved draft';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    retirement.resolve(new Response(null, { status: 204 }));
    await eventually(() =>
      expect(
        button('Original')?.closest('tr')?.textContent?.toLowerCase()
      ).not.toContain(`retire ${kind}`)
    );
    if (destination === 'selected') {
      expect(target.querySelector('form input')).toBeNull();
      return;
    }
    expect(target.querySelector<HTMLInputElement>('form input')?.value).toBe(
      'Unsaved draft'
    );
    target
      .querySelector('form')
      ?.dispatchEvent(
        new SubmitEvent('submit', { bubbles: true, cancelable: true })
      );
    await eventually(() => expect(writes).toHaveBeenCalledOnce());
    expect(writes.mock.calls[0][0].method).toBe(
      destination === 'new' ? 'POST' : 'PUT'
    );
    expect(new URL(writes.mock.calls[0][0].url).pathname).toBe(
      `/api/v1/${path}${destination === 'new' ? '' : '/other'}`
    );
  }
);

it('retries a failed policy save and activation with the same idempotency key', async () => {
  const keys = vi.fn<(key: string | null) => void>();
  const failed = new Set<string>();
  serve(
    async (request) => {
      const path = new URL(request.url).pathname;
      if (request.method === 'GET') {
        if (path === '/api/v1/projects')
          return json({ items: [{ id: 'project', name: 'Project' }] });
        if (path === '/api/v1/guardrail-attachments')
          return json({ items: [] });
        return json({ items: [original, other] });
      }
      keys(request.headers.get('Idempotency-Key'));
      const attempt = `${request.method} ${path}`;
      if (!failed.has(attempt)) {
        failed.add(attempt);
        return json({ title: 'Response lost. Try again.', status: 500 }, 500);
      }
      if (path.endsWith('/activate')) return json({});
      const body = (await request.json()) as { name: string };
      return json({
        ...original,
        ...body,
        revision: 2,
        revision_id: 'saved-revision',
        etag: 'saved-etag'
      });
    },
    ['read', 'configure']
  );
  const { target } = mountWithState(GuardrailsPage, { tab: 'policies' });
  const button = (name: string) => findButton(target, name);
  const click = (name: string) => clickButton(target, name);
  const save = () => {
    target
      .querySelector('form')
      ?.dispatchEvent(
        new SubmitEvent('submit', { bubbles: true, cancelable: true })
      );
    flushSync();
  };
  await eventually(() => expect(button('Original')).toBeDefined());
  click('Original');
  save();
  await eventually(() =>
    expect(target.querySelector('[role=alert]')?.textContent).toContain(
      'Response lost'
    )
  );
  save();
  await eventually(() => expect(keys).toHaveBeenCalledTimes(2));
  await eventually(() => expect(button('Activate revision')).toBeDefined());
  click('Activate revision');
  await eventually(() =>
    expect(target.querySelector('[role=alert]')?.textContent).toContain(
      'Response lost'
    )
  );
  click('Activate revision');
  await eventually(() => expect(keys).toHaveBeenCalledTimes(4));
  const [saveKey, saveRetryKey, activateKey, activateRetryKey] =
    keys.mock.calls.map((call) => call[0]);
  expect(saveKey).toBeTruthy();
  expect(saveRetryKey).toBe(saveKey);
  expect(activateKey).toBeTruthy();
  expect(activateKey).not.toBe(saveKey);
  expect(activateRetryKey).toBe(activateKey);
});
