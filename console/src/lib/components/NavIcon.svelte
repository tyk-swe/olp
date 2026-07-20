<script lang="ts">
  import type { IconName } from './icons';

  let { name, size = 20 }: { name: IconName; size?: number } = $props();
</script>

<svg
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
  {#if name === 'overview'}
    <rect x="3" y="3" width="7" height="7" rx="1.5"></rect>
    <rect x="14" y="3" width="7" height="7" rx="1.5"></rect>
    <rect x="3" y="14" width="7" height="7" rx="1.5"></rect>
    <rect x="14" y="14" width="7" height="7" rx="1.5"></rect>
  {:else if name === 'provider'}
    <path d="M7 7h10v10H7z"></path><path d="M9 3v4m6-4v4M9 17v4m6-4v4M3 9h4m10 0h4M3 15h4m10 0h4"></path>
  {:else if name === 'model'}
    <path d="m12 3 8 4.5-8 4.5-8-4.5L12 3Z"></path><path d="m4 12 8 4.5 8-4.5M4 16.5l8 4.5 8-4.5"></path>
  {:else if name === 'route'}
    <circle cx="5" cy="6" r="2"></circle><circle cx="19" cy="6" r="2"></circle><circle cx="19" cy="18" r="2"></circle><path d="M7 6h3a4 4 0 0 1 4 4v4a4 4 0 0 0 3 3.9M14 10a4 4 0 0 1 3-3.9"></path>
  {:else if name === 'key'}
    <circle cx="8" cy="15" r="4"></circle><path d="m11 12 8-8m-3 3 2 2m-5 1 2 2"></path>
  {:else if name === 'access'}
    <circle cx="9" cy="8" r="3"></circle><path d="M3.5 19a5.5 5.5 0 0 1 11 0M16 5.5a3 3 0 0 1 0 5.8M17 14a4.5 4.5 0 0 1 3.5 4.4"></path>
  {:else if name === 'request'}
    <path d="M6 3h9l4 4v14H6z"></path><path d="M14 3v5h5M9 13h7M9 17h5"></path>
  {:else if name === 'usage'}
    <path d="M4 20V10m5 10V4m6 16v-7m5 7V7"></path>
  {:else if name === 'health'}
    <path d="M3 12h4l2-6 4 12 2-6h6"></path>
  {:else if name === 'audit'}
    <path d="M7 3h10v4H7zM5 5H3v16h18V5h-2M8 12h8M8 16h6"></path>
  {:else if name === 'playground'}
    <path d="m9 3-1 4-4 1 4 1 1 4 1-4 4-1-4-1-1-4ZM17 12l-.8 3.2L13 16l3.2.8L17 20l.8-3.2L21 16l-3.2-.8L17 12Z"></path>
  {:else if name === 'settings'}
    <circle cx="12" cy="12" r="3"></circle><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.9l.1.1-2.8 2.8-.1-.1a1.7 1.7 0 0 0-1.9-.3 1.7 1.7 0 0 0-1 1.6v.2h-4V21a1.7 1.7 0 0 0-1-1.6 1.7 1.7 0 0 0-1.9.3l-.1.1L4.2 17l.1-.1a1.7 1.7 0 0 0 .3-1.9A1.7 1.7 0 0 0 3 14H2.8v-4H3a1.7 1.7 0 0 0 1.6-1 1.7 1.7 0 0 0-.3-1.9L4.2 7 7 4.2l.1.1a1.7 1.7 0 0 0 1.9.3A1.7 1.7 0 0 0 10 3V2.8h4V3a1.7 1.7 0 0 0 1 1.6 1.7 1.7 0 0 0 1.9-.3l.1-.1L19.8 7l-.1.1a1.7 1.7 0 0 0-.3 1.9 1.7 1.7 0 0 0 1.6 1h.2v4H21a1.7 1.7 0 0 0-1.6 1Z"></path>
  {:else if name === 'menu'}
    <path d="M4 7h16M4 12h16M4 17h16"></path>
  {:else if name === 'sun'}
    <circle cx="12" cy="12" r="4"></circle><path d="M12 2v2m0 16v2M4.9 4.9l1.4 1.4m11.4 11.4 1.4 1.4M2 12h2m16 0h2M4.9 19.1l1.4-1.4M17.7 6.3l1.4-1.4"></path>
  {:else if name === 'moon'}
    <path d="M20 15.2A8 8 0 0 1 8.8 4a8 8 0 1 0 11.2 11.2Z"></path>
  {:else if name === 'chevron'}
    <path d="m8 10 4 4 4-4"></path>
  {:else if name === 'arrow'}
    <path d="M5 12h14m-5-5 5 5-5 5"></path>
  {/if}
</svg>
