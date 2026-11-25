<script lang="ts">
  import type { Project } from '#lib/features/access/index.js';
  import EmptyState from '#lib/components/EmptyState.svelte';
  import ScrollTable from '#lib/components/ScrollTable.svelte';
  import { listPublications } from './api.js';

  type Kind = 'agents' | 'prompts' | 'toolsets';

  let {
    kind,
    busy,
    canCreate,
    pending,
    items,
    projects,
    empty,
    onCreate,
    onOpen
  }: {
    kind: Kind;
    busy: boolean;
    canCreate: boolean;
    pending: boolean;
    items: Awaited<ReturnType<typeof listPublications>>;
    projects: Project[];
    empty: { title: string; body: string };
    onCreate: () => void;
    onOpen: (id: string) => void;
  } = $props();
</script>

<section class="section" aria-label="Publications">
  <div class="page-actions">
    <button
      class="button button-primary"
      type="button"
      disabled={busy || !canCreate}
      onclick={onCreate}
    >
      New {kind.slice(0, -1)}
    </button>
  </div>
  {#if pending}<div class="loading-state" role="status">
      Loading publications…
    </div>{:else if !items.length}<EmptyState title={empty.title}>
      {empty.body}
    </EmptyState>{:else}<ScrollTable label="Publications">
      <table class="data-table">
        <thead>
          <tr>
            <th scope="col">Name</th><th scope="col">Project</th><th
              scope="col"
            >
              Published revision
            </th><th scope="col">Inspect</th>
          </tr>
        </thead><tbody>
          {#each items as item (item.id)}<tr>
              <td>{item.name}</td><td>
                {projects.find((project) => project.id === item.project_id)
                  ?.name ?? item.project_id}
              </td><td><code>{item.revision_id ?? 'Draft only'}</code></td><td>
                <button
                  class="button button-secondary"
                  type="button"
                  disabled={busy}
                  onclick={() => onOpen(item.id)}
                >
                  Open
                </button>
              </td>
            </tr>{/each}
        </tbody>
      </table>
    </ScrollTable>{/if}
</section>
