import { afterEach, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { json, serve } from '$lib/api/test/transport';
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
      await vi.waitFor(() => expect(button(editor.save).disabled).toBe(false));
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
      await vi.waitFor(() => expect(writes).toHaveBeenCalledTimes(2));
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
      await vi.waitFor(() => expect(button(editor.save).disabled).toBe(false));
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
      await vi.waitFor(() => expect(writes).toHaveBeenCalledTimes(3));
      expect(writes.mock.calls[2][0].headers.get('If-Match')).toBe(
        `"${newDestination ? 'created' : id}-saved-etag"`
      );
    }
  );
}
