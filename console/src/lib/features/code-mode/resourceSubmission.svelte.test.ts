import { afterEach, describe, expect, it, onTestFinished, vi } from 'vitest';
import { flushSync, getAllContexts, mount, unmount } from 'svelte';
import { useQueryClient } from '@tanstack/svelte-query';
import { ApiProblem } from '#lib/management/http.js';
import { json, serve } from '#lib/management/test/transport.js';
import StateHarness from '../../../../tests/unit/support/StateHarness.svelte';
import AccountEditor from './AccountEditor.svelte';
import { useResourceSubmission } from './resourceSubmission.svelte';
import { confirmAction } from '#lib/components/confirm.js';

vi.mock('#lib/components/confirm.js', () => ({ confirmAction: vi.fn() }));

vi.mock('$app/navigation', () => ({ beforeNavigate: vi.fn() }));

type Input = { name: string };
type Options = Parameters<typeof useResourceSubmission<Input>>[0];

function renderSubmission(overrides: Partial<Options> = {}) {
  const persist = vi.fn<Options['persist']>(async () => undefined);
  const onDraftChange = vi.fn<Options['onDraftChange']>();
  const onSaved = vi.fn<Options['onSaved']>(async () => undefined);
  const onCancel = vi.fn<Options['onCancel']>();
  const options: Options = {
    allowed: () => true,
    creating: () => true,
    build: () => ({ ok: true, value: { name: 'Example' } }),
    persist,
    onDraftChange,
    onSaved,
    onCancel,
    ...overrides
  };
  const ready = vi.fn(
    (state: ReturnType<typeof useResourceSubmission<Input>>) => state
  );
  const component = mount(StateHarness, {
    target: document.createElement('div'),
    props: { setup: () => ready(useResourceSubmission(options)) }
  });
  onTestFinished(() => unmount(component));
  flushSync();
  return {
    state: ready.mock.results[0].value,
    persist,
    onDraftChange,
    onSaved,
    onCancel
  };
}

function submit(): SubmitEvent {
  return new SubmitEvent('submit', { cancelable: true });
}

afterEach(() => vi.unstubAllGlobals());

