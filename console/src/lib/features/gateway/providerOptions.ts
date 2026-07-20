import type { ProviderKind } from '$lib/api/management/providers';

export const connectorOptions = [
  ['openai', 'OpenAI', 'Official OpenAI HTTPS API'],
  ['anthropic', 'Anthropic', 'Native Messages API'],
  ['gemini', 'Gemini Developer API', 'Google AI API key'],
  ['vertex_ai', 'Vertex AI', 'Google Cloud identity'],
  ['bedrock', 'AWS Bedrock', 'AWS default chain or static credentials'],
  ['azure_openai', 'Azure OpenAI', 'Azure deployment endpoint'],
  ['openai_compatible', 'OpenAI-compatible', 'Explicit custom HTTPS endpoint']
] as const satisfies readonly (readonly [ProviderKind, string, string])[];
