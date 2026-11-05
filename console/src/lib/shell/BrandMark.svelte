<script lang="ts">
  let { size = 24, animated = false }: { size?: number; animated?: boolean } =
    $props();
</script>

<svg
  class="mark"
  class:animated
  width={size}
  height={size}
  viewBox="0 0 32 32"
  aria-hidden="true"
>
  <g
    fill="none"
    stroke="currentColor"
    stroke-width="2.5"
    stroke-linecap="round"
  >
    <path
      class="route"
      pathLength="1"
      d="M8 10.5h6.25a4 4 0 0 1 4 4v3a4 4 0 0 0 4 4H24"
    ></path>
    <path
      class="route"
      pathLength="1"
      d="M8 21.5h4.25a4 4 0 0 0 4-4v-3a4 4 0 0 1 4-4H24"
    ></path>
  </g>
  <g class="dots">
    <circle cx="8" cy="10.5" r="2.25"></circle>
    <circle cx="8" cy="21.5" r="2.25"></circle>
    <circle class="target" cx="24" cy="10.5" r="2.25"></circle>
    <circle class="target" cx="24" cy="21.5" r="2.25"></circle>
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

  circle {
    transform-box: fill-box;
    transform-origin: center;
  }

  /* Hovering the surrounding link re-traces both routes once. */
  :global(a:hover) > .mark .route {
    stroke-dasharray: 1;
    animation: route-draw 700ms var(--ease-out);
  }

  /* The loader: routes draw from the origin dots to the targets and retract,
     and each end lights up as the signal passes through it. */
  .animated .route {
    stroke-dasharray: 1;
    animation: route-loop 2.4s var(--ease-in-out) infinite;
  }

  .animated circle {
    animation: dot-pulse 2.4s var(--ease-out) infinite;
  }

  .animated .target {
    animation-delay: 1.08s;
  }

  @keyframes route-draw {
    from {
      stroke-dashoffset: 1;
    }
    to {
      stroke-dashoffset: 0;
    }
  }

  @keyframes route-loop {
    0% {
      stroke-dashoffset: 1;
    }
    45%,
    55% {
      stroke-dashoffset: 0;
    }
    100% {
      stroke-dashoffset: -1;
    }
  }

  @keyframes dot-pulse {
    0%,
    30%,
    100% {
      transform: scale(1);
    }
    12% {
      transform: scale(1.45);
    }
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
