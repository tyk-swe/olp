import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
import {
  useMutationIntent,
  usePrincipalLifetime
} from './mutationIntent.svelte';

export function useAction(refreshOnConflict?: () => Promise<void>) {
  let busy = $state(false);
  let error = $state('');
  const capture = usePrincipalLifetime(() => {
    busy = false;
    error = '';
  });
  return {
    get busy() {
      return busy;
    },
    get error() {
      return error;
    },
    set error(value: string) {
      error = value;
    },
    async run<T>(
      send: () => Promise<T>,
      accept: (value: T) => void | Promise<void>
    ) {
      if (busy) return;
      const current = capture();
      busy = true;
      error = '';
      try {
        const value = await send();
        if (current()) await accept(value);
      } catch (cause) {
        if (current()) {
          error = errorMessage(cause);
          if (isEtagMismatch(cause)) await refreshOnConflict?.();
        }
      } finally {
        if (current()) busy = false;
      }
    }
  };
}

export function useMutation(refreshOnConflict?: () => Promise<void>) {
  const action = useAction(refreshOnConflict);
  const intent = useMutationIntent();
  return {
    get busy() {
      return action.busy;
    },
    get error() {
      return action.error;
    },
    set error(value: string) {
      action.error = value;
    },
    submit<I, R>(
      input: I,
      send: (input: I, requestId: string) => Promise<R>,
      accept: (value: R) => void | Promise<void>
    ) {
      return action.run(() => intent.submit(input, send), accept);
    }
  };
}
