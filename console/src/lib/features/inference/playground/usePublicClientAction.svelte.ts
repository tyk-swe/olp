import { onDestroy } from 'svelte';
import { useAction } from '#lib/editing/action.svelte.js';
import { errorMessage } from '#lib/management/http.js';

export function usePublicClientAction(
  fallback: string,
  onCleanup?: () => void
) {
  const action = useAction();
  let abort: AbortController | null = null;
  let epoch = 0;

  onDestroy(() => {
    epoch += 1;
    abort?.abort();
    abort = null;
    onCleanup?.();
  });

  return {
    get busy() {
      return action.busy;
    },
    get key() {
      return action.key;
    },
    get error() {
      return action.error;
    },
    setError(message: string) {
      action.error = message;
    },
    clearError() {
      action.reset();
    },
    cancel() {
      abort?.abort();
    },
    reset() {
      epoch += 1;
      abort?.abort();
      abort = null;
      action.reset();
    },
    async run<T>(
      send: (signal: AbortSignal) => Promise<T>,
      accept: (value: T) => void,
      actionKey?: string
    ): Promise<boolean> {
      if (action.busy) return false;
      const owner = epoch;
      const controller = new AbortController();
      abort = controller;
      const settled = await action.run(
        () => send(controller.signal),
        (value, current) => {
          if (owner === epoch && current()) accept(value);
        },
        {
          key: actionKey,
          failure: (reason) =>
            owner === epoch ? errorMessage(reason, fallback) : action.error
        }
      );
      if (abort === controller) abort = null;
      return settled && owner === epoch;
    }
  };
}
