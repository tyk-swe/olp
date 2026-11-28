import { onDestroy } from 'svelte';
import {
  applyServerFieldErrors,
  errorMessage,
  fieldIssues,
  isEtagMismatch
} from '#lib/management/http.js';
import {
  useMutationIntent,
  usePrincipalLifetime
} from './mutationIntent.svelte';
import type { FieldErrors, Valid, Validation } from './validation';

type Stage = 'send' | 'accept';

export type ActionFeedback<Field extends string = never> = {
  key?: string;
  fallback?: string;
  failure?: (cause: unknown, stage: Stage) => string;
  serverFields?: Readonly<Record<string, Field>>;
};

function describeFailure<Field extends string>(
  cause: unknown,
  stage: Stage,
  { failure, fallback, serverFields }: ActionFeedback<Field>
) {
  const message = failure
    ? failure(cause, stage)
    : errorMessage(cause, fallback);
  const fields: Readonly<Record<string, Field>> =
    stage === 'send' ? (serverFields ?? {}) : {};
  const issues = fieldIssues(cause);
  const allOnFields =
    issues.length > 0 &&
    issues.every(({ field }) => Object.hasOwn(fields, field));
  return {
    error: allOnFields ? '' : message,
    fieldErrors: applyServerFieldErrors(cause, fields)
  };
}

export function useAction<Field extends string = never>(
  refreshOnConflict?: () => Promise<unknown>,
  lifetime: 'principal' | 'authentication' = 'principal'
) {
  let activeKey = $state<string | null>(null);
  let error = $state('');
  let fieldErrors = $state.raw<FieldErrors<Field>>({});
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
          fieldErrors = {};
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
    get fieldErrors() {
      return fieldErrors;
    },
    get conflict() {
      return conflict;
    },
    reset() {
      error = '';
      fieldErrors = {};
      conflict = false;
    },
    check<Value>(
      validation: Validation<Value, Field>
    ): validation is Valid<Value> {
      error = '';
      fieldErrors = validation.ok ? {} : validation.fieldErrors;
      return validation.ok;
    },
    async run<T>(
      send: () => Promise<T>,
      accept?: (value: T, current: () => boolean) => void | Promise<void>,
      feedback: ActionFeedback<Field> = {}
    ): Promise<boolean> {
      if (activeKey !== null) return false;
      const current = capture();
      activeKey = feedback.key ?? 'submit';
      error = '';
      fieldErrors = {};
      conflict = false;
      let stage: Stage = 'send';
      try {
        const value = await send();
        if (current()) {
          stage = 'accept';
          await accept?.(value, current);
        }
        return current();
      } catch (cause) {
        if (current()) {
          ({ error, fieldErrors } = describeFailure(cause, stage, feedback));
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

export function useMutation<Field extends string = never>(
  refreshOnConflict?: () => Promise<unknown>
) {
  const action = useAction<Field>(refreshOnConflict);
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
    get fieldErrors() {
      return action.fieldErrors;
    },
    get conflict() {
      return action.conflict;
    },
    reset() {
      action.reset();
      intent.reset();
    },
    check: action.check,
    run: action.run,
    submit<I, R>(
      input: I,
      send: (input: I, requestId: string) => Promise<R>,
      accept?: (value: R, current: () => boolean) => void | Promise<void>,
      feedback?: ActionFeedback<Field>
    ) {
      return action.run(() => intent.submit(input, send), accept, feedback);
    },
    submitBuilt<I, B, R>(
      input: I,
      build: () => B,
      send: (input: B, requestId: string) => Promise<R>,
      accept?: (value: R, current: () => boolean) => void | Promise<void>,
      feedback?: ActionFeedback<Field>
    ) {
      return action.run(
        () => intent.submitBuilt(input, build, send),
        accept,
        feedback
      );
    }
  };
}
