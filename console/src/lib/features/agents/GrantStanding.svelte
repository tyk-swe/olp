<script lang="ts">
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { formatDate } from '#lib/format.js';
  import type { components } from '#lib/management/schema.js';

  type Grant = components['schemas']['MCPUserGrantStanding'];

  let {
    error,
    pending,
    items,
    working,
    onRevoke
  }: {
    error: string | null;
    pending: boolean;
    items: Grant[];
    working: boolean;
    onRevoke: (grantID: string) => void;
  } = $props();
</script>

<section class="section">
  <h2>Grant standing</h2>
  {#if error}<p class="inline-problem" role="alert">
      {error}
    </p>
  {:else if pending}<p class="inline-status" role="status">Loading grants</p>
  {:else if !items.length}<p class="inline-status">
      No personal grants or enrollments
    </p>
  {:else}
    <ScrollTable label="Personal grants">
      <table class="data-table">
        <thead>
          <tr>
            <th scope="col">Resource</th><th scope="col">Individual</th><th
              scope="col"
            >
              Audience and scopes
            </th><th scope="col">Standing</th><th scope="col">Expires</th><th
              scope="col"
            >
              Action
            </th>
          </tr>
        </thead>
        <tbody>
          {#each items as grant (grant.id)}
            <tr>
              <td>
                <code>{grant.binding.agent_id ?? grant.binding.server_id}</code>
              </td>
              <td>
                {grant.binding.identity.kind}<br /><code>
                  {grant.binding.identity.id}
                </code>
              </td>
              <td>
                {grant.binding.oauth?.audience}<br
                />{grant.binding.oauth?.scopes.join(' ')}
              </td>
              <td>{grant.state}</td><td>{formatDate(grant.expires_at)}</td>
              <td>
                {#if grant.grant_id && grant.state !== 'revoked'}<button
                    class="button button-secondary"
                    disabled={working}
                    onclick={() => onRevoke(grant.grant_id!)}
                  >
                    Revoke
                  </button>{/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </ScrollTable>
  {/if}
</section>
