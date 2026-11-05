import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '$lib/management/test/transport';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
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

afterEach(() => vi.unstubAllGlobals());

for (const editor of [
  {
    tab: 'Definitions',
    path: 'guardrails',
    create: 'New definition',
    save: 'Save draft'
  },
  {
    tab: 'Policies',
    path: 'guardrail-policies',
    create: 'New policy',
    save: 'Save policy draft'
  }
]) {
  it.each([
    'unchanged',
    'new',
    'existing',
    'same',
    'round-trip',
    'new-from-new'
  ])(
    `preserves the %s editor after a delayed ${editor.tab} save`,
    async (destination) => {
      const pending = Promise.withResolvers<void>();
      const release = Promise.withResolvers<void>();
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
            return json({ items: [original, other] });
          }
          const body = (await request.json()) as { name: string };
          writes(request, body);
          if (writes.mock.calls.length === 1) {
            pending.resolve();
            await release.promise;
          }
          const id =
            request.method === 'POST' ? 'created' : path.split('/').at(-1);
          return json({
            ...original,
            ...body,
            id,
            policy_id: id,
            etag: `${id}-saved-etag`,
            revision: 2
          });
        },
        ['configure']
      );
      const target = document.createElement('div');
      const setup = vi.fn(() =>
        mount(GuardrailsPage, { target, context: getAllContexts() })
      );
      const harness = mount(StateHarness, {
        target,
        props: { setup }
      });
      onTestFinished(async () => {
        release.resolve();
        await unmount(setup.mock.results[0].value);
        await unmount(harness);
      });
      const button = (name: string) => {
        const found = [...target.querySelectorAll('button')].find(
          (item) => item.textContent?.trim() === name
        );
        if (!found) throw new Error(`Missing button ${name}`);
        return found;
      };
      const click = (name: string) => {
        button(name).click();
        flushSync();
      };
      const rename = (value: string) => {
        const input = target.querySelector('input');
        if (!input) throw new Error('Missing name input');
        input.value = value;
        input.dispatchEvent(new Event('input', { bubbles: true }));
        flushSync();
      };
      const save = () => {
        const form = target.querySelector('form');
        if (!form) throw new Error('Missing editor form');
        form.dispatchEvent(
          new SubmitEvent('submit', { bubbles: true, cancelable: true })
        );
        flushSync();
      };
      flushSync();
      await vi.waitFor(() => expect(button('Original')).toBeDefined());
      click(editor.tab);
      click('Original');
      if (destination === 'new-from-new') click(editor.create);
      rename('First save');
      save();
      await pending.promise;
      if (destination === 'new' || destination === 'new-from-new')
        click(editor.create);
      if (destination === 'existing' || destination === 'round-trip')
        click('Other');
      if (destination === 'same' || destination === 'round-trip')
        click('Original');
      rename('Current edits');
      release.resolve();
      await vi.waitFor(() => expect(button(editor.save).disabled).toBe(false));
      expect(target.querySelector('input')?.value).toBe('Current edits');
      if (destination !== 'unchanged')
        expect(target.textContent).not.toMatch(/draft saved/i);
      save();
      await vi.waitFor(() => expect(writes).toHaveBeenCalledTimes(2));
      const [request, body] = writes.mock.calls[1];
      const newDestination =
        destination === 'new' || destination === 'new-from-new';
      const id = destination === 'existing' ? 'other' : 'original';
      expect(request.method).toBe(newDestination ? 'POST' : 'PUT');
      expect(new URL(request.url).pathname).toBe(
        `/api/v1/${editor.path}${newDestination ? '' : `/${id}`}`
      );
      expect(request.headers.get('If-Match')).toBe(
        newDestination
          ? null
          : `"${id}${destination === 'existing' ? '' : '-saved'}-etag"`
      );
      expect(body.name).toBe('Current edits');
      await vi.waitFor(() => expect(button(editor.save).disabled).toBe(false));
      rename('Another edit');
      save();
      await vi.waitFor(() => expect(writes).toHaveBeenCalledTimes(3));
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
  const target = document.createElement('div');
  const setup = vi.fn(() =>
    mount(GuardrailsPage, { target, context: getAllContexts() })
  );
  const harness = mount(StateHarness, { target, props: { setup } });
  onTestFinished(async () => {
    await unmount(setup.mock.results[0].value);
    await unmount(harness);
  });
  function click(name: string) {
    [...target.querySelectorAll('button')]
      .find((item) => item.textContent?.trim() === name)
      ?.click();
    flushSync();
  }
  flushSync();
  click('Attachments');
  await vi.waitFor(() =>
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
  await vi.waitFor(() =>
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
    const target = document.createElement('div');
    const setup = vi.fn(() =>
      mount(GuardrailsPage, { target, context: getAllContexts() })
    );
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    function click(label: string) {
      [...target.querySelectorAll('button')]
        .find((item) => item.textContent?.trim() === label)
        ?.click();
      flushSync();
    }
    flushSync();
    click('Attachments');
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Attached revision pinned')
    );
    click('Inspect pinned revision');
    await vi.waitFor(() => expect(reads).toHaveBeenCalledOnce());
    if (inspection === 'loaded')
      await vi.waitFor(() =>
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
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Attached revision next')
    );
    pending.resolve(json(oldPolicy));
    await pending.promise;
    await vi.waitFor(() =>
      expect(
        [...target.querySelectorAll('button')].find(
          (item) => item.textContent?.trim() === 'Inspect pinned revision'
        )?.disabled
      ).toBe(false)
    );
    expect(target.textContent).not.toContain(oldPolicy.name);
    click('Inspect pinned revision');
    await vi.waitFor(() =>
      expect(target.textContent).toContain('Replacement policy contents')
    );
    expect(target.textContent).not.toContain(oldPolicy.name);
  }
);

