import type { Page } from '@playwright/test';

const apiKeyAuth = [
  { mode: 'api_key', label: 'Stored API key', credential: 'required' }
];

const seedModel = { field: 'model', label: 'Seed model', required: false };

// Browser tests mock the management API boundary. Keep this deliberately
// minimal: the UI must consume the response rather than importing a production
// fallback capability matrix.
const providerKinds = {
  items: [
    {
      kind: 'openai',
      label: 'OpenAI',
      description: 'Official OpenAI HTTPS API',
      default_auth_mode: 'api_key',
      auth_modes: apiKeyAuth,
      fields: [seedModel]
    },
    {
      kind: 'openai_compatible',
      label: 'OpenAI-compatible',
      description: 'Explicit custom HTTPS endpoint',
      default_auth_mode: 'api_key',
      auth_modes: apiKeyAuth,
      fields: [
        { field: 'endpoint', label: 'HTTPS endpoint', required: true },
        seedModel
      ]
    }
  ]
};

export async function mockProviderKinds(page: Page) {
  await page.route('**/api/v1/provider-kinds', async (route) => {
    await route.fulfill({ json: providerKinds });
  });
}
