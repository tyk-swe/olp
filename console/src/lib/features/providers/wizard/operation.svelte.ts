import { useAction } from '#lib/editing/action.svelte.js';
import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
import type { Valid, Validation } from '#lib/editing/validation.js';
import {
  errorMessage,
  fieldIssues,
  isEtagMismatch,
  type FieldIssue
} from '#lib/management/http.js';
import type { ConnectionField } from './connectionValidation';

export type WizardField = ConnectionField | 'manualModelNames';
export type WizardOperation = ReturnType<typeof useWizardOperation>;

type Problem = { message: string; issues: FieldIssue[] };
type Outcome<T> = {
  accept?: (value: T, isCurrent: () => boolean) => void | Promise<void>;
  serverFields?: Readonly<Record<string, WizardField>>;
};

const noProblem: Problem = { message: '', issues: [] };

export function useWizardOperation(allowed: () => boolean) {
  const action = useAction<WizardField>();
  let notice = $state('');
  let problem = $state.raw(noProblem);
  let conflict = $state(false);
  const capture = usePrincipalLifetime(() => {
    notice = '';
    problem = noProblem;
  });
  function describe(
    cause: unknown,
    shown: Readonly<Record<string, WizardField>>
  ) {
    if (isEtagMismatch(cause)) {
      conflict = true;
      return '';
    }
    problem = {
      message: '',
      issues: fieldIssues(cause).filter(
        ({ field }) => !Object.hasOwn(shown, field)
      )
    };
    return errorMessage(cause);
  }
  return {
    get busy() {
      return action.key;
    },
    get notice() {
      return notice;
    },
    get error() {
      return (
        problem.message ||
        action.error ||
        action.fieldErrors.configuration ||
        ''
      );
    },
    get issues() {
      return problem.issues;
    },
    get fieldErrors() {
      return action.fieldErrors;
    },
    get conflict() {
      return conflict;
    },
    resolveConflict() {
      conflict = false;
    },
    announce(message: string) {
      notice = message;
    },
    report(message: string, cause?: unknown) {
      action.reset();
      if (isEtagMismatch(cause)) conflict = true;
      else problem = { message, issues: fieldIssues(cause) };
    },
    reportRefreshFailure(message: string) {
      problem = {
        message: `The provider change succeeded, but refreshing failed. ${message}`,
        issues: []
      };
    },
    check<Value>(
      validation: Validation<Value, WizardField>
    ): validation is Valid<Value> {
      problem = noProblem;
      return action.check(validation);
    },
    run<T>(
      key: string,
      send: (isCurrent: () => boolean) => Promise<T>,
      { accept, serverFields = {} }: Outcome<T> = {}
    ): Promise<boolean> {
      if (action.busy || !allowed()) return Promise.resolve(false);
      notice = '';
      problem = noProblem;
      const isCurrent = capture();
      return action.run(() => send(isCurrent), accept, {
        key,
        serverFields,
        failure: (cause, stage) =>
          describe(cause, stage === 'send' ? serverFields : {})
      });
    }
  };
}
