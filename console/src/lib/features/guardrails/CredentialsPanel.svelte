<script lang="ts">
  import { untrack } from 'svelte';
  import { createQuery, useQueryClient } from '@tanstack/svelte-query';
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useAction } from '$lib/editing/action.svelte';
  import {
    useMutationIntent,
    usePrincipalLifetime
  } from '$lib/editing/mutationIntent.svelte';
  import { nativeObject, parseManagementJSON } from '$lib/nativeJson';
  import { errorMessage } from '$lib/management/http';
  import ConfirmAction from '$lib/components/ConfirmAction.svelte';
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

<section class="card workflow">
  <h2>Guardrail credentials</h2>
  <p>
    Replacement creates a new named credential. Save and activate new definition
    and policy revisions, then replace their attachments. Existing revisions
    retain their credentials
  </p>
  {#if credentials.error}<p role="alert">
      {errorMessage(credentials.error)}
    </p>{/if}
  {#each (credentials.data ?? []).filter((value) => !projectID || value.project_id === projectID) as value (value.id)}
    <article>
      <h3>{value.name}</h3>
      <p>
        {projects.find((item) => item.id === value.project_id)?.name ??
          value.project_id} · {value.id}
      </p>
      {#if access.allows('DELETE /api/v1/guardrail-credentials/{credential_id}')}<ConfirmAction
          label="Delete unused credential"
          description={`Delete ${value.name} and its sealed secret. Credentials referenced by retained revisions cannot be deleted`}
          identity={value.id}
          run={(requestId) => deleteCredential(value, requestId)}
          done={async () => {
            await credentials.refetch();
          }}
        />{/if}
    </article>
  {/each}
  {#if access.allows('POST /api/v1/guardrail-credentials')}
    <form onsubmit={submit}>
      <label
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
      <label>New credential name<input required bind:value={name} /></label>
      <label
        >Secret fields as JSON<textarea
          required
          autocomplete="off"
          spellcheck="false"
          bind:value={credential}></textarea></label
      >
      <button class="button button-primary" disabled={action.busy}
        >Create credential</button
      >
    </form>
  {/if}
  {#if action.error}<p role="alert">{action.error}</p>{/if}
</section>
