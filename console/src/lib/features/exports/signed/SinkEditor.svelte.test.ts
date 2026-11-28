import SinksPage from './SinksPage.svelte';
import { expect, it, vi } from 'vitest';
import { flushSync } from 'svelte';
import { json, serve } from '#lib/session/test/transport.js';
import type { ManagementOperation } from '#lib/management/requirements.js';
import { eventually } from '#test/eventually.js';
import {
  findButton,
  findControl,
  mountWithState,
  reportedError,
  setFieldValue,
  submitForm
} from '#test/mount.js';
import SinkEditor from './SinkEditor.svelte';
import type { Sink } from './api';

it('preserves the signing credential when editing other fields and supports explicit removal', async () => {
  const writes = vi.fn<(body: unknown, etag: string | null) => void>();
  const saved = vi.fn();
  const current: Sink = {
    id: 'sink',
    etag: 'observed-version',
    name: 'Signed facts',
    type: 'https',
    project_id: null,
    destination: 'https://export.example.test/facts',
    streams: ['requests'],
    enabled: false,
    has_credential: true,
    retired_at: null,
    delivery: { delivered: 0, failed: 0, expired: 0, last_delivered_at: null }
  };
  serve(
    async (request) => {
      writes(await request.json(), request.headers.get('If-Match'));
      return json({ ...current, etag: 'saved-version' });
    },
    ['read', 'settings']
  );
  const { target } = mountWithState(SinkEditor, {
    current,
    projects: [],
    saved
  });
  const name = target.querySelectorAll('input')[0];
  setFieldValue(name, 'Renamed facts');
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => expect(saved).toHaveBeenCalledOnce());
  expect(writes.mock.calls[0][0]).toMatchObject({ name: 'Renamed facts' });
  expect(writes.mock.calls[0][0]).not.toHaveProperty('credential');
  expect(writes.mock.calls[0][1]).toBe('"observed-version"');
  const remove = [...target.querySelectorAll('label')]
    .find((label) =>
      label.textContent
        ?.replace(/\s+/g, ' ')
        .includes('Remove signing credential')
    )
    ?.querySelector('input');
  expect(remove).toBeDefined();
  if (remove) {
    remove.checked = true;
    remove.dispatchEvent(new Event('change', { bubbles: true }));
  }
  flushSync();
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  await eventually(() => expect(saved).toHaveBeenCalledTimes(2));
  expect(writes.mock.calls[1][0]).toHaveProperty('credential', null);
  expect(writes.mock.calls[1][1]).toBe('"saved-version"');
});

it.each(['refresh', 'conflict'])(
  'preserves export edits and offers reload after %s',
  async (trigger) => {
    const current: Sink = {
      id: 'sink',
      etag: 'before',
      name: 'Original',
      type: 'https',
      project_id: null,
      destination: 'https://export.example.test/facts',
      streams: ['requests'],
      enabled: false,
      has_credential: false,
      retired_at: null,
      delivery: { delivered: 0, failed: 0, expired: 0, last_delivered_at: null }
    };
    const load = vi.fn(() => json({ items: [current] }));
    const writes = vi.fn<(request: Request) => void>();
    serve(
      (request) => {
        if (request.method === 'PUT') {
          writes(request);
          return json(
            {
              title: 'Changed elsewhere',
              status: 412,
              type: 'https://openllmproxy.dev/problems/etag_mismatch'
            },
            412
          );
        }
        return new URL(request.url).pathname.endsWith('/projects')
          ? json({ items: [] })
          : load();
      },
      ['read', 'settings']
    );
    const { target } = mountWithState(SinksPage);
    const button = (name: string) => findButton(target, name);
    await eventually(() => expect(button('Edit')).toBeDefined());
    button('Edit')?.click();
    flushSync();
    const name = target.querySelector('form input') as HTMLInputElement;
    name.value = 'My draft';
    name.dispatchEvent(new Event('input', { bubbles: true }));
    flushSync();
    load.mockImplementation(() =>
      json({ items: [{ ...current, name: 'Other operator', etag: 'remote' }] })
    );
    if (trigger === 'refresh') {
      button('Refresh delivery counts')?.click();
      await eventually(() =>
        expect(target.querySelector('tbody strong')?.textContent).toBe(
          'Other operator'
        )
      );
    }
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'My draft'
    );
    target
      .querySelector('form')
      ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
    await eventually(() => expect(writes).toHaveBeenCalledOnce());
    expect(writes.mock.calls[0][0].headers.get('If-Match')).toBe('"before"');
    await eventually(() =>
      expect(target.textContent).toContain('Changed elsewhere')
    );
    await eventually(() => expect(button('Reload')?.disabled).toBe(false));
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'My draft'
    );
    button('Reload')?.click();
    flushSync();
    expect((target.querySelector('form input') as HTMLInputElement).value).toBe(
      'Other operator'
    );
  }
);

