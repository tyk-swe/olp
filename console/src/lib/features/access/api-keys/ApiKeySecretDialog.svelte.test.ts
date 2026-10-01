// @vitest-environment jsdom
import { flushSync, mount, unmount } from 'svelte';
import { QueryClient } from '@tanstack/svelte-query';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { ApiKeySecret } from '$lib/features/access/api-keys/api';
import { testSdkRequest } from '$lib/features/access/api-keys/sdkExamples';
import ApiKeySecretProbe from './test/ApiKeySecretProbe.svelte';

vi.mock('$lib/features/access/session/serviceCapabilities.svelte', () => ({
  useServiceCapabilities: () => ({ gatewayAvailable: true })
}));
vi.mock('$lib/features/access/api-keys/sdkExamples', async (original) => ({
  ...(await original<
    typeof import('$lib/features/access/api-keys/sdkExamples')
  >()),
  testSdkRequest: vi.fn()
}));

const secret = { secret: 'olp_test_secret' } as ApiKeySecret;
const dialogPrototype = HTMLDialogElement.prototype as unknown as Record<
  string,
  unknown
>;
const originalShowModal = Object.getOwnPropertyDescriptor(
  HTMLDialogElement.prototype,
  'showModal'
);

let host: HTMLElement;
let client: QueryClient;
let component: ReturnType<typeof mount> | undefined;

beforeEach(() => {
  vi.resetAllMocks();
  dialogPrototype.showModal = function showModal(this: HTMLDialogElement) {
    this.open = true;
  };
  host = document.createElement('div');
  document.body.append(host);
  client = new QueryClient({
    defaultOptions: { queries: { retry: false, staleTime: Infinity } }
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  component = undefined;
  client.clear();
  host.remove();
  if (originalShowModal)
    Object.defineProperty(
      HTMLDialogElement.prototype,
      'showModal',
      originalShowModal
    );
  else delete dialogPrototype.showModal;
});

function render() {
  component = mount(ApiKeySecretProbe, {
    target: host,
    props: { client, secret }
  });
  flushSync();
}

async function settle() {
  await new Promise((resolve) => setTimeout(resolve, 0));
  await new Promise((resolve) => setTimeout(resolve, 0));
  flushSync();
}

function button(text: string) {
  return [...host.querySelectorAll('button')].find(
    (candidate) => candidate.textContent?.trim() === text
  )!;
}

it('reports a passing connection test for the tested SDK', async () => {
  vi.mocked(testSdkRequest).mockResolvedValue(undefined);
  render();
  button('Run connection test').click();
  await settle();
  expect(testSdkRequest).toHaveBeenCalledWith('openai', secret.secret, 'chat');
  expect(host.querySelector('[role="status"]')?.textContent).toContain(
    'OpenAI request succeeded through route chat.'
  );
});

it('drops a connection result that settles after the SDK tab changed', async () => {
  const pending = Promise.withResolvers<void>();
  vi.mocked(testSdkRequest).mockReturnValue(pending.promise);
  render();
  button('Run connection test').click();
  flushSync();
  button('Gemini TS').click();
  flushSync();
  pending.resolve();
  await settle();
  expect(host.textContent).not.toContain('request succeeded');
  expect(button('Run connection test')).toBeTruthy();
});
