<script lang="ts">
  export type StatusTone =
    'neutral' | 'accent' | 'success' | 'warning' | 'danger';

  let { label, tone = 'neutral' }: { label: string; tone?: StatusTone } =
    $props();
</script>

<span
  class="badge"
  class:accent={tone === 'accent'}
  class:success={tone === 'success'}
  class:warning={tone === 'warning'}
  class:danger={tone === 'danger'}>{label}</span
>
