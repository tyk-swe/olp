<script lang="ts">
  import CursorPagination from '#lib/components/CursorPagination.svelte';
  import {
    cursorPaginationProps,
    type CursorHistory
  } from '#lib/listing/pagination.js';

  let {
    pagination,
    query,
    label
  }: {
    pagination: CursorHistory;
    query: {
      data?: { nextCursor?: string | null };
      isError: boolean;
      isFetching: boolean;
      refetch: () => Promise<unknown>;
    };
    label: string;
  } = $props();
</script>

<div class="list-controls">
  <button
    class="button button-secondary"
    type="button"
    disabled={query.isFetching}
    onclick={() => query.refetch()}
  >
    Refresh
  </button>
  <CursorPagination
    {...cursorPaginationProps(
      pagination,
      query.isError || query.isFetching ? null : query.data?.nextCursor
    )}
    {label}
  />
</div>

<style>
  .list-controls {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: flex-end;
    gap: 0.65rem;
    margin-top: 0.75rem;
  }
  .list-controls :global(.pagination) {
    margin-top: 0;
  }
</style>
