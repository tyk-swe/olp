// The Vercel AI SDK providers pointed at the gateway through the recording
// proxy, shared by the test files of this suite. Each test file runs in its own
// process, so each starts its own proxy and closes it in `after`.
import { createAnthropic } from '@ai-sdk/anthropic';
import { createGoogleGenerativeAI } from '@ai-sdk/google';
import { createOpenAI } from '@ai-sdk/openai';
import { jsonSchema, tool } from 'ai';
import { apiKey, models } from '../../lib/harness.mjs';
import { startTap } from '../../lib/tap.mjs';

export const tap = await startTap();

/**
 * The three providers of one credential. The Anthropic and Google providers
 * take a base URL that already ends in the API version; the OpenAI provider
 * takes the gateway's /v1.
 */
export function vendors(key) {
  return {
    openai: createOpenAI({ apiKey: key, baseURL: tap.baseURLs.openai }),
    anthropic: createAnthropic({ apiKey: key, baseURL: `${tap.baseURLs.anthropic}/v1` }),
    google: createGoogleGenerativeAI({ apiKey: key, baseURL: `${tap.baseURLs.gemini}/v1beta` })
  };
}

export const main = vendors(apiKey);
export const { openai, anthropic, google } = main;

/**
 * The language-model providers, one per wire API a provider can speak. `api`
 * is the API a client request uses; Google has one for unary and one for
 * streamed calls. `model` takes the vendors of another credential to test a
 * refusal, and the route slug to address another vendor's route. `structuredScript` is how the scripted upstream sees a structured
 * output request when the provider does not send a schema: the Anthropic
 * provider asks for JSON through a forced tool call.
 */
export const chatProviders = [
  { name: 'OpenAI Chat Completions', slug: models.openai, model: (v = main, slug = models.openai) => v.openai.chat(slug), api: () => 'openai.chat' },
  { name: 'OpenAI Responses', slug: models.openai, model: (v = main, slug = models.openai) => v.openai.responses(slug), api: () => 'openai.responses' },
  { name: 'Anthropic Messages', slug: models.anthropic, model: (v = main, slug = models.anthropic) => v.anthropic(slug), api: () => 'anthropic.messages', structuredScript: 'tool_call' },
  { name: 'Google Generative AI', slug: models.gemini, model: (v = main, slug = models.gemini) => v.google(slug), api: (stream) => (stream ? 'gemini.stream' : 'gemini.generate') }
];

/** What the tool does, so a schema that arrives mangled is not accepted. */
export const weatherSchema = {
  type: 'object',
  properties: {
    city: { type: 'string', description: 'The city to look up', minLength: 2 },
    units: { type: 'string', enum: ['celsius', 'fahrenheit'] }
  },
  required: ['city'],
  additionalProperties: false
};

/** A city that exercises UTF-8 from the prompt, through the tool arguments, to the result. */
export const city = 'Zürich 東京 🚀';

export const weather = tool({
  description: 'Look up the weather for a city',
  inputSchema: jsonSchema(weatherSchema),
  execute: async ({ city }) => `sunny in ${city}`
});

/** The structured-output schema, and the instance the scripted upstream answers with. */
export const ticketSchema = {
  type: 'object',
  properties: {
    title: { type: 'string' },
    priority: { type: 'string', enum: ['low', 'high'] },
    score: { type: 'integer' },
    done: { type: 'boolean' },
    tags: { type: 'array', items: { type: 'string' } }
  },
  required: ['title', 'priority', 'score', 'done', 'tags'],
  additionalProperties: false
};
export const ticket = { title: 'fixture', priority: 'low', score: 42, done: true, tags: ['fixture'] };
