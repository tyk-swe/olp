import { confirmAction } from '#lib/components/confirm.js';
import { onDestroy } from 'svelte';
import { ApiProblem, errorMessage, unanswered } from '#lib/management/http.js';
import { useAction } from '#lib/editing/action.svelte.js';
import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
import { captureResult } from '#lib/editing/result.js';
import { confirmDiscardChanges } from '#lib/editing/unsavedChanges.js';
import { mutationError } from './presentation';

export type ResourceEditorProps<Current> = {
  current?: Current;
  projectId: string;
  allowed: boolean;
  onDraftChange: (dirty: boolean) => void;
  onSaved: () => Promise<void>;
  onCancel: () => void;
};

/** One mounted editor owns a submission until it is saved or explicitly edited. */
export function useResourceSubmission<Input>(options: {
  allowed: () => boolean;
  creating: () => boolean;
  blocked?: () => boolean;
  build: () => Input;
  persist: (input: Input, requestId: string) => Promise<unknown>;
  onDraftChange: (dirty: boolean) => void;
  onSaved: () => Promise<void>;
  onCancel: () => void;
}) {
  const intent = useMutationIntent();
  const action = useAction();
  let dirty = $state(false);
  // A create whose response was lost remains the same logical submission.
  // Keep its fields fixed until the operator retries or explicitly edits it.
  let submitted = $state(false);

  $effect(() =>
    options.onDraftChange(
      dirty || submitted || !!options.blocked?.() || action.busy
    )
  );
  onDestroy(() => options.onDraftChange(false));

  function cancel() {
    if (confirmDiscardChanges(dirty || submitted || !!options.blocked?.()))
      options.onCancel();
  }

  async function editRequest() {
    if (
      !(await confirmAction({
        title: 'Start a new request?',
        body: 'The last request may already have been saved. Check before submitting again.',
        confirmLabel: 'Start over'
      }))
    )
      return;
    submitted = false;
    intent.reset();
    action.reset();
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (action.busy || options.blocked?.() || !options.allowed()) return;
    const prepared = captureResult(() => intent.prepare(options.build()));
    if (!prepared.ok) {
      submitted = false;
      action.error = mutationError(prepared.error);
      return;
    }
    await action.run(
      () => options.persist(prepared.value.input, prepared.value.requestId),
      async () => {
        intent.reset();
        dirty = false;
        submitted = false;
        await options.onSaved();
      },
      {
        failure: (cause, stage) => {
          if (stage === 'accept')
            return `Changes saved, but the page could not update. ${errorMessage(cause)}`;
          submitted =
            options.creating() &&
            (unanswered(cause) || !(cause instanceof ApiProblem));
          return mutationError(cause);
        }
      }
    );
  }

  return {
    get busy() {
      return action.busy;
    },
    get error() {
      return action.error;
    },
    get submitted() {
      return submitted;
    },
    markDirty() {
      dirty = true;
    },
    save,
    cancel,
    editRequest
  };
}

export type ResourceSubmission = ReturnType<typeof useResourceSubmission>;
