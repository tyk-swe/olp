// The LlamaIndex.TS LLMs pointed at the gateway through the recording proxy,
// shared by the test files of this suite. Each test file runs in its own
// process, so each starts its own proxy and closes it in `after`.
import { Anthropic, AnthropicSession } from '@llamaindex/anthropic';
import { Gemini } from '@llamaindex/google';
import { OpenAI, OpenAIResponses } from '@llamaindex/openai';
import { tool } from 'llamaindex';
import { apiKey, models } from '../../lib/harness.mjs';
import { namedModels } from '../../lib/relay.mjs';
import { startTap } from '../../lib/tap.mjs';

export const tap = await startTap();

// Retries are off everywhere: the suites share one gateway, and LlamaIndex
// retries ten times by default, which would repeat an error against the
// provider's circuit breaker.
const noRetries = { maxRetries: 0 };

/**
 * The LLMs, one per wire API a LlamaIndex provider can speak. `create` takes
 * `temperature`, `maxTokens`, the credential `key` and the route `slug` to
 * address another vendor's route. `api` is the API a
 * request uses; Google has one for unary and one for streamed calls.
 *
 * LlamaIndex.TS enables tool calling only for model names it recognizes: any
 * name for OpenAI, a name with `-3` or `-4` for Anthropic and a listed
 * Gemini name for Gemini. The Anthropic and Gemini routes are therefore named
 * after the model, as an operator names a route after the model it replaces.
 * Anthropic also needs an explicit session to take a base URL.
 *
 * `streams`, `agent`, `sendsTemperature` and `translates` are false where the
 * client cannot be qualified for that feature, with the reason beside the entry.
 */
export const chatProviders = [
  {
    name: 'OpenAI (Chat Completions)',
    slug: models.openai,
    api: () => 'openai.chat',
    create: ({ temperature, maxTokens, key = apiKey, slug = models.openai } = {}) => new OpenAI({ model: slug, apiKey: key, baseURL: tap.baseURLs.openai, temperature, maxTokens, ...noRetries })
  },
  {
    name: 'OpenAIResponses',
    slug: models.openai,
    api: () => 'openai.responses',
    // Open items, recorded in docs/clients.md. The client repeats the last text
    // delta once for each event that follows it, and every agent step streams,
    // so streaming and the agent return corrupted text whatever the server is.
    // It also drops the temperature of a model name it does not recognize.
    streams: false,
    agent: false,
    sendsTemperature: false,
    // And it sends `store: false` on every request, which a route that
    // translates to another vendor refuses: that vendor has no such parameter.
    translates: false,
    create: ({ temperature, maxTokens, key = apiKey, slug = models.openai } = {}) =>
      new OpenAIResponses({ model: slug, apiKey: key, baseURL: tap.baseURLs.openai, temperature, maxOutputTokens: maxTokens, ...noRetries })
  },
  {
    name: 'Anthropic',
    slug: namedModels.anthropic,
    api: () => 'anthropic.messages',
    create: ({ temperature, maxTokens = 1024, key = apiKey, slug = namedModels.anthropic } = {}) =>
      new Anthropic({
        model: slug,
        apiKey: key,
        temperature,
        maxTokens,
        session: new AnthropicSession({ apiKey: key, baseURL: tap.baseURLs.anthropic, ...noRetries }),
        ...noRetries
      })
  },
  {
    name: 'Gemini',
    slug: namedModels.gemini,
    api: (stream) => (stream ? 'gemini.stream' : 'gemini.generate'),
    // The class sends safety settings that switch the filters off unless told
    // otherwise, which only Gemini can honor: pass none for another vendor.
    translationOptions: { safetySettings: [] },
    create: ({ temperature, maxTokens, key = apiKey, slug = namedModels.gemini, ...extra } = {}) =>
      new Gemini({ model: slug, apiKey: key, temperature, maxTokens, httpOptions: { baseUrl: tap.baseURLs.gemini }, ...extra })
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

export const weather = tool({
  name: 'get_weather',
  description: 'Look up the weather for a city',
  parameters: weatherSchema,
  execute: async ({ city }) => `sunny in ${city}`
});
