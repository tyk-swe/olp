<script lang="ts">
  // Decorative: clients on the left reach every provider on the right through
  // one gateway node, a 68px square centred on (260, 150), with signal packets
  // travelling each route. Client chips start at x=0 and provider chips at
  // x=396; each route leaves or enters its chip at the port on the inner edge.
  const clients = [
    ['Services', 78],
    ['Agents', 150],
    ['SDK clients', 222]
  ] as const;
  const providers = [
    ['OpenAI', 48],
    ['Anthropic', 116],
    ['Gemini', 184],
    ['Bedrock', 252]
  ] as const;
  const chips = [
    ...clients.map(([label, y], index) => ({
      label,
      y,
      x: 0,
      port: 124,
      delay: index * 0.7,
      d: `M124 ${y} C176 ${y} 174 150 226 150`
    })),
    ...providers.map(([label, y], index) => ({
      label,
      y,
      x: 396,
      port: 396,
      delay: 0.9 + index * 0.55,
      d: `M294 150 C346 150 344 ${y} 396 ${y}`
    }))
  ];
</script>

<svg class="diagram" viewBox="0 0 520 300" aria-hidden="true">
  {#each chips as chip (chip.label)}
    <path class="route" d={chip.d}></path>
    <path
      class="packet"
      pathLength="1"
      d={chip.d}
      style="animation-delay: {chip.delay}s"
    ></path>
  {/each}

  {#each chips as chip (chip.label)}
    <g class="chip">
      <rect x={chip.x} y={chip.y - 14} width="124" height="28" rx="3"></rect>
      <text x={chip.x + 12} y={chip.y + 3.5}>{chip.label}</text>
      <circle class="port" cx={chip.port} cy={chip.y} r="2.5"></circle>
    </g>
  {/each}

  <g class="hub">
    <rect class="hub-pulse" x="226" y="116" width="68" height="68" rx="10"
    ></rect>
    <rect class="hub-body" x="226" y="116" width="68" height="68" rx="10"
    ></rect>
    <g transform="translate(240 130) scale(1.25)">
      <path
        class="hub-mark"
        d="M8 10.5h6.25a4 4 0 0 1 4 4v3a4 4 0 0 0 4 4H24M8 21.5h4.25a4 4 0 0 0 4-4v-3a4 4 0 0 1 4-4H24"
      ></path>
      <circle class="hub-dot" cx="8" cy="10.5" r="2.25"></circle>
      <circle class="hub-dot" cx="8" cy="21.5" r="2.25"></circle>
      <circle class="hub-dot" cx="24" cy="10.5" r="2.25"></circle>
      <circle class="hub-dot" cx="24" cy="21.5" r="2.25"></circle>
    </g>
    <text class="hub-label" x="260" y="204">OpenLLMProxy</text>
  </g>
</svg>

<style>
  .diagram {
    width: 100%;
    height: auto;
    overflow: visible;
  }

  text {
    fill: var(--foreground-subtle);
    font-family: var(--font-mono);
    font-size: 10.5px;
    letter-spacing: 0.04em;
    text-transform: uppercase;
  }

  .route {
    fill: none;
    stroke: var(--border);
    stroke-width: 1;
  }

  /* One short dash per route, travelling from its start to its end. */
  .packet {
    fill: none;
    stroke: var(--signal);
    stroke-dasharray: 0.14 1;
    stroke-dashoffset: 0.14;
    stroke-linecap: round;
    stroke-width: 2;
    animation: packet 3.2s var(--ease-in-out) infinite backwards;
  }

  .chip rect {
    fill: var(--surface);
    stroke: var(--border);
  }

  .port {
    fill: var(--signal);
  }

  .hub-body {
    fill: var(--surface-raised);
    stroke: color-mix(in srgb, var(--signal) 55%, var(--border));
  }

  .hub-pulse {
    fill: none;
    stroke: var(--signal);
    opacity: 0;
    transform-box: fill-box;
    transform-origin: center;
    animation: hub-pulse 3.2s var(--ease-out) 0.6s infinite;
  }

  .hub-mark {
    fill: none;
    stroke: var(--foreground);
    stroke-linecap: round;
    stroke-width: 2.5;
  }

  .hub-dot {
    fill: var(--signal);
  }

  .hub-label {
    fill: var(--foreground-muted);
    font-size: 9.5px;
    letter-spacing: 0.08em;
    text-anchor: middle;
  }

  @keyframes packet {
    from {
      stroke-dashoffset: 0.14;
    }
    to {
      stroke-dashoffset: -1;
    }
  }

  @keyframes hub-pulse {
    0% {
      opacity: 0.7;
      transform: scale(1);
    }
    60%,
    100% {
      opacity: 0;
      transform: scale(1.5);
    }
  }

  @media (prefers-reduced-motion: reduce) {
    .packet,
    .hub-pulse {
      display: none;
    }
  }

  @media (forced-colors: active) {
    .packet,
    .hub-pulse {
      display: none;
    }
  }
</style>
