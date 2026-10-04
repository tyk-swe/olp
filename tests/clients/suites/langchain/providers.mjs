// The LangChain chat models pointed at the gateway through the recording proxy,
// shared by the test files of this suite. Each test file runs in its own
// process, so each starts its own proxy and closes it in `after`.
import { ChatAnthropic } from '@langchain/anthropic';
import { tool } from '@langchain/core/tools';
import { ChatGoogleGenerativeAI } from '@langchain/google-genai';
import { ChatOpenAI } from '@langchain/openai';
import { apiKey, models } from '../../lib/harness.mjs';
import { namedModels } from '../../lib/relay.mjs';
import { startTap } from '../../lib/tap.mjs';

export const tap = await startTap();

// Retries are off everywhere: the suites share one gateway, and a client that
// retried an error would repeat it against the provider's circuit breaker.
const noRetries = { maxRetries: 0 };

/**
 * The chat models, one per wire API a LangChain provider can speak. `create`
 * takes `temperature`, `maxTokens`, the credential `key`, the route `slug` to
 * address another vendor's route and any other constructor option. `api` is the API a
 * request uses; Google has one for unary and one for streamed calls.
 * `structuredScript` is how the scripted upstream sees a structured output
 * request when it is not a schema: ChatAnthropic asks for JSON through a forced
 * tool call.
 */
export const chatProviders = [
  {
    name: 'ChatOpenAI (Chat Completions)',
    slug: models.openai,
    api: () => 'openai.chat',
    create: ({ temperature, maxTokens, key = apiKey, slug = models.openai, ...extra } = {}) =>
      new ChatOpenAI({ model: slug, apiKey: key, temperature, maxTokens, configuration: { baseURL: tap.baseURLs.openai }, ...noRetries, ...extra })
  },
  {
    name: 'ChatOpenAI (Responses)',
    slug: models.openai,
    api: () => 'openai.responses',
    create: ({ temperature, maxTokens, key = apiKey, slug = models.openai, ...extra } = {}) =>
      new ChatOpenAI({ model: slug, apiKey: key, temperature, maxTokens, useResponsesApi: true, configuration: { baseURL: tap.baseURLs.openai }, ...noRetries, ...extra })
  },
  {
    name: 'ChatAnthropic',
    slug: models.anthropic,
    api: () => 'anthropic.messages',
    structuredScript: 'tool_call',
    create: ({ temperature, maxTokens = 1024, key = apiKey, slug = models.anthropic, ...extra } = {}) =>
      new ChatAnthropic({ model: slug, apiKey: key, temperature, maxTokens, anthropicApiUrl: tap.baseURLs.anthropic, ...noRetries, ...extra })
  },
  {
    name: 'ChatGoogleGenerativeAI',
    // The model accepts images only for a name it recognizes as multimodal, such
    // as gemini-2.5-flash, so the route is named after the model it replaces.
    slug: namedModels.gemini,
    imagesNeedKnownModelName: true,
    api: (stream) => (stream ? 'gemini.stream' : 'gemini.generate'),
    create: ({ temperature, maxTokens, key = apiKey, slug = namedModels.gemini, ...extra } = {}) =>
      new ChatGoogleGenerativeAI({ model: slug, apiKey: key, temperature, maxOutputTokens: maxTokens, baseUrl: tap.baseURLs.gemini, ...noRetries, ...extra })
  }
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

export const weather = tool(async ({ city }) => `sunny in ${city}`, {
  name: 'get_weather',
  description: 'Look up the weather for a city',
  schema: weatherSchema
});

/** The structured-output schema, and the instance the scripted upstream answers with. */
export const ticketSchema = {
  type: 'object',
  title: 'ticket',
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
