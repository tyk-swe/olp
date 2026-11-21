<script lang="ts">
  import { formatDate } from '#lib/format.js';
  import type { ApiKey } from './api.js';

  let { overlaps }: { overlaps: NonNullable<ApiKey['active_overlaps']> } =
    $props();
</script>

{#if overlaps.length}
  <section aria-labelledby="overlap-heading">
    <h2 id="overlap-heading">Overlapping secrets</h2>
    <p class="muted">
      Observations cover admitted HTTP generation requests, including cache
      hits. They can lag or be incomplete. A missing timestamp does not mean all
      clients have switched.
    </p>
    <div class="table-shell">
      <table class="data-table">
        <thead>
          <tr>
            <th scope="col">Previous lookup ID</th>
            <th scope="col">Valid until</th>
            <th scope="col">Last observed use</th>
          </tr>
        </thead>
        <tbody>
          {#each overlaps as overlap (overlap.lookup_id)}
            <tr>
              <td><code>{overlap.lookup_id}</code></td>
              <td>{formatDate(overlap.expires_at)}</td>
              <td>
                {#if overlap.last_observed_use}
                  <time datetime={overlap.last_observed_use}>
                    {formatDate(overlap.last_observed_use)}
                  </time>
                {/if}
                {#if overlap.observation_status === 'degraded'}
                  <div class="badge warning">
                    {overlap.last_observed_use
                      ? 'May be stale (recording failed)'
                      : 'Observation unavailable (recording failed)'}
                  </div>
                  {#if !overlap.last_observed_use}
                    <small>The old secret may still be in use.</small>
                  {/if}
                {:else if !overlap.last_observed_use}
                  {overlap.observation_status === 'ok'
                    ? 'No use observed'
                    : 'Observation unavailable'}
                {/if}
              </td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  </section>
{/if}
