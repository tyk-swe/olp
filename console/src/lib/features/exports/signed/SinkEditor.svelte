<script lang="ts">
  import { untrack } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import {
    guardUnsavedChanges,
    hasChanges
  } from '#lib/editing/unsavedChanges.js';
  import ConflictNotice from '#lib/components/ConflictNotice.svelte';
  import FormField from '#lib/components/FormField.svelte';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { stateLabel } from '#lib/format.js';
  import { saveSink, type Sink } from './api';
  import {
    SINK_STREAMS,
    sinkDraft,
    sinkServerFields,
    validateSink,
    type SinkField
  } from './sinkDraft';
  import { sinkKeys } from './sinkKeys';
  let {
    current,
    projects,
    saved,
    reload
  }: {
    current: Sink | null;
    projects: { id: string; name: string }[];
    reload?: () => void;
    saved: (value: Sink) => Promise<void>;
  } = $props();
  const id = $props.id();
  const streamsErrorId = `${id}-streams-error`;
  const initial = untrack(() => current);
  let baseline = $state.raw(initial);
  let draft = $state(sinkDraft(initial));
  const access = useRole();
  const client = useQueryClient();
  const action = useMutation<SinkField>(() =>
    client.invalidateQueries({ queryKey: sinkKeys.all })
  );
  usePrincipalLifetime(() => {
    draft.credential = '';
  });
  async function submit(
    event: SubmitEvent & { currentTarget: HTMLFormElement }
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    const checked = validateSink(
      $state.snapshot(draft),
      !baseline && !access.can('settings.update')
    );
    if (!action.check(checked)) return focusFormError(form);
    const accepted = await action.submit(
      { current: baseline, body: checked.value },
      (input, requestId) => saveSink(input.current, input.body, requestId),
      async (value) => {
        baseline = value;
        draft = sinkDraft(value);
        await saved(value);
      },
      { serverFields: sinkServerFields }
    );
    if (!accepted) await focusFormError(form);
  }
  export function isDirty() {
    return hasChanges(sinkDraft(baseline), $state.snapshot(draft));
  }
  guardUnsavedChanges(isDirty);
</script>

<form class="form-card" novalidate onsubmit={submit}>
  <FormField label="Name" error={action.fieldErrors.name}>
    {#snippet control(attributes)}
      <input {...attributes} required bind:value={draft.name} />
    {/snippet}
  </FormField>
  <FormField label="Scope" error={action.fieldErrors.project}>
    {#snippet control(attributes)}
      <select
        {...attributes}
        required={!access.can('settings.update')}
        disabled={Boolean(current)}
        bind:value={draft.project}
      >
        <option value="">
          {access.can('settings.update') ? 'Installation' : 'Choose project'}
        </option>
        {#each projects as item (item.id)}<option value={item.id}>
            {item.name}
          </option>{/each}
      </select>
    {/snippet}
  </FormField>
  <FormField label="HTTPS destination" error={action.fieldErrors.destination}>
    {#snippet control(attributes)}
      <input
        {...attributes}
        type="url"
        required
        bind:value={draft.destination}
      />
    {/snippet}
  </FormField>
  <fieldset>
    <legend>Streams, maximum four</legend>
    {#each SINK_STREAMS as stream (stream)}<label class="check-field">
        <input
          type="checkbox"
          value={stream}
          bind:group={draft.streams}
          disabled={stream === 'audit' &&
            Boolean(draft.project) &&
            !draft.streams.includes('audit')}
          aria-invalid={action.fieldErrors.streams ? 'true' : undefined}
          aria-describedby={action.fieldErrors.streams
            ? streamsErrorId
            : undefined}
        />{stateLabel(stream)}
      </label>{/each}
    {#if action.fieldErrors.streams}<p class="field-error" id={streamsErrorId}>
        {action.fieldErrors.streams}
      </p>{/if}
  </fieldset>
  <FormField
    label="Replacement signing credential"
    error={action.fieldErrors.credential}
  >
    {#snippet control(attributes)}
      <input
        {...attributes}
        type="password"
        autocomplete="new-password"
        bind:value={draft.credential}
        disabled={draft.removeCredential}
      />
    {/snippet}
  </FormField>
  <p class="inline-status">
    Blank retains the current credential. {baseline?.has_credential
      ? 'A signing credential is configured'
      : 'No signing credential is configured'}
  </p>
  {#if baseline?.has_credential}<label class="check-field">
      <input type="checkbox" bind:checked={draft.removeCredential} />Remove
      signing credential
    </label>{/if}
  <label class="check-field">
    <input type="checkbox" bind:checked={draft.enabled} />Enabled
  </label>
  <div class="page-actions">
    <button class="button button-primary" disabled={action.busy}>
      Save export
    </button>
  </div>
  {#if reload && current?.etag !== baseline?.etag}
    <ConflictNotice notice="newer" onReload={reload} disabled={action.busy} />
  {/if}
  {#if action.error}<p
      class="inline-problem"
      role="alert"
      tabindex="-1"
      data-error-summary
    >
      {action.error}
    </p>{/if}
</form>
