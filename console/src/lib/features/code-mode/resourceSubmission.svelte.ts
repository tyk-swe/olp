import { onDestroy } from 'svelte';
import { ApiProblem, unanswered } from '$lib/api/http';
import { useMutationIntent } from '$lib/forms/mutationIntent.svelte';
import { confirmDiscardChanges } from '$lib/forms/unsavedChanges';
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
  let busy = $state(false);
  let error = $state('');
  let dirty = $state(false);
  // A create whose response was lost remains the same logical submission.
  // Keep its fields fixed until the operator retries or explicitly edits it.
  let submitted = $state(false);
  let active = true;

  $effect(() =>
    options.onDraftChange(dirty || submitted || !!options.blocked?.() || busy)
  );
  onDestroy(() => {
    active = false;
    options.onDraftChange(false);
  });

  function cancel() {
    if (confirmDiscardChanges(dirty || submitted || !!options.blocked?.()))
      options.onCancel();
  }

  function editRequest() {
    if (
      !confirm(
        'The previous request may already have been saved. Start a different submission?'
      )
    )
      return;
    submitted = false;
    intent.reset();
    error = '';
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (busy || options.blocked?.() || !options.allowed()) return;
    busy = true;
    error = '';
    let dispatched = false;
    try {
      const submission = intent.prepare(options.build());
      const promise = options.persist(submission.input, submission.requestId);
      dispatched = true;
      await promise;
      if (!active) return;
      intent.reset();
      dirty = false;
      submitted = false;
      await options.onSaved();
    } catch (e) {
      if (!active) return;
      submitted =
        dispatched &&
        options.creating() &&
        (unanswered(e) || !(e instanceof ApiProblem));
      error = mutationError(e);
    } finally {
      if (active) busy = false;
    }
  }

  return {
    get busy() {
      return busy;
    },
    get error() {
      return error;
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
