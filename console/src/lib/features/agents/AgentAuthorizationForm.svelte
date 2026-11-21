<script lang="ts">
  import type { Writable } from 'svelte/store';
  import { listPublications } from './api.js';
  import type { PageState } from './grantsState.js';
  import type { PersonalGrantEnrollment } from './grantEnrollment.svelte';
  import type { CreateQueryResult } from '@tanstack/svelte-query';

  let {
    pageState,
    agents,
    working,
    patch,
    enrollment,
    startAgentEnrollment
  }: {
    pageState: Writable<PageState>;
    agents: CreateQueryResult<Awaited<ReturnType<typeof listPublications>>>;
    working: boolean;
    patch: (values: Partial<PageState>) => void;
    enrollment: PersonalGrantEnrollment;
    startAgentEnrollment: () => Promise<void>;
  } = $props();
</script>

<form
  onsubmit={(event) => {
    event.preventDefault();
    void startAgentEnrollment();
  }}
>
  <label class="form-field"
    >Published agent <select
      value={$pageState.agentID}
      disabled={working}
      onchange={(event) => {
        enrollment.clearInvitation();
        patch({ agentID: event.currentTarget.value });
      }}
      required
    >
      <option value="">Select an agent</option>
      {#each (agents.data ?? []).filter((item) => item.revision_id && !item.retired) as item (item.id)}
        <option value={item.id}>{item.name}</option>
      {/each}
    </select></label
  >
  <label class="form-field"
    >Agent API key UUID <input
      value={$pageState.agentKeyID}
      disabled={working}
      oninput={(event) => patch({ agentKeyID: event.currentTarget.value })}
      required
    /></label
  >
  <button class="button button-secondary" disabled={working || !$pageState.agentID}
    >Start agent key enrollment</button
  >
</form>
