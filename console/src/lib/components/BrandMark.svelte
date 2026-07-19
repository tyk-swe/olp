<script lang="ts">
  let {
    size = 32,
    decorative = true,
    label = 'OpenLLMProxy routing mark'
  }: { size?: number; decorative?: boolean; label?: string } = $props();
</script>

<svg
  class="mark"
  width={size}
  height={size}
  viewBox="0 0 32 32"
  role={decorative ? undefined : 'img'}
  aria-hidden={decorative ? 'true' : undefined}
  aria-label={decorative ? undefined : label}
>
  <rect width="32" height="32" rx="6" fill="currentColor"></rect>
  <path
    d="M8 10.5h6.25a4 4 0 0 1 4 4v3a4 4 0 0 0 4 4H24M8 21.5h4.25a4 4 0 0 0 4-4v-3a4 4 0 0 1 4-4H24"
    fill="none"
    stroke="var(--mark-line, white)"
    stroke-width="2.5"
    stroke-linecap="round"
  ></path>
  <g fill="var(--mark-line, white)">
    <circle cx="8" cy="10.5" r="2"></circle>
    <circle cx="8" cy="21.5" r="2"></circle>
    <circle cx="24" cy="10.5" r="2"></circle>
    <circle cx="24" cy="21.5" r="2"></circle>
  </g>
</svg>

<style>
  .mark {
    display: block;
    flex: none;
    color: var(--accent);
  }

  @media (forced-colors: active) {
    .mark {
      --mark-line: Canvas;
      color: CanvasText;
    }
  }
</style>
