import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
import { usePrincipalLifetime } from './mutationIntent.svelte';

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
