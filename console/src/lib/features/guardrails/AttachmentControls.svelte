<script lang="ts">
  import { useRole } from '$lib/features/access/session/useRole.svelte';
  import { useAction } from '$lib/editing/action.svelte';
  import ConfirmAction from '$lib/components/ConfirmAction.svelte';
  import {
    detachPolicy,
    replaceAttachment,
    getPolicyRevision,
    type Attachment,
    type Policy
  } from './api';
  let {
    value,
    policies,
    changed
  }: {
    value: Attachment & { id: string; etag: string };
    policies: Policy[];
    changed: () => Promise<void>;
  } = $props();
  const access = useRole();
  const action = useAction();
  let revision = $state('');
  let inspected = $state<Awaited<ReturnType<typeof getPolicyRevision>> | null>(
    null
  );
</script>

<p>Attached revision {value.policy_revision_id}</p>
<p>Active policy revision {value.active_revision_id ?? 'None'}</p>
{#if value.policy_id}<button
    class="button button-secondary"
    type="button"
    disabled={action.busy}
    onclick={() =>
      action.run(
        () =>
          getPolicyRevision(value.policy_id ?? '', value.policy_revision_id),
        (result) => {
          inspected = result;
        }
      )}>Inspect pinned revision</button
  >{/if}
{#if inspected}<pre>{JSON.stringify(inspected, null, 2)}</pre>{/if}
{#if access.allows('PUT /api/v1/guardrail-attachments/{attachment_id}')}
  <label
    >Replacement active revision<select bind:value={revision}
      ><option value="">Choose policy</option
      >{#each policies.filter((item) => item.active_revision_id && !item.retired_at) as item (item.policy_id)}<option
          value={item.active_revision_id}
          >{item.name} · {item.active_revision_id}</option
        >{/each}</select
    ></label
  >
  <ConfirmAction
    label="Replace attachment"
    description="Switch this attachment to the selected revision while retaining its scope and position"
    identity={`${value.id}:${value.etag}:${revision}`}
    disabled={!revision}
    run={(requestId) => replaceAttachment(value, revision, requestId)}
    done={changed}
  />
{/if}
{#if access.allows('DELETE /api/v1/guardrail-attachments/{attachment_id}')}<ConfirmAction
    label="Detach policy"
    description="Remove this policy attachment. Inherited policies from broader scopes still apply"
    identity={`${value.id}:${value.etag}`}
    run={(requestId) => detachPolicy(value, requestId)}
    done={changed}
  />{/if}
{#if action.error}<p role="alert">{action.error}</p>{/if}
