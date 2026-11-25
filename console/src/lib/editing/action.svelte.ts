import { onDestroy } from 'svelte';
import {
  errorMessage,
  fieldIssues,
  isEtagMismatch,
  type FieldIssue
} from '#lib/management/http.js';
import {
  useMutationIntent,
  usePrincipalLifetime
} from './mutationIntent.svelte';
import { readResult } from './result';

export type ActionFeedback = {
  key?: string;
  fallback?: string;
  failure?: (cause: unknown, stage: 'send' | 'accept') => string;
};

export type ActionOptions = {
  refreshOnConflict?: () => Promise<unknown>;
  lifetime?: 'principal' | 'authentication';
  current?: () => boolean;
  conflict?: (cause: unknown) => boolean;
  begin?: () => void;
};

export function useAction(options: ActionOptions = {}) {
  let activeKey = $state<string | null>(null);
  let error = $state('');
  let conflict = $state(false);
  let issues = $state<FieldIssue[]>([]);
  let generation = 0;
  let pending: AbortController | null = null;
  const retire = () => {
    generation += 1;
    pending?.abort();
    pending = null;
    activeKey = null;
    error = '';
    conflict = false;
    issues = [];
  };
  const capture =
    options.lifetime === 'authentication'
      ? (() => {
          let mounted = true;
          onDestroy(() => {
            mounted = false;
            pending?.abort();
          });
          return () => () => mounted;
        })()
      : usePrincipalLifetime(retire);
  const fail = async (
    cause: unknown,
    stage: 'send' | 'accept',
    feedback: ActionFeedback
  ) => {
    if (stage === 'send' && options.conflict?.(cause)) {
      conflict = true;
      return;
    }
    error = feedback.failure
      ? feedback.failure(cause, stage)
      : errorMessage(cause, feedback.fallback);
    issues = fieldIssues(cause);
    if (stage !== 'send' || options.conflict || !isEtagMismatch(cause)) return;
    conflict = true;
    const refresh = options.refreshOnConflict;
    if (refresh) await readResult(refresh);
  };
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
    get issues() {
      return issues;
    },
    set issues(value: FieldIssue[]) {
      issues = value;
    },
    reset() {
      error = '';
      conflict = false;
      issues = [];
    },
    retire,
    async run<T>(
      send: (current: () => boolean, signal: AbortSignal) => Promise<T>,
      accept?: (value: T, current: () => boolean) => void | Promise<void>,
      feedback: ActionFeedback = {}
    ): Promise<boolean> {
      if (activeKey !== null) return false;
      const owned = capture();
      const started = generation;
      const current = () =>
        owned() && started === generation && (options.current?.() ?? true);
      const abort = new AbortController();
      pending = abort;
      activeKey = feedback.key ?? 'submit';
      error = '';
      conflict = false;
      issues = [];
      options.begin?.();
      let stage: 'send' | 'accept' = 'send';
      try {
        const value = await send(current, abort.signal);
        if (current()) {
          stage = 'accept';
          await accept?.(value, current);
        }
        return current();
      } catch (cause) {
        if (current()) await fail(cause, stage, feedback);
        return false;
      } finally {
        if (current()) activeKey = null;
      }
    }
  };
}

export type Action = ReturnType<typeof useAction>;

export function useMutation(refreshOnConflict?: () => Promise<unknown>) {
  const action = useAction({ refreshOnConflict });
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
