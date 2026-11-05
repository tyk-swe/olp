<script lang="ts">
  import { untrack } from 'svelte';
  import { useQueryClient } from '@tanstack/svelte-query';
  import { useAction } from '$lib/editing/action.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/editing/mutationIntent.svelte';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { saveSink, sinkKeys, type Sink, type SinkInput } from './api';
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
        await saved(value);
      }
    );
  }
</script>

<form class="card" onsubmit={submit}>
  <h2>{current ? 'Edit signed fact export' : 'Create signed fact export'}</h2>
  <label>Name<input required maxlength="100" bind:value={name} /></label>
  <label
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
  <label
    >HTTPS destination<input
      type="url"
      required
      pattern="https://.*"
      bind:value={destination}
    /></label
  >
  <fieldset>
    <legend>Streams, maximum four</legend
    >{#each ['requests', 'attempts', 'guardrail_decisions', 'audit', 'runs', 'trace_spans'] as stream (stream)}<label
        ><input
          type="checkbox"
          value={stream}
          bind:group={streams}
          disabled={stream === 'audit' && Boolean(project)}
        />{stream.replaceAll('_', ' ')}</label
      >{/each}
  </fieldset>
  <label
    >Replacement signing credential<input
      type="password"
      autocomplete="new-password"
      bind:value={credential}
      disabled={removeCredential}
    /></label
  >
  <p>
    Blank retains the current credential. {baseline?.has_credential
      ? 'A signing credential is configured'
      : 'No signing credential is configured'}
  </p>
  {#if baseline?.has_credential}<label
      ><input type="checkbox" bind:checked={removeCredential} />Remove signing
      credential</label
    >{/if}
  <label><input type="checkbox" bind:checked={enabled} />Enabled</label><button
    class="button button-primary"
    disabled={action.busy}>Save export</button
  >
  {#if reload && current?.etag !== baseline?.etag}
    <p role="status">
      This resource changed since editing began. Your draft is preserved.
      Reloading discards your edits
    </p>
    <button
      class="button button-secondary"
      type="button"
      disabled={action.busy}
      onclick={reload}>Reload latest</button
    >
  {/if}
  {#if action.error}<p role="alert">{action.error}</p>{/if}
</form>
