import type { Page } from '../playwright';

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
      fields: [seedModel],
      presets: []
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
      ],
      presets: [
        {
          id: 'groq',
          label: 'Groq',
          description:
            "Low-latency inference through Groq's OpenAI-compatible API.",
          endpoint: 'https://api.groq.com/openai/v1',
          auth_mode: 'api_key',
          maintainer: 'Groq',
          documentation_label: 'OpenAI Compatibility',
          documentation_url: 'https://console.groq.com/docs/openai'
        },
        {
          id: 'mistral_ai',
          label: 'Mistral AI',
          description:
            'Mistral models through the official OpenAI-compatible API surface.',
          endpoint: 'https://api.mistral.ai/v1',
          auth_mode: 'api_key',
          maintainer: 'Mistral AI',
          documentation_label: 'Migration from OpenAI',
          documentation_url:
            'https://docs.mistral.ai/resources/migration-guides'
        }
      ]
    }
  ]
};

export async function mockProviderKinds(page: Page) {
  await page.route('**/api/v1/provider-kinds', async (route) => {
    await route.fulfill({ json: providerKinds });
  });
}
