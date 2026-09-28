import { flushSync, mount, unmount } from 'svelte';
import { afterEach, expect, it } from 'vitest';
import ProviderRevisionComparison from './ProviderRevisionComparison.svelte';
import type { ProviderRevisionDiff } from './revisions';

const unchanged: ProviderRevisionDiff = {
  from_revision: 1,
  to_revision: 2,
  name_changed: false,
  endpoint_changed: false,
  cloud_context_changed: false,
  deployment_changed: false,
  api_version_changed: false,
  connector_changed: false,
  credential_changed: false,
  models_added: [],
  models_removed: [],
  models_changed: [],
  capabilities_added: [],
  capabilities_removed: [],
  profile_changed: false,
  plugin_changed: false,
  plugin_options_changed: false,
  semantic_configuration_changed: false,
  serving_binding_changed: false,
  network_configuration_changed: false
};

let host: HTMLElement | undefined;
let component: ReturnType<typeof mount> | undefined;

afterEach(async () => {
  if (component) await unmount(component);
  host?.remove();
});

function flags(revisionDiff: ProviderRevisionDiff) {
  host = document.createElement('div');
  document.body.append(host);
  component = mount(ProviderRevisionComparison, {
    target: host,
    props: { revisionDiff }
  });
  flushSync();
  return [...host.querySelectorAll('.diff-flags li')].map((item) =>
    item.textContent?.trim()
  );
}

it('shows a move to another plugin build', () => {
  expect(
    flags({ ...unchanged, profile_changed: true, plugin_changed: true })
  ).toEqual(['Provider profile changed', 'Plugin digest changed']);
});

it('shows a change of plugin options', () => {
  expect(
    flags({
      ...unchanged,
      endpoint_changed: true,
      plugin_options_changed: true
    })
  ).toEqual(['Endpoint changed', 'Plugin options changed']);
});
