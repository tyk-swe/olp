import { useQueryClient, type QueryClient } from '@tanstack/svelte-query';
import {
  flushSync,
  getAllContexts,
  mount,
  unmount,
  type Component
} from 'svelte';
import { onTestFinished } from 'vitest';
import StateHarness from './StateHarness.svelte';

export type MountOptions = {
  readonly attach?: boolean;
  readonly configureClient?: (client: QueryClient) => void;
};

export type MountedComponent = {
  readonly target: HTMLDivElement;
  readonly client: QueryClient;
  readonly unmount: () => Promise<void>;
};

export type MountedState<T> = {
  readonly value: T;
  readonly target: HTMLDivElement;
  readonly client: QueryClient;
  readonly unmount: () => Promise<void>;
};

export function mountState<T>(
  setup: (context: { target: HTMLDivElement; client: QueryClient }) => T,
  options: MountOptions = {}
): MountedState<T> {
  const target = document.createElement('div');
  if (options.attach) document.body.append(target);
  let value!: T;
  let client!: QueryClient;
  const harness = mount(StateHarness, {
    target,
    props: {
      setup: () => {
        client = useQueryClient();
        options.configureClient?.(client);
        value = setup({ target, client });
      }
    }
  });
  let disposed = false;
  const dispose = async () => {
    if (disposed) return;
    disposed = true;
    await unmount(harness);
    target.remove();
  };
  onTestFinished(dispose);
  flushSync();
  return { value, target, client, unmount: dispose };
}

export function mountWithState<
  Props extends Record<string, unknown>,
  Exports extends Record<string, unknown>
>(
  component: Component<Props, Exports>,
  ...args: Record<string, never> extends Props
    ? [props?: NoInfer<Props>, options?: MountOptions]
    : [props: NoInfer<Props>, options?: MountOptions]
): MountedComponent {
  const [props, options = {}] = args;
  const target = document.createElement('div');
  if (options.attach) document.body.append(target);
  let child!: Exports;
  let client!: QueryClient;
  const harness = mount(StateHarness, {
    target,
    props: {
      setup: () => {
        client = useQueryClient();
        options.configureClient?.(client);
        child = mount(component, {
          target,
          props: (props ?? {}) as Props,
          context: getAllContexts()
        });
      }
    }
  });
  let disposed = false;
  const dispose = async () => {
    if (disposed) return;
    disposed = true;
    await unmount(child);
    await unmount(harness);
    target.remove();
  };
  onTestFinished(dispose);
  flushSync();
  return { target, client, unmount: dispose };
}

export function findButton(
  target: ParentNode,
  label: string
): HTMLButtonElement | undefined {
  return [...target.querySelectorAll('button')].find(
    (item) => item.textContent?.trim() === label
  );
}

export function clickButton(target: ParentNode, label: string): void {
  const button = findButton(target, label);
  if (!button) throw new Error(`Missing button ${label}`);
  button.click();
  flushSync();
}

export function setFieldValue(
  element: HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement,
  value: string,
  event: 'input' | 'change' = element instanceof HTMLSelectElement
    ? 'change'
    : 'input'
): void {
  element.value = value;
  element.dispatchEvent(new Event(event, { bubbles: true }));
  flushSync();
}

export function findControl(
  target: ParentNode,
  label: string
): HTMLInputElement {
  const control = [...target.querySelectorAll('label')].find(
    (item) => item.textContent?.trim() === label
  )?.control;
  if (!(control instanceof HTMLInputElement))
    throw new Error(`Missing input ${label}`);
  return control;
}

export function reportedError(control: Element): string | undefined {
  if (control.getAttribute('aria-invalid') !== 'true') return undefined;
  const id = control.getAttribute('aria-describedby');
  return control.ownerDocument.getElementById(id ?? '')?.textContent?.trim();
}

export function submitForm(target: ParentNode): void {
  target
    .querySelector('form')
    ?.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
  flushSync();
}