const sink: Sink = {
  id: 'sink',
  etag: 'version',
  name: 'Signed facts',
  type: 'https',
  project_id: null,
  destination: 'https://export.example.test/facts',
  streams: ['requests'],
  enabled: true,
  has_credential: false,
  retired_at: null,
  delivery: { delivered: 0, failed: 0, expired: 0, last_delivered_at: null }
};

function mountSinks(operations: ManagementOperation[], listed: Sink = sink) {
  serve(
    (request) =>
      json(
        new URL(request.url).pathname.endsWith('/projects')
          ? { items: [] }
          : { items: [listed] }
      ),
    operations
  );
  const { target } = mountWithState(SinksPage, {}, { attach: true });
  const button = (name: string) => findButton(target, name);
  return { target, button };
}

it('offers no create or edit controls to a member who cannot write exports', async () => {
  const { target, button } = mountSinks(['read']);
  await eventually(() =>
    expect(target.querySelector('tbody strong')?.textContent).toBe(
      'Signed facts'
    )
  );

  expect(button('New export')).toBeUndefined();
  expect(button('Edit')).toBeUndefined();
  expect(target.querySelector('form')).toBeNull();
});

it('keeps an edited draft when the operator declines to discard it', async () => {
  const decline = vi.fn(() => false);
  vi.stubGlobal('confirm', decline);
  const { target, button } = mountSinks(['read', 'settings']);
  await eventually(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  flushSync();
  const name = () => target.querySelector('form input') as HTMLInputElement;
  name().value = 'My draft';
  name().dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  target
    .querySelector<HTMLButtonElement>('dialog [aria-label="Close"]')
    ?.click();
  flushSync();

  expect(decline).toHaveBeenCalledOnce();
  expect(name().value).toBe('My draft');
});

it('clears a new draft when the operator accepts discarding it', async () => {
  vi.stubGlobal(
    'confirm',
    vi.fn(() => true)
  );
  const { target, button } = mountSinks(['read', 'settings']);
  await eventually(() => expect(button('New export')).toBeDefined());
  button('New export')?.click();
  flushSync();
  const name = () => target.querySelector('form input') as HTMLInputElement;
  name().value = 'Abandoned draft';
  name().dispatchEvent(new Event('input', { bubbles: true }));
  flushSync();

  target
    .querySelector<HTMLButtonElement>('dialog [aria-label="Close"]')
    ?.click();
  flushSync();
  expect(target.querySelector('form')).toBeNull();
  button('New export')?.click();
  flushSync();

  expect(name().value).toBe('');
});

function mountEditor(
  respond: (body: unknown) => Response,
  current: Sink | null = sink
) {
  const writes = vi.fn<(body: unknown) => void>();
  serve(
    async (request) => {
      const body: unknown = await request.json();
      writes(body);
      return respond(body);
    },
    ['read', 'settings']
  );
  const saved = vi.fn();
  const { target } = mountWithState(
    SinkEditor,
    { current, projects: [{ id: 'search', name: 'Search' }], saved },
    { attach: true }
  );
  return { target, writes, saved };
}

it('reports a rejected draft on its inputs without writing, then saves the corrected one', async () => {
  const { target, writes, saved } = mountEditor(() => json(sink));
  const name = findControl(target, 'Name');
  const destination = findControl(target, 'HTTPS destination');
  const requests = findControl(target, 'requests');
  setFieldValue(name, '   ');
  setFieldValue(destination, 'http://export.example.test/facts');
  requests.click();
  submitForm(target);

  expect(reportedError(name)).toBe('Enter a name.');
  expect(reportedError(destination)).toBe('Enter an HTTPS URL.');
  expect(reportedError(requests)).toBe('Choose between one and four streams.');
  await eventually(() => expect(document.activeElement).toBe(name));
  expect(writes).not.toHaveBeenCalled();

  setFieldValue(name, ' Corrected ');
  setFieldValue(destination, 'https://export.example.test/corrected');
  requests.click();
  submitForm(target);

  await eventually(() => expect(saved).toHaveBeenCalledOnce());
  expect(writes.mock.calls).toMatchObject([
    [
      {
        name: 'Corrected',
        destination: 'https://export.example.test/corrected',
        streams: ['requests']
      }
    ]
  ]);
  expect(target.querySelector('[aria-invalid="true"]')).toBeNull();
});

it('lets the operator remove audit events from a project export', async () => {
  const { target, writes } = mountEditor(() => json(sink), null);
  setFieldValue(findControl(target, 'Name'), 'Search facts');
  setFieldValue(
    findControl(target, 'HTTPS destination'),
    'https://export.example.test/search'
  );
  const audit = findControl(target, 'audit');
  audit.click();
  const scope = target.querySelector('select') as HTMLSelectElement;
  setFieldValue(scope, 'search');
  submitForm(target);

  expect(reportedError(audit)).toBe(
    'Audit events require an installation sink.'
  );
  expect(audit.disabled).toBe(false);

  audit.click();
  flushSync();
  expect(audit.disabled).toBe(true);
  submitForm(target);

  await eventually(() => expect(writes).toHaveBeenCalledOnce());
  expect(writes.mock.calls[0][0]).toMatchObject({
    project_id: 'search',
    streams: ['requests']
  });
});

it('shows a refused field on its input and any other refusal above the form', async () => {
  const refuse = (field: string, message: string) =>
    json(
      {
        title: 'Validation failed',
        status: 422,
        detail: message,
        errors: { [field]: [{ code: 'validation_failed', message }] }
      },
      422
    );
  const respond = vi
    .fn<() => Response>()
    .mockReturnValueOnce(refuse('destination', 'The endpoint is refused.'))
    .mockReturnValueOnce(refuse('sinks', 'Too many exports.'));
  const { target, saved } = mountEditor(respond);
  const destination = findControl(target, 'HTTPS destination');
  setFieldValue(destination, 'https://export.example.test/facts?team=a');
  submitForm(target);

  await eventually(() =>
    expect(reportedError(destination)).toBe('The endpoint is refused.')
  );
  await eventually(() => expect(document.activeElement).toBe(destination));
  expect(target.querySelector('[role="alert"]')).toBeNull();
  expect(destination.value).toBe('https://export.example.test/facts?team=a');

  submitForm(target);

  const summary = () => target.querySelector('[role="alert"]');
  await eventually(() =>
    expect(summary()?.textContent?.trim()).toBe('Too many exports.')
  );
  await eventually(() => expect(document.activeElement).toBe(summary()));
  expect(reportedError(destination)).toBeUndefined();
  expect(saved).not.toHaveBeenCalled();
});

it('asks to discard a changed stream list but not one switched back', async () => {
  const ask = vi.fn(() => false);
  vi.stubGlobal('confirm', ask);
  const { target, button } = mountSinks(['read', 'settings'], {
    ...sink,
    streams: ['runs', 'requests']
  });
  await eventually(() => expect(button('Edit')).toBeDefined());
  button('Edit')?.click();
  flushSync();
  const close = () => {
    target
      .querySelector<HTMLButtonElement>('dialog [aria-label="Close"]')
      ?.click();
    flushSync();
  };

  findControl(target, 'requests').click();
  flushSync();
  close();
  expect(ask).toHaveBeenCalledOnce();

  findControl(target, 'requests').click();
  flushSync();
  close();
  expect(ask).toHaveBeenCalledOnce();
  expect(target.querySelector('form')).toBeNull();
});
