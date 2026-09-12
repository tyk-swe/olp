<script lang="ts">
  let { size = 24 }: { size?: number } = $props();
</script>

<svg
  class="mark"
  width={size}
  height={size}
  viewBox="0 0 32 32"
  aria-hidden="true"
>
  <path
    d="M8 10.5h6.25a4 4 0 0 1 4 4v3a4 4 0 0 0 4 4H24M8 21.5h4.25a4 4 0 0 0 4-4v-3a4 4 0 0 1 4-4H24"
    fill="none"
    stroke="currentColor"
    stroke-width="2.5"
    stroke-linecap="round"
  ></path>
  <g class="dots">
    <circle cx="8" cy="10.5" r="2.25"></circle>
    <circle cx="8" cy="21.5" r="2.25"></circle>
    <circle cx="24" cy="10.5" r="2.25"></circle>
    <circle cx="24" cy="21.5" r="2.25"></circle>
  </g>
</svg>

<style>
  .mark {
    display: block;
    flex: none;
    color: var(--foreground);
  }

  .dots {
    fill: var(--signal);
  }

  @media (forced-colors: active) {
    .mark {
      color: CanvasText;
    }

    .dots {
      fill: CanvasText;
    }
  }
</style>
