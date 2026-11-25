<script lang="ts">
  let {
    canManage,
    busy,
    refreshPending,
    selectedCount,
    failedCount,
    status,
    failures,
    onValidate,
    onRetryFailed,
    onRetryRefresh,
    onStop
  }: {
    canManage: boolean;
    busy: boolean;
    refreshPending: boolean;
    selectedCount: number;
    failedCount: number;
    status: string;
    failures: string[];
    onValidate: () => void;
    onRetryFailed: () => void;
    onRetryRefresh: () => void;
    onStop: () => void;
  } = $props();
</script>

<div class="form-actions">
  {#if canManage}<button
      class="button button-primary"
      type="button"
      disabled={busy || refreshPending || !selectedCount}
      onclick={onValidate}
    >
      Validate {selectedCount} selected models
    </button>{/if}
  {#if failedCount && !busy && !refreshPending}<button
      class="button button-secondary"
      type="button"
      onclick={onRetryFailed}
    >
      Retry {failedCount} failed models
    </button>{/if}
  {#if refreshPending && !busy}<button
      class="button button-secondary"
      type="button"
      onclick={onRetryRefresh}
    >
      Retry refresh
    </button>{/if}
  {#if busy}<button
      class="button button-secondary"
      type="button"
      onclick={onStop}
    >
      Stop validation
    </button>{/if}
</div>
{#if status}<p class="status" role="status">{status}</p>{/if}
{#if failures.length}<ul class="inline-problem" role="alert">
    {#each failures as failure (failure)}<li>
        {failure}
      </li>{/each}
  </ul>{/if}

<style>
  .status {
    margin-top: 1rem;
    color: var(--foreground-muted);
  }
</style>
