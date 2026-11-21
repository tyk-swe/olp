import { onDestroy } from 'svelte';
import { errorMessage, isEtagMismatch } from '#lib/management/http.js';
import {
  useMutationIntent,
  usePrincipalLifetime
} from './mutationIntent.svelte';

export type ActionFeedback = {
  key?: string;
  fallback?: string;
  failure?: (cause: unknown, stage: 'send' | 'accept') => string;
};

export function useAction(
  refreshOnConflict?: () => Promise<unknown>,
  lifetime: 'principal' | 'authentication' = 'principal'
) {
  let activeKey = $state<string | null>(null);
  let error = $state('');
  let conflict = $state(false);
  const capture =
    lifetime === 'authentication'
      ? (() => {
          let mounted = true;
          onDestroy(() => {
            mounted = false;
          });
          return () => () => mounted;
        })()
      : usePrincipalLifetime(() => {
          activeKey = null;
          error = '';
          conflict = false;
        });
  return {
    get busy() {
      return activeKey !== null;
    },
    get key() {
      return activeKey ?? '';
    },
    get error() {
      return error;
    },
    set error(value: string) {
      error = value;
    },
    get conflict() {
      return conflict;
    },
    reset() {
      error = '';
      conflict = false;
    },
    async run<T>(
      send: () => Promise<T>,
      accept?: (value: T, current: () => boolean) => void | Promise<void>,
      feedback: ActionFeedback = {}
    ): Promise<boolean> {
      if (activeKey !== null) return false;
      const current = capture();
      activeKey = feedback.key ?? 'submit';
      error = '';
      conflict = false;
      let stage: 'send' | 'accept' = 'send';
      try {
        const value = await send();
        if (current()) {
          stage = 'accept';
          await accept?.(value, current);
        }
        return current();
      } catch (cause) {
        if (current()) {
          error = feedback.failure
            ? feedback.failure(cause, stage)
            : errorMessage(cause, feedback.fallback);
          if (stage === 'send' && isEtagMismatch(cause)) {
            conflict = true;
            try {
              await refreshOnConflict?.();
            } catch {
              // The reported conflict stands; a failed refresh adds nothing.
            }
          }
        }
        return false;
      } finally {
        if (current()) activeKey = null;
      }
    }
  };
}

export function useMutation(refreshOnConflict?: () => Promise<unknown>) {
  const action = useAction(refreshOnConflict);
  const intent = useMutationIntent();
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
    set error(value: string) {
      action.error = value;
    },
    get conflict() {
      return action.conflict;
    },
    reset() {
      action.reset();
      intent.reset();
    },
    run: action.run,
    submit<I, R>(
      input: I,
      send: (input: I, requestId: string) => Promise<R>,
      accept?: (value: R, current: () => boolean) => void | Promise<void>,
      feedback?: ActionFeedback
    ) {
      return action.run(() => intent.submit(input, send), accept, feedback);
    },
    submitBuilt<I, B, R>(
      input: I,
      build: () => B,
      send: (input: B, requestId: string) => Promise<R>,
      accept?: (value: R, current: () => boolean) => void | Promise<void>,
      feedback?: ActionFeedback
    ) {
      return action.run(
        () => intent.submitBuilt(input, build, send),
        accept,
        feedback
      );
    }
  };
}
