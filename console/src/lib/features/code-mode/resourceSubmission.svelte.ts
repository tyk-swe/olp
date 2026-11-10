import { confirmAction } from '#lib/components/confirm.js';
import { onDestroy } from 'svelte';
import { ApiProblem, unanswered } from '#lib/management/http.js';
import { useMutationIntent } from '#lib/editing/mutationIntent.svelte.js';
import { confirmDiscardChanges } from '#lib/editing/unsavedChanges.js';
import { mutationError, type Validation } from './presentation';

export type ResourceEditorProps<Current> = {
  current?: Current;
  projectId: string;
  allowed: boolean;
  onDraftChange: (dirty: boolean) => void;
  onSaved: () => Promise<void>;
  onCancel: () => void;
};

export function useResourceSubmission<Input>(options: {
  allowed: () => boolean;
  creating: () => boolean;
  blocked?: () => boolean;
  build: () => Validation<Input>;
  persist: (input: Input, requestId: string) => Promise<unknown>;
  onDraftChange: (dirty: boolean) => void;
  onSaved: () => Promise<void>;
  onCancel: () => void;
}) {
  const intent = useMutationIntent();
  let busy = $state(false);
  let error = $state('');
  let dirty = $state(false);
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
    error = '';
  }

  async function save(event: SubmitEvent) {
    event.preventDefault();
    if (busy || options.blocked?.() || !options.allowed()) return;
    const built = options.build();
    if (!built.ok) {
      error = built.message;
      return;
    }
    busy = true;
    error = '';
    let dispatched = false;
    try {
      const submission = intent.prepare(built.value);
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
