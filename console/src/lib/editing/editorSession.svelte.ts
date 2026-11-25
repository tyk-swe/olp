import { onDestroy } from 'svelte';
import { authLifecycle } from '#lib/session/lifecycle.js';
import { MutationIntent } from './mutationIntent';
import { usePrincipalLifetime } from './mutationIntent.svelte';

export type MutationIntentGroup<Names extends readonly string[]> = {
  readonly [Name in Names[number]]: MutationIntent;
} & {
  resetAll(): void;
  reset(...names: Names[number][]): void;
};

export function useMutationIntentGroup<const Names extends readonly string[]>(
  names: Names
): MutationIntentGroup<Names> {
  const entries = names.map((name) => [name, new MutationIntent()] as const);
  const intents = Object.fromEntries(entries) as Record<
    Names[number],
    MutationIntent
  >;
  const resetAll = () => {
    for (const [, intent] of entries) intent.reset();
  };
  const reset = (...selected: Names[number][]) => {
    for (const name of selected) intents[name].reset();
  };
  usePrincipalLifetime(resetAll);
  return Object.assign(intents, {
    resetAll,
    reset
  }) as MutationIntentGroup<Names>;
}

export type CapturedOwnership = {
  isCurrent: () => boolean;
  samePrincipal: () => boolean;
};

/** Tracks principal changes, resource switches, and unmount disposal for an
 * editor's asynchronous operations without repeating epoch boilerplate. */
export class EditorSession {
  private epoch = 0;
  private disposed = false;
  private readonly capturePrincipal: () => () => boolean;

  constructor(onPrincipalReset: () => void) {
    this.capturePrincipal = usePrincipalLifetime(() => {
      this.epoch += 1;
      onPrincipalReset();
    });
    onDestroy(() => {
      this.disposed = true;
      this.epoch += 1;
    });
  }

  get isDisposed(): boolean {
    return this.disposed;
  }

  advance(): void {
    this.epoch += 1;
  }

  capture(extraCurrent?: () => boolean): CapturedOwnership {
    const principalCurrent = this.capturePrincipal();
    const capturedEpoch = this.epoch;
    const user = authLifecycle.snapshot().user;
    const partition = user ? authLifecycle.principalPartition(user) : '';
    const samePrincipal = () => {
      const current = authLifecycle.snapshot().user;
      return (
        partition === (current ? authLifecycle.principalPartition(current) : '')
      );
    };
    const isCurrent = () =>
      principalCurrent() &&
      samePrincipal() &&
      !this.disposed &&
      capturedEpoch === this.epoch &&
      (extraCurrent ? extraCurrent() : true);
    return { isCurrent, samePrincipal };
  }
}
