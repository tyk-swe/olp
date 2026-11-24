<script lang="ts">
  import { ICONS, type IconName } from '#lib/shell/icons.js';

  let { name, size = 20 }: { name: IconName; size?: number } = $props();
</script>

<svg
  class="icon icon-{name}"
  width={size}
  height={size}
  viewBox="0 0 24 24"
  fill="none"
  stroke="currentColor"
  stroke-width="1.8"
  stroke-linecap="round"
  stroke-linejoin="round"
  aria-hidden="true"
>
  <path d={ICONS[name]}></path>
</svg>