describe('resource submissions', () => {
  it('shows failed account provider lookups and allows cancelling without enabling save', async () => {
    const lookup = Promise.withResolvers<Response>();
    serve((request) => {
      const path = new URL(request.url).pathname;
      if (path === '/api/v1/providers')
        return json({
          items: [{ id: 'provider', project_id: 'project', name: 'Fixture' }]
        });
      if (path === '/api/v1/providers/provider') return lookup.promise;
      if (path === '/api/v1/providers/provider/credentials')
        return json({ items: [] });
      if (path === '/api/v1/provider-kinds') return json({ items: [] });
      return json({ title: 'Unexpected request', status: 404 }, 404);
    });
    vi.stubGlobal(
      'confirm',
      vi.fn(() => true)
    );
    const onCancel = vi.fn();
    const target = document.createElement('div');
    const setup = vi.fn(() => {
      useQueryClient().setDefaultOptions({ queries: { retry: false } });
      return mount(AccountEditor, {
        target,
        context: getAllContexts(),
        props: {
          projectId: 'project',
          allowed: true,
          onDraftChange: vi.fn(),
          onSaved: vi.fn(async () => undefined),
          onCancel
        }
      });
    });
    const harness = mount(StateHarness, { target, props: { setup } });
    onTestFinished(async () => {
      await unmount(setup.mock.results[0].value);
      await unmount(harness);
    });
    flushSync();
    const provider = target.querySelector<HTMLSelectElement>('#code-provider');
    await vi.waitFor(() => expect(provider?.options.length).toBe(2));
    if (!provider) throw new Error('Provider selection is missing');
    provider.value = 'provider';
    provider.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
    expect(
      target.querySelector<HTMLButtonElement>('[type="submit"]')?.disabled
    ).toBe(true);

    lookup.resolve(json({ title: 'Provider lookup failed', status: 503 }, 503));
    await vi.waitFor(() =>
      expect(target.textContent).toMatch(/Cancel\s+and reopen to retry\./)
    );
    expect(
      target.querySelector<HTMLButtonElement>('[type="submit"]')?.disabled
    ).toBe(true);
    const cancel = [...target.querySelectorAll('button')].find(
      (button) => button.textContent?.trim() === 'Cancel'
    );
    expect(cancel?.disabled).toBe(false);
    cancel?.click();
    expect(onCancel).toHaveBeenCalledOnce();
  });

  it.each([{ allowed: () => false }, { blocked: () => true }])(
    'does not submit when permission or another operation blocks it',
    async (options) => {
      const { state, persist } = renderSubmission(options);
      const event = submit();

      await state.save(event);

      expect(event.defaultPrevented).toBe(true);
      expect(persist).not.toHaveBeenCalled();
    }
  );

  it('saves only once while a request is in flight and clears the draft after success', async () => {
    const pending = Promise.withResolvers<void>();
    const persist = vi.fn<Options['persist']>(() => pending.promise);
    const { state, onDraftChange, onSaved } = renderSubmission({ persist });
    state.markDirty();
    flushSync();
    expect(onDraftChange).toHaveBeenLastCalledWith(true);

    const saving = state.save(submit());
    expect(state.busy).toBe(true);
    await state.save(submit());
    expect(persist).toHaveBeenCalledOnce();
    pending.resolve();
    await saving;
    flushSync();

    expect(state.busy).toBe(false);
    expect(onSaved).toHaveBeenCalledOnce();
    expect(onDraftChange).toHaveBeenLastCalledWith(false);
  });

  it('retries an unanswered create with the same request identity', async () => {
    const persist = vi.fn<Options['persist']>(async () => undefined);
    persist.mockRejectedValueOnce(new TypeError('Network error'));
    const { state, onSaved } = renderSubmission({ persist });

    await state.save(submit());
    expect(state.submitted).toBe(true);
    expect(state.error).toBeTruthy();
    expect(onSaved).not.toHaveBeenCalled();
    await state.save(submit());

    expect(persist).toHaveBeenCalledTimes(2);
    expect(persist.mock.calls[1]).toEqual(persist.mock.calls[0]);
    expect(state.submitted).toBe(false);
    expect(state.error).toBe('');
    expect(onSaved).toHaveBeenCalledOnce();
  });

  it('shows a definite rejection without locking the create request', async () => {
    const persist = vi.fn<Options['persist']>(async () => undefined);
    persist.mockRejectedValueOnce(
      new ApiProblem({ title: 'Invalid resource', status: 400 })
    );
    const { state, onSaved } = renderSubmission({ persist });

    await state.save(submit());

    expect(state.error).toBe('Invalid resource');
    expect(state.submitted).toBe(false);
    expect(onSaved).not.toHaveBeenCalled();
  });

  it('asks before discarding edits and keeps them when the user declines', () => {
    const confirm = vi
      .fn()
      .mockReturnValueOnce(false)
      .mockReturnValueOnce(true);
    vi.stubGlobal('confirm', confirm);
    const { state, onCancel } = renderSubmission();
    state.markDirty();

    state.cancel();
    expect(onCancel).not.toHaveBeenCalled();
    state.cancel();
    expect(onCancel).toHaveBeenCalledOnce();
    expect(confirm).toHaveBeenCalledTimes(2);
  });

  it('starts a different request only after confirming an uncertain submission', async () => {
    const persist = vi.fn<Options['persist']>(async () => undefined);
    persist.mockRejectedValueOnce(new TypeError('Network error'));
    vi.mocked(confirmAction)
      .mockResolvedValueOnce(false)
      .mockResolvedValueOnce(true);
    const { state } = renderSubmission({ persist });
    await state.save(submit());

    await state.editRequest();
    expect(state.submitted).toBe(true);
    await state.editRequest();
    expect(state.submitted).toBe(false);
    expect(state.error).toBe('');
    await state.save(submit());

    expect(persist.mock.calls[1][1]).not.toBe(persist.mock.calls[0][1]);
  });

  it('reports validation failures as values without dispatching a request', async () => {
    const { state, persist } = renderSubmission({
      build: () => ({ ok: false, message: 'Enter a valid route slug.' })
    });

    await state.save(submit());

    expect(state.error).toBe('Enter a valid route slug.');
    expect(state.submitted).toBe(false);
    expect(persist).not.toHaveBeenCalled();
  });

  it('ignores a completed request after the editor is closed', async () => {
    const pending = Promise.withResolvers<void>();
    const onSaved = vi.fn<Options['onSaved']>(async () => undefined);
    const onDraftChange = vi.fn<Options['onDraftChange']>();
    const ready = vi.fn(
      (state: ReturnType<typeof useResourceSubmission<Input>>) => state
    );
    const component = mount(StateHarness, {
      target: document.createElement('div'),
      props: {
        setup: () =>
          ready(
            useResourceSubmission({
              allowed: () => true,
              creating: () => true,
              build: () => ({ ok: true, value: { name: 'Example' } }),
              persist: () => pending.promise,
              onDraftChange,
              onSaved,
              onCancel: () => undefined
            })
          )
      }
    });
    flushSync();
    const saving = ready.mock.results[0].value.save(submit());
    await unmount(component);
    pending.resolve();
    await saving;

    expect(onSaved).not.toHaveBeenCalled();
    expect(onDraftChange).toHaveBeenLastCalledWith(false);
  });
});
