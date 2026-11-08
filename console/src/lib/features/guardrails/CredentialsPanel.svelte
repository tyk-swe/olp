<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '#lib/features/access/session/useRole.svelte.js';
  import { useAction } from '#lib/editing/action.svelte.js';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '#lib/editing/mutationIntent.svelte.js';
  import { nativeObject, parseManagementJSON } from '#lib/nativeJson.js';
  import { errorMessage } from '#lib/management/http.js';
  import ConfirmAction from '#lib/components/ConfirmAction.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { listCredentials, createCredential, deleteCredential } from './api';
  import { guardrailKeys } from './guardrailKeys';
  let {
    projects,
    projectID = '',
    oncreated = () => {}
  }: {
    projects: { id: string; name: string }[];
    projectID?: string;
    oncreated?: (id: string) => void;
  } = $props();
  let project = $state(untrack(() => projectID));
  let name = $state('');
  let credential = $state('');
  const access = useRole();
  const action = useAction();
  const intent = useMutationIntent();
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
    credential = '';
  });
  async function submit(event: SubmitEvent) {
    event.preventDefault();
    const current = capture();
    await action.run(
      async () => {
        const parsed = parseManagementJSON(credential);
        if (!nativeObject(parsed)) return { ok: false as const };
        return {
          ok: true as const,
          value: await intent.submit(
            { name, project_id: project, credential: parsed },
            createCredential
          )
        };
      },
      async (result) => {
        if (!result.ok) {
          action.error = 'Enter a credential JSON object';
          return;
        }
        credential = '';
        await client.invalidateQueries({
          queryKey: guardrailKeys.credentials()
        });
        if (current()) oncreated(result.value.id);
      }
    );
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
              <td><strong>{value.name}</strong><br /><code>{value.id}</code></td
              >
              <td
                >{projects.find((item) => item.id === value.project_id)?.name ??
                  value.project_id}</td
              >
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
      <label class="form-field"
        >Project<select
          required
          bind:value={project}
          disabled={Boolean(projectID)}
          ><option value="">Choose project</option
          >{#each projects as item (item.id)}<option value={item.id}
              >{item.name}</option
            >{/each}</select
        ></label
      >
      <label class="form-field"
        >New credential name<input required bind:value={name} /></label
      >
      <label class="form-field full"
        >Secret fields as JSON<textarea
          required
          autocomplete="off"
          spellcheck="false"
          bind:value={credential}></textarea></label
      >
      <div class="page-actions">
        <button class="button button-primary" disabled={action.busy}
          >Create credential</button
        >
      </div>
    </form>
  {/if}
  {#if action.error}<p class="inline-problem" role="alert">
      {action.error}
    </p>{/if}
</section>

<style>
  .credential-form {
    margin-top: 0;
    padding: 0;
  }
</style>
