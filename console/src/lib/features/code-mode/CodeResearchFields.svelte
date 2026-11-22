<script lang="ts">
  import type { CodeAccount } from '#lib/features/code-mode/api.js';

  type ResearchService = 'search' | 'reader' | 'zread';

  let {
    research = $bindable(),
    researchAccount = $bindable(),
    researchServices = $bindable(),
    researchAccounts,
    services
  }: {
    research: boolean;
    researchAccount: string;
    researchServices: ResearchService[];
    researchAccounts: CodeAccount[];
    services: readonly { id: string; label: string }[];
  } = $props();
</script>

<label class="choice"
  ><input
    id="code-research-enabled"
    type="checkbox"
    bind:checked={research}
  />Research tools</label
>
{#if research}
  <div class="form-field">
    <label for="code-research-account">Research account</label>
    <select id="code-research-account" bind:value={researchAccount} required>
      <option value="">Choose a Z.ai account</option>
      {#each researchAccounts as account (account.id)}
        <option value={account.id}>{account.name}</option>
      {/each}
      {#if researchAccount && !researchAccounts.some((account) => account.id === researchAccount)}
        <option value={researchAccount}>Current account · unavailable</option>
      {/if}
    </select>
    <small
      >Uses an enrolled Z.ai global account in this pool, independently of the
      route's models.</small
    >
    {#if !researchAccounts.length}
      <p>
        Add an eligible Z.ai global account to this pool to enable research.
      </p>
    {/if}
  </div>
  <fieldset>
    <legend>Research services</legend>
    {#each services as service (service.id)}
      <label class="choice">
        <input
          type="checkbox"
          value={service.id}
          bind:group={researchServices}
        />
        {service.label}
      </label>
    {/each}
  </fieldset>
  <p>
    Publish, then copy the client configuration. Your existing OLP key
    authorizes these tools.
  </p>
{/if}
