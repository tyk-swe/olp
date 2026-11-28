<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import FormField from '#lib/components/FormField.svelte';
  import { useRole } from '#lib/session/useRole.svelte.js';
  import { useMutation } from '#lib/editing/action.svelte.js';
  import { focusFormError } from '#lib/editing/focusError.js';
  import { usePrincipalLifetime } from '#lib/editing/mutationIntent.svelte.js';
  import { errorMessage } from '#lib/management/http.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { listCredentials, createCredential, deleteCredential } from './api';
  import { validateCredential } from './credentialDraft';
  import { guardrailKeys } from '#lib/domain/guardrails/guardrailKeys.js';
  let {
    projects,
    projectID = '',
    oncreated = () => {}
  }: {
    projects: { id: string; name: string }[];
    projectID?: string;
    oncreated?: (id: string) => void;
  } = $props();
  const draft = $state({
    project: untrack(() => projectID),
    name: '',
    secret: ''
  });
  const access = useRole();
  const action = useMutation<'secret'>();
  const errors = $derived(action.fieldErrors);
  const client = useQueryClient();
  const credentials = createQuery(() => ({
    queryKey: guardrailKeys.credentials(),
    queryFn: listCredentials
  }));
  const visibleCredentials = $derived(
    (credentials.data ?? []).filter(
      (value) => !projectID || value.project_id === projectID
    )
  );
  const capture = usePrincipalLifetime(() => {
    draft.secret = '';
  });
  async function submit(
    event: SubmitEvent & { currentTarget: HTMLFormElement }
  ) {
    event.preventDefault();
    const form = event.currentTarget;
    const current = capture();
    const checked = validateCredential(draft);
    if (!action.check(checked)) return focusFormError(form);
    const accepted = await action.submit(
      checked.value,
      createCredential,
      async (created) => {
        draft.secret = '';
        await client.invalidateQueries({
          queryKey: guardrailKeys.credentials()
        });
        if (current()) oncreated(created.id);
      }
    );
    if (!accepted) await focusFormError(form);
  }
</script>

<section class="card stack-card">
  <h2>Guardrail credentials</h2>
  <p class="inline-status">
    Replacement creates a new named credential. Save and activate new definition
    and policy revisions, then replace their attachments. Existing revisions
    retain their credentials.
  </p>
  {#if credentials.error}
    <p class="inline-problem" role="alert">
      {errorMessage(credentials.error)}
    </p>
  {:else if credentials.isPending}
    <p class="inline-status" role="status">Loading credentials…</p>
  {:else if !visibleCredentials.length}
    <p class="inline-status">No guardrail credentials yet.</p>
  {:else}
    <ScrollTable label="Guardrail credentials">
      <table class="data-table">
        <caption class="sr-only">Guardrail credentials</caption>
        <thead>
          <tr>
            <th scope="col">Credential</th>
            <th scope="col">Project</th>
            <th scope="col">Actions</th>
          </tr>
        </thead>
        <tbody>
          {#each visibleCredentials as value (value.id)}
            <tr>
              <td>
                <strong>{value.name}</strong><br /><code>{value.id}</code>
              </td>
              <td>
                {projects.find((item) => item.id === value.project_id)?.name ??
                  value.project_id}
              </td>
              <td>
                <div class="page-actions">
                  {#if access.allows('DELETE /api/v1/guardrail-credentials/{credential_id}')}
                    <ConfirmAction
                      danger
                      label="Delete unused credential"
                      description={`Delete ${value.name} and its sealed secret. Credentials referenced by retained revisions cannot be deleted.`}
                      identity={value.id}
                      run={(requestId) => deleteCredential(value, requestId)}
                      done={async () => {
                        await credentials.refetch();
                      }}
                    />
                  {/if}
                </div>
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </ScrollTable>
  {/if}
  {#if access.allows('POST /api/v1/guardrail-credentials')}
    <form class="form-card credential-form" onsubmit={submit}>
      <FormField label="Project" error={undefined}>
        {#snippet control(attributes)}
          <select
            {...attributes}
            required
            bind:value={draft.project}
            disabled={Boolean(projectID)}
          >
            <option value="">
              Choose project
            </option>{#each projects as item (item.id)}<option value={item.id}>
                {item.name}
              </option>{/each}
          </select>
        {/snippet}
      </FormField>
      <FormField label="New credential name" error={undefined}>
        {#snippet control(attributes)}
          <input {...attributes} required bind:value={draft.name} />
        {/snippet}
      </FormField>
      <FormField label="Secret fields as JSON" error={errors.secret} full>
        {#snippet control(attributes)}
          <textarea
            {...attributes}
            required
            autocomplete="off"
            spellcheck="false"
            bind:value={draft.secret}></textarea>
        {/snippet}
      </FormField>
      <div class="page-actions">
        <button class="button button-primary" disabled={action.busy}>
          Create credential
        </button>
      </div>
      {#if action.error}<p
          class="inline-problem"
          role="alert"
          tabindex="-1"
          data-error-summary
        >
          {action.error}
        </p>{/if}
    </form>
  {/if}
</section>

<style>
  .credential-form {
    margin-top: 0;
    padding: 0;
  }
</style>
