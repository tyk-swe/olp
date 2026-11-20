<script lang="ts">
  import { useBranding } from './branding';

  const branding = useBranding();
  const steps = ['300', '400', '500', '600', '700'] as const;
  const icon = document.querySelector<HTMLLinkElement>('link[rel="icon"]');
  const defaultIcon = icon?.href ?? '';

  $effect(() => {
    const scale = branding.data?.accent_scale;
    const root = document.documentElement.style;
    steps.forEach((step) =>
      scale
        ? root.setProperty(`--color-accent-${step}`, scale[step])
        : root.removeProperty(`--color-accent-${step}`)
    );
  });

  $effect(() => {
    if (icon) icon.href = branding.data?.logo || defaultIcon;
  });
</script>
