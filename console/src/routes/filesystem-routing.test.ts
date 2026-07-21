import { describe, expect, it } from 'vitest';
import shell from '../lib/components/AppShell.svelte?raw';
import config from '../../svelte.config.js?raw';
import rootLayout from './+layout.ts?raw';

const pages = import.meta.glob('./**/+page.svelte', {
  eager: true,
  import: 'default',
  query: '?raw'
}) as Record<string, string>;
const layouts = import.meta.glob('./**/+layout.svelte', {
  eager: true,
  import: 'default',
  query: '?raw'
}) as Record<string, string>;

const protectedPages = [
  ['(console)/+page.svelte', 'Overview'],
  ['(console)/providers/+page.svelte', 'ProviderList'],
  ['(console)/providers/new/+page.svelte', 'ProviderWizard'],
  ['(console)/providers/[providerId]/+page.svelte', 'ProviderDetail'],
  ['(console)/models/+page.svelte', 'ModelsPage'],
  ['(console)/routes/+page.svelte', 'RouteList'],
  ['(console)/routes/new/+page.svelte', 'RouteDraftEditor'],
  ['(console)/routes/[routeId]/+page.svelte', 'RouteDraftEditor'],
  ['(console)/routes/[routeId]/revisions/+page.svelte', 'RouteRevisionHistory'],
  ['(console)/api-keys/+page.svelte', 'ApiKeysPage'],
  ['(console)/api-keys/new/+page.svelte', 'ApiKeysPage'],
  ['(console)/requests/+page.svelte', 'RequestsPage'],
  ['(console)/requests/[requestId]/+page.svelte', 'RequestsPage'],
  ['(console)/media-jobs/+page.svelte', 'MediaJobsPage'],
  ['(console)/media-jobs/[jobId]/+page.svelte', 'MediaJobsPage'],
  ['(console)/health/+page.svelte', 'HealthPage'],
  ['(console)/usage/+page.svelte', 'UsagePage'],
  ['(console)/audit/+page.svelte', 'AuditPage'],
  ['(console)/access/+page.svelte', 'AccessPage'],
  ['(console)/playground/+page.svelte', 'PlaygroundPage'],
  ['(console)/settings/+page.svelte', 'SettingsPage'],
  ['(console)/settings/profile/+page.svelte', 'ProfilePage']
] as const;

describe('console filesystem routing', () => {
  it.each(protectedPages)('%s owns %s', (route, component) => {
    const source = pages[`./${route}`];
    expect(source).toContain(`import ${component} from '$lib/features/`);
    expect(source.match(/import \w+ from '\$lib\/features\/.*\.svelte';/g)).toHaveLength(1);
  });

  it('keeps authentication in the protected layout instead of the shell', () => {
    const layout = layouts['./(console)/+layout.svelte'];
    expect(layout).toContain('currentSession');
    expect(layout).toContain('getSetupStatus');
    expect(shell).not.toContain('currentSession');
    expect(shell).not.toContain('getSetupStatus');
  });

  it('uses the static SPA fallback without the eager dispatcher', () => {
    expect(config).toContain("fallback: 'index.html'");
    expect(rootLayout).toContain('export const ssr = false');
    expect(pages['./[...path]/+page.svelte']).toBeUndefined();
    expect(pages['./(console)/[...path]/+page.svelte']).not.toContain('$lib/features/');
  });
});
