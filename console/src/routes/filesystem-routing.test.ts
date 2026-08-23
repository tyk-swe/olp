import { expect, it } from 'vitest';
import config from '../../svelte.config.js?raw';
import rootLayout from './+layout.ts?raw';

const pages = import.meta.glob('./**/+page.svelte', {
  eager: true,
  import: 'default',
  query: '?raw'
}) as Record<string, string>;

it('uses the static SPA fallback without the eager dispatcher', () => {
  expect(config).toContain("fallback: 'index.html'");
  expect(rootLayout).toContain('export const ssr = false');
  expect(pages['./[...path]/+page.svelte']).toBeUndefined();
  expect(pages['./(console)/[...path]/+page.svelte']).not.toContain(
    '$lib/features/'
  );
});
