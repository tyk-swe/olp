<script lang="ts">
  import type { ProviderRevisionDiff } from '$lib/api/management/providerRevisions';
  let { revisionDiff }: { revisionDiff: ProviderRevisionDiff | null } =
    $props();
</script>

{#if revisionDiff}<div
    class="revision-diff"
    role="region"
    aria-label={`Provider revision ${revisionDiff.from_revision} to ${revisionDiff.to_revision} diff`}
  >
    <h3>
      Revision {revisionDiff.from_revision} → {revisionDiff.to_revision}
    </h3>
    <ul class="diff-flags">
      {#if revisionDiff.name_changed}<li>
          Name changed
        </li>{/if}{#if revisionDiff.connector_changed}<li>
          Connector changed
        </li>{/if}{#if revisionDiff.endpoint_changed}<li>
          Endpoint changed
        </li>{/if}{#if revisionDiff.cloud_context_changed}<li>
          Cloud context changed
        </li>{/if}{#if revisionDiff.deployment_changed}<li>
          Deployment changed
        </li>{/if}{#if revisionDiff.api_version_changed}<li>
          API version changed
        </li>{/if}{#if revisionDiff.credential_changed}<li>
          Credential version changed (secret remains redacted)
        </li>{/if}
    </ul>
    <div class="diff-columns">
      <div>
        <strong>Models added</strong>
        <ul>
          {#each revisionDiff.models_added as value (value)}<li>
              <code>{value}</code>
            </li>{/each}
        </ul>
      </div>
      <div>
        <strong>Models changed</strong>
        <ul>
          {#each revisionDiff.models_changed as value (value)}<li>
              <code>{value}</code>
            </li>{/each}
        </ul>
      </div>
      <div>
        <strong>Models removed</strong>
        <ul>
          {#each revisionDiff.models_removed as value (value)}<li>
              <code>{value}</code>
            </li>{/each}
        </ul>
      </div>
      <div>
        <strong>Capabilities added</strong>
        <ul>
          {#each revisionDiff.capabilities_added as value (value)}<li>
              <code>{value}</code>
            </li>{/each}
        </ul>
      </div>
      <div>
        <strong>Capabilities removed</strong>
        <ul>
          {#each revisionDiff.capabilities_removed as value (value)}<li>
              <code>{value}</code>
            </li>{/each}
        </ul>
      </div>
    </div>
  </div>{/if}

<style>
  .revision-diff {
    margin: 1rem 0;
    padding: 1rem;
    border: 1px solid var(--border);
    border-radius: 0.375rem;
    background: var(--surface-subtle);
  }
  .revision-diff h3 {
    margin: 0;
    font-size: 1rem;
  }
  .diff-flags {
    display: flex;
    flex-wrap: wrap;
    gap: 0.4rem 1.2rem;
    padding-left: 1.2rem;
  }
  .diff-columns {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.75rem;
  }
  .diff-columns ul {
    margin: 0.35rem 0 0;
    padding-left: 1.1rem;
  }
  .diff-columns li {
    overflow-wrap: anywhere;
  }

  /* Viewing a revision is the incidental action next to Restore, so it reads
     as a quiet control rather than a second bordered button. */

  code {
    font:
      0.75rem 'JetBrains Mono Variable',
      monospace;
  }
  @media (max-width: 64rem) {
    .diff-columns {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }
  @media (max-width: 42rem) {
    .diff-columns {
      grid-template-columns: 1fr;
    }
  }
</style>
