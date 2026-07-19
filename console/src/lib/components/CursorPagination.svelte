<script lang="ts">
  let {
    page,
    hasPrevious,
    hasNext,
    onPrevious,
    onNext,
    label = 'Result pages'
  }: {
    page: number;
    hasPrevious: boolean;
    hasNext: boolean;
    onPrevious: () => void;
    onNext: () => void;
    label?: string;
  } = $props();
</script>

<nav class="pagination" aria-label={label}>
  <button class="button button-secondary" type="button" onclick={onPrevious} disabled={!hasPrevious}>Previous</button>
  <span>Page {page}</span>
  <button class="button button-secondary" type="button" onclick={onNext} disabled={!hasNext}>Next</button>
</nav>

<style>
  .pagination { display: flex; min-height: 2.75rem; align-items: center; justify-content: flex-end; gap: .65rem; margin-top: .75rem; }
  .pagination span { min-width: 4rem; color: var(--foreground-muted); font-size: .75rem; text-align: center; }
</style>
