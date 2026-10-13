import { flushSync, mount, unmount } from 'svelte';
import { afterEach, expect, it } from 'vitest';
import { providerEditValues } from './providerEditor';
import type { Provider } from './api/providers';
import ProviderCommitmentEditor from './ProviderCommitmentEditor.svelte';
import type { ProviderKindCapability } from './api/models';
import { stringifyNativeJSON } from '$lib/json/nativeJson';

const base = {
  configuration: {
    kind: 'openai',
    auth_mode: 'api_key',
    endpoint: 'https://provider.example/v1',
    options: { vendor_id: null }
  }
} as unknown as Provider;

const spec = {
  kind: 'openai',
  label: 'OpenAI',
  description: '',
  auth_modes: [],
  default_auth_mode: 'api_key',
  fields: [],
  presets: []
} as unknown as ProviderKindCapability;

let host: HTMLElement | undefined;
let component: ReturnType<typeof mount> | undefined;

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  host?.remove();
  host = undefined;
});

function establish(provider: Provider = base) {
  host = document.createElement('div');
  document.body.append(host);
  const values = providerEditValues(provider, spec);
  component = mount(ProviderCommitmentEditor, {
    target: host,
    props: { values, idPrefix: 'test' }
  });
  flushSync();
  return values;
}

function field(id: string) {
  const element = host!.querySelector<HTMLInputElement | HTMLSelectElement>(
    `#${id}`
  );
  if (!element) throw new Error(`missing ${id}`);
  return element;
}

function edit(id: string, value: string) {
  const element = field(id);
  element.value = value;
  element.dispatchEvent(
    new Event(element instanceof HTMLSelectElement ? 'change' : 'input', {
      bubbles: true
    })
  );
  flushSync();
}

it('writes and clears the commitment member on the configuration document', () => {
  const values = establish();
  edit('test-commitment-amount', '300.50');
  edit('test-commitment-period', 'month');
  let wire = stringifyNativeJSON(values.document!.configuration());
  expect(wire).toContain('"commitment":{"amount":"300.50","period":"month"}');
  edit('test-commitment-amount', '   ');
  wire = stringifyNativeJSON(values.document!.configuration());
  expect(wire).not.toContain('"commitment"');
});

it('reflects an existing commitment', () => {
  const provider = {
    ...base,
    configuration: {
      ...base.configuration,
      options: {
        vendor_id: null,
        commitment: { amount: '100', period: 'hour' }
      }
    }
  } as unknown as Provider;
  establish(provider);
  expect(field('test-commitment-amount').value).toBe('100');
  expect(field('test-commitment-period').value).toBe('hour');
});
