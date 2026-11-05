<script lang="ts">
  import { untrack } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { useAction } from '$lib/editing/action.svelte';
  import { guardUnsavedChanges } from '$lib/editing/unsavedChanges';
  import ConflictNotice from '$lib/shell/ConflictNotice.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/editing/mutationIntent.svelte';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { stateLabel } from '$lib/format';
  import { saveSink, type Sink, type SinkInput } from './api';
  import { sinkKeys } from './sinkKeys';
  const SINK_STREAMS = [
    'requests',
    'attempts',
    'guardrail_decisions',
    'audit',
    'runs',
    'trace_spans'
  ] as const;
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
  const initial = untrack(() => current);
  let baseline = $state(initial);
  const access = useRole();
  let name = $state(initial?.name ?? '');
  let project = $state(initial?.project_id ?? '');
  let destination = $state(initial?.destination ?? '');
  let enabled = $state(initial?.enabled ?? false);
  let streams = $state<SinkInput['streams']>(initial?.streams ?? ['requests']);
  let credential = $state('');
  let removeCredential = $state(false);
  const client = useQueryClient();
  const action = useAction(() =>
    client.invalidateQueries({ queryKey: sinkKeys.all })
  );
  const intent = useMutationIntent();
  usePrincipalLifetime(() => {
    credential = '';
  });
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    if (!streams.length || streams.length > 4) {
      action.error = 'Choose between one and four streams';
      return;
    }
    if (project && streams.includes('audit')) {
      action.error = 'Audit events require an installation sink';
      return;
    }
    const body: SinkInput = {
      name,
      project_id: project || null,
      type: 'https',
      destination,
      enabled,
      streams,
      ...(removeCredential
        ? { credential: null }
        : credential
          ? { credential }
          : {})
    };
    await action.run(
      () =>
        intent.submit({ current: baseline, body }, (input, requestId) =>
          saveSink(input.current, input.body, requestId)
        ),
      async (value) => {
        baseline = value;
        credential = '';
        removeCredential = false;
        clean = draft();
        await saved(value);
      }
    );
  }
  const draft = () =>
    JSON.stringify([
      name,
      project,
      destination,
      enabled,
      streams,
      credential,
      removeCredential
    ]);
  let clean = $state(draft());
  export function isDirty() {
    return draft() !== clean;
  }
  guardUnsavedChanges(isDirty);
</script>

<form class="card form-card" onsubmit={submit}>
  <h2>{current ? 'Edit signed fact export' : 'Create signed fact export'}</h2>
  <label class="form-field"
    >Name<input required maxlength="100" bind:value={name} /></label
  >
  <label class="form-field"
    >Scope<select
      required={!access.can('settings.update')}
      disabled={Boolean(current)}
      bind:value={project}
      >{#if access.can('settings.update')}<option value="">Installation</option
        >{:else}<option value="">Choose project</option
        >{/if}{#each projects as item (item.id)}<option value={item.id}
          >{item.name}</option
        >{/each}</select
    ></label
  >
  <label class="form-field"
    >HTTPS destination<input
      type="url"
      required
      pattern="https://.*"
      bind:value={destination}
    /></label
  >
  <fieldset>
    <legend>Streams, maximum four</legend
    >{#each SINK_STREAMS as stream (stream)}<label class="check-field"
        ><input
          type="checkbox"
          value={stream}
          bind:group={streams}
          disabled={stream === 'audit' && Boolean(project)}
        />{stateLabel(stream)}</label
      >{/each}
  </fieldset>
  <label class="form-field"
    >Replacement signing credential<input
      type="password"
      autocomplete="new-password"
      bind:value={credential}
      disabled={removeCredential}
    /></label
  >
  <p class="inline-status">
    Blank retains the current credential. {baseline?.has_credential
      ? 'A signing credential is configured'
      : 'No signing credential is configured'}
  </p>
  {#if baseline?.has_credential}<label class="check-field"
      ><input type="checkbox" bind:checked={removeCredential} />Remove signing
      credential</label
    >{/if}
  <label class="check-field"
    ><input type="checkbox" bind:checked={enabled} />Enabled</label
  >
  <div class="page-actions">
    <button class="button button-primary" disabled={action.busy}
      >Save export</button
    >
  </div>
  {#if reload && current?.etag !== baseline?.etag}
    <ConflictNotice notice="newer" onReload={reload} disabled={action.busy} />
  {/if}
  {#if action.error}<p class="inline-problem" role="alert">
      {action.error}
    </p>{/if}
</form>
