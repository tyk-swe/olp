import { flushSync, mount, unmount } from 'svelte';
import { expect, it, onTestFinished, vi } from 'vitest';
import { json, route, serve } from '#lib/session/test/transport.js';
import { eventually } from '#test/eventually.js';
import RouteFixtureChecks from './RouteFixtureChecks.svelte';
import type { RouteFixturePlan, RouteFixtureReport } from './api.js';

it('compares the selected local file and shows the assertion and omitted classifier verdict', async () => {
  const sample = JSON.stringify({
    synthetic: true,
    fixtures: [
      {
        name: 'Structured example',
        operation: 'generation',
        surface: 'openai',
        mode: 'unary',
        request: {
          model: 'sample',
          messages: [{ role: 'user', content: 'Green square' }]
        },
        seed: 'sample',
        expect_eligible: true
      }
    ]
  });
  const published: RouteFixturePlan = {
    winner: {
      route: 'sample',
      model: 'sample-model',
      provider_id: 'example',
      provider_name: 'Sample provider'
    },
    reason: null,
    targets: [],
    selectors: [],
    fallbacks: []
  };
  const report: RouteFixtureReport = {
    published_revision_id: 'revision',
    published_revision: 3,
    draft_etag: 'current',
    planning_time: '2026-01-01T00:00:00Z',
    live_supply: 'not_simulated',
    passed: false,
    results: [
      {
        name: 'Structured example',
        published,
        draft: {
          ...published,
          winner: null,
          reason: 'no_eligible_targets',
          selectors: [
            {
              route: 'sample',
              id: 'classifier',
              matched: false,
              outcome: 'classifier_not_simulated'
            }
          ]
        },
        changes: ['winner', 'eligibility', 'selector'],
        expect_eligible: true,
        passed: false
      }
    ]
  };
  const compare = vi.fn(async (request: Request) => {
    route(request, 'POST', '/api/v1/route-drafts/example/check-fixtures');
    expect(await request.text()).toBe(sample);
    return json(report);
  });
  serve(compare);
  const target = document.createElement('div');
  document.body.append(target);
  const component = mount(RouteFixtureChecks, {
    target,
    props: { draftId: 'example', draftEtag: 'current', disabled: false }
  });
  onTestFinished(async () => {
    await unmount(component);
    target.remove();
  });
  const picker = target.querySelector<HTMLInputElement>('input[type=file]')!;
  const button = target.querySelector('button')!;
  expect(button.disabled).toBe(true);

  const select = (file: File) => {
    Object.defineProperty(picker, 'files', {
      value: [file],
      configurable: true
    });
    picker.dispatchEvent(new Event('change', { bubbles: true }));
    flushSync();
  };
  select(new File(['a'.repeat(256 * 1024 + 1)], 'oversized.json'));
  expect(target.querySelector('[role=alert]')?.textContent).toContain(
    '256 KiB'
  );
  expect(button.disabled).toBe(true);
  expect(compare).not.toHaveBeenCalled();

  const file = new File([sample], 'fixtures.json', {
    type: 'application/json'
  });
  Object.defineProperty(file, 'text', { value: async () => sample });
  select(file);
  expect(compare).not.toHaveBeenCalled();
  expect(target.querySelector('[role=alert]')).toBeNull();
  expect(button.disabled).toBe(false);
  button.click();

  await eventually(() =>
    expect(target.textContent).toContain('Fixture assertions failed')
  );
  expect(target.textContent).toContain('Structured example');
  expect(target.textContent).toContain('Classifier verdict omitted');
  expect(target.textContent).toContain('Sample provider / sample-model');
  expect(target.textContent).not.toContain('Green square');
});
