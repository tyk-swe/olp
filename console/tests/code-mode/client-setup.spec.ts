import AxeBuilder from '@axe-core/playwright';
import { expect, test } from '../playwright';
import { signInGatewayOwner } from '../gateway/signIn';
import {
  account,
  projectId,
  route
} from '../../src/lib/features/code-mode/test/fixtures';
import type {
  CodeClientConfiguration,
  CodeRoute
} from '../../src/lib/api/code-mode';

// The route mixes subscriptions: GLM Coding Plan serves GLM and OpenCode Go
// serves MiniMax.
const models = ['glm-5.3', 'minimax-m3'];
const providers: Record<string, string> = {
  'glm-5.3': 'zai-coding-plan',
  'minimax-m3': 'opencode-go'
};

function configuration(url: URL): CodeClientConfiguration {
  const client = (url.searchParams.get('client') ?? 'claude-code') as
    'claude-code' | 'opencode';
  const model = url.searchParams.get('model') ?? models[0];
  const plan = url.searchParams.get('plan_model') ?? model;
  const small = url.searchParams.get('small_model') ?? model;
  const base = `${url.searchParams.get('gateway_url')}/code/${route.slug}`;
  const openCode = (native: string) => `${providers[native]}/${native}`;
  return {
    route_slug: route.slug,
    base_url: base,
    native_models: models,
    adapters: ['opencode_go', 'zai_coding'],
    client,
    supported_clients: ['claude-code', 'opencode'],
    client_version: client === 'opencode' ? '1.18.34' : '2.1.286',
    model,
    plan_model: plan,
    small_model: small,
    format: client === 'opencode' ? 'json' : 'shell',
    file: client === 'opencode' ? 'opencode.json' : null,
    configuration:
      client === 'opencode'
        ? JSON.stringify(
            {
              model: openCode(model),
              small_model: openCode(small),
              ...(plan !== model && {
                agent: { plan: { model: openCode(plan) } }
              }),
              enabled_providers: ['opencode-go', 'zai-coding-plan'],
              provider: Object.fromEntries(
                ['opencode-go', 'zai-coding-plan'].map((provider) => [
                  provider,
                  {
                    options: {
                      baseURL: `${base}/v1`,
                      apiKey: '{env:OLP_API_KEY}'
                    }
                  }
                ])
              )
            },
            null,
            2
          )
        : [
            `export ANTHROPIC_BASE_URL='${base}'`,
            `export ANTHROPIC_MODEL='${plan === model ? model : 'opusplan'}'`,
            `export ANTHROPIC_DEFAULT_OPUS_MODEL='${plan}'`,
            `export ANTHROPIC_DEFAULT_SONNET_MODEL='${model}'`,
            `export ANTHROPIC_DEFAULT_HAIKU_MODEL='${small}'`
          ].join('\n'),
    qualification_gaps: [
      'Each request carries the conversation so far, so every subscription a conversation mixes receives the turns the others produced.'
    ]
  };
}

test('generates Claude Code and OpenCode setup for a mixed subscription route accessibly', async ({
  page
}, info) => {
  await signInGatewayOwner(page);
  const mixed = {
    ...route,
    models,
    adapters: ['opencode_go', 'zai_coding'] as CodeRoute['adapters']
  };
  const headers = { 'Cache-Control': 'no-store' };
  await page.route('**/api/v1/project-memberships', (fulfil) =>
    fulfil.fulfill({
      headers,
      json: {
        items: [{ id: projectId, name: 'Coding plans', role: 'manager' }]
      }
    })
  );
  await page.route('**/api/v1/code/accounts?**', (fulfil) =>
    fulfil.fulfill({ headers, json: { items: [account], next_cursor: null } })
  );
  await page.route('**/api/v1/code/routes?**', (fulfil) =>
    fulfil.fulfill({
      headers,
      json: { items: [mixed], next_cursor: null }
    })
  );
  await page.route('**/api/v1/code/routes/*/revisions**', (fulfil) =>
    fulfil.fulfill({
      headers,
      json: {
        items: [{ id: route.revision_id, route: mixed }],
        next_cursor: null
      }
    })
  );
  const selections: Array<string | null> = [];
  await page.route('**/api/v1/code/routes/*/client-config?**', (fulfil) => {
    const url = new URL(fulfil.request().url());
    selections.push(url.searchParams.get('client'));
    if (url.searchParams.get('small_model') === 'glm-4.7')
      return fulfil.fulfill({
        status: 422,
        headers: { ...headers, 'Content-Type': 'application/problem+json' },
        json: {
          type: 'https://openllmproxy.dev/problems/validation_failed',
          title: 'Validation failed',
          status: 422,
          detail: 'Choose a native model admitted by the published route.'
        }
      });
    return fulfil.fulfill({ headers, json: configuration(url) });
  });

  await page.goto('/code-mode');
  await expect(page.getByText('GLM Coding Plan').first()).toBeVisible();
  await page.getByRole('button', { name: 'Routes', exact: true }).click();
  await expect(page.getByText('OpenCode Go', { exact: true })).toBeVisible();
  await page
    .getByRole('button', { name: 'Revisions and client setup' })
    .click();
  const setup = page.getByRole('region', { name: 'Client configuration' });
  await expect(setup.getByText('Claude Code 2.1.286')).toBeVisible();
  await expect(setup.getByLabel(/Generated configuration · SHELL/)).toHaveValue(
    /ANTHROPIC_BASE_URL=/
  );
  await expect(
    setup.getByText(
      'Source this file in your shell before starting the client.'
    )
  ).toBeVisible();
  await expect(setup.getByText('Subscriptions')).toBeVisible();
  await setup.getByLabel('Planning model').selectOption('minimax-m3');
  await expect(setup.getByLabel(/Generated configuration/)).toHaveValue(
    /ANTHROPIC_MODEL='opusplan'\nexport ANTHROPIC_DEFAULT_OPUS_MODEL='minimax-m3'/
  );
  await setup.getByLabel('Background model').selectOption('minimax-m3');
  await expect(setup.getByLabel(/Generated configuration/)).toHaveValue(
    /ANTHROPIC_DEFAULT_HAIKU_MODEL='minimax-m3'/
  );
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  await page.screenshot({
    path: info.outputPath('code-client-claude-code.png'),
    fullPage: true
  });

  await setup.getByText('OpenCode', { exact: true }).click();
  await expect(setup.getByText('Save as opencode.json.')).toBeVisible();
  await expect(setup.getByLabel(/Generated configuration · JSON/)).toHaveValue(
    /zai-coding-plan\/glm-5\.3/
  );
  await setup.getByLabel('Planning model').selectOption('minimax-m3');
  await expect(setup.getByLabel(/Generated configuration/)).toHaveValue(
    /"plan": \{\s+"model": "opencode-go\/minimax-m3"/
  );
  expect(selections).toContain('opencode');
  await page.setViewportSize({ width: 390, height: 844 });
  expect((await new AxeBuilder({ page }).analyze()).violations).toEqual([]);
  expect(
    await page.evaluate(
      () => document.documentElement.scrollWidth <= window.innerWidth
    )
  ).toBe(true);
  await page.screenshot({
    path: info.outputPath('code-client-opencode-mobile.png'),
    fullPage: true
  });
});
