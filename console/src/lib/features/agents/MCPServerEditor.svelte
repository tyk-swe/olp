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
  import { createServer, saveServer, type MCPServer } from './api';
  import { agentKeys } from './agentKeys';
  import MCPServerAuthorizationFields from './MCPServerAuthorizationFields.svelte';
  import MCPServerPricingFields from './MCPServerPricingFields.svelte';
  import {
    clearedSecrets,
    serverDraft,
    serverFields,
    validateServer,
    type ServerField
  } from './serverDraft';
  let {
    current,
    projects,
    onsaved,
    reload
  }: {
    current: MCPServer | null;
    projects: { id: string; name: string }[];
    reload?: () => void;
    onsaved: (server: MCPServer) => Promise<void>;
  } = $props();
  const initial = untrack(() => current);
  let baseline = $state.raw(initial);
  let draft = $state(serverDraft(initial));
  const client = useQueryClient();
  const action = useMutation<ServerField>(() =>
    client.invalidateQueries({ queryKey: agentKeys.servers() })
  );
  usePrincipalLifetime(() => {
    draft = { ...draft, ...clearedSecrets };
  });
  async function save(event: SubmitEvent & { currentTarget: HTMLFormElement }) {
    event.preventDefault();
    const form = event.currentTarget;
    const checked = validateServer($state.snapshot(draft), baseline);
    if (!action.check(checked)) return focusFormError(form);
    const accepted = await action.submit(
      { current: baseline, ...checked.value },
      (input, requestId) =>
        input.current
          ? saveServer(input.current, input.update, requestId)
          : createServer(input.create, requestId),
      async (server) => {
        baseline = server;
        draft = serverDraft(server);
        await onsaved(server);
      },
      { serverFields }
    );
    if (!accepted) await focusFormError(form);
  }
  export function isDirty() {
    return hasChanges(serverDraft(baseline), $state.snapshot(draft));
  }
  guardUnsavedChanges(isDirty);
</script>

<form class="form-card" novalidate onsubmit={save}>
  <FormField label="Name" error={action.fieldErrors.name}>
    {#snippet control(attributes)}
      <input {...attributes} required maxlength="100" bind:value={draft.name} />
    {/snippet}
  </FormField>
  <FormField label="Project" error={action.fieldErrors.project}>
    {#snippet control(attributes)}
      <select
        {...attributes}
        required
        disabled={Boolean(current)}
        bind:value={draft.project}
      >
        <option value="">
          Choose project
        </option>{#each projects as item (item.id)}<option value={item.id}>
            {item.name}
          </option>{/each}
      </select>
    {/snippet}
  </FormField>
  <FormField label="Transport" error={action.fieldErrors.transport}>
    {#snippet control(attributes)}
      <select
        {...attributes}
        disabled={Boolean(current)}
        bind:value={draft.transport}
      >
        <option value="streamable_http">Streamable HTTP</option><option
          value="sse"
        >
          SSE
        </option><option value="stdio">Local stdio</option>
      </select>
    {/snippet}
  </FormField>
  {#if draft.transport === 'stdio'}
    <FormField
      label="Approved executable"
      error={action.fieldErrors.executable}
    >
      {#snippet control(attributes)}
        <input
          {...attributes}
          required
          disabled={Boolean(current)}
          bind:value={draft.executable}
        />
      {/snippet}
    </FormField>
    <FormField label="Executable SHA256" error={action.fieldErrors.digest}>
      {#snippet control(attributes)}
        <input
          {...attributes}
          required
          disabled={Boolean(current)}
          bind:value={draft.digest}
        />
      {/snippet}
    </FormField>
  {:else}
    <FormField label="Endpoint" error={action.fieldErrors.endpoint}>
      {#snippet control(attributes)}
        <input
          {...attributes}
          type="url"
          required
          bind:value={draft.endpoint}
        />
      {/snippet}
    </FormField>
  {/if}
  <label class="check-field">
    <input type="checkbox" bind:checked={draft.enabled} />Enabled
  </label>
  {#if draft.transport !== 'stdio'}
    <FormField label="Bearer credential" error={action.fieldErrors.credential}>
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
    {#if baseline?.has_credential}<label class="check-field">
        <input type="checkbox" bind:checked={draft.removeCredential} />Remove
        stored bearer credential
      </label>{/if}
    <p class="inline-status">
      Leave secret fields blank to retain existing values
    </p>
  {/if}
  <MCPServerPricingFields
    bind:pricing={draft.pricing}
    tools={baseline?.catalog.tools ?? []}
    errors={action.fieldErrors}
  />
  {#if draft.transport !== 'stdio'}
    <MCPServerAuthorizationFields bind:draft errors={action.fieldErrors} />
  {/if}
  <div class="page-actions">
    <button class="button button-primary" disabled={action.busy}>
      Save and certify
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