it.each(
  ['Definitions', 'Policies'].flatMap((tab) =>
    ['other', 'new', 'selected'].map((destination) => ({ tab, destination }))
  )
)(
  'preserves the $destination $tab draft when retirement completes',
  async ({ tab, destination }) => {
    const retirement = Promise.withResolvers<Response>();
    const retired = vi.fn();
    const writes = vi.fn<(request: Request) => void>();
    const path = tab === 'Definitions' ? 'guardrails' : 'guardrail-policies';
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
    const target = document.createElement('div');
    const setup = vi.fn(() =>
      mount(GuardrailsPage, { target, context: getAllContexts() })
    );
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      retirement.resolve(new Response(null, { status: 204 }));
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    const button = (label: string) =>
      [...target.querySelectorAll('button')].find(
        (item) => item.textContent?.trim() === label
      );
    const click = (label: string) => {
      button(label)?.click();
      flushSync();
    };
    flushSync();
    click(tab);
    await vi.waitFor(() => expect(button('Original')).toBeDefined());
    click('Original');
    const kind = tab === 'Definitions' ? 'definition' : 'policy';
    click(`Retire ${kind}`);
    click(`Confirm retire ${kind}`);
    await vi.waitFor(() =>
      expect(retired).toHaveBeenCalledExactlyOnceWith(
        `/api/v1/${path}/original`
      )
    );
    if (destination === 'other') click('Other');
    if (destination === 'new') click(`New ${kind}`);
    const input = target.querySelector('form input');
    expect(input).toBeInstanceOf(HTMLInputElement);
    if (!(input instanceof HTMLInputElement)) return;
    input.value = 'Unsaved draft';
    input.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    retirement.resolve(new Response(null, { status: 204 }));
    await vi.waitFor(() =>
      expect(
        button('Original')?.closest('li')?.textContent?.toLowerCase()
      ).not.toContain(`retire ${kind}`)
    );
    expect(target.querySelector<HTMLInputElement>('form input')?.value).toBe(
      destination === 'selected' ? '' : 'Unsaved draft'
    );
    if (destination === 'selected') return;
    target
      .querySelector('form')
      ?.dispatchEvent(
        new SubmitEvent('submit', { bubbles: true, cancelable: true })
      );
    await vi.waitFor(() => expect(writes).toHaveBeenCalledOnce());
    expect(writes.mock.calls[0][0].method).toBe(
      destination === 'new' ? 'POST' : 'PUT'
    );
    expect(new URL(writes.mock.calls[0][0].url).pathname).toBe(
      `/api/v1/${path}${destination === 'new' ? '' : '/other'}`
    );
  }
);
