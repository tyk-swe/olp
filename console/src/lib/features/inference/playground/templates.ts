import type { PlaygroundOperation } from '$lib/features/inference/playground/api';

export type PlaygroundTemplate = {
  key: string;
  label: string;
  operation: PlaygroundOperation;
  surface?: 'openai' | 'anthropic' | 'gemini';
  request: Record<string, unknown>;
};

export const playgroundTemplates: PlaygroundTemplate[] = [
  {
    key: 'generation-multiturn',
    label: 'Generation · multi-turn',
    operation: 'generation',
    request: {
      model: '',
      messages: [
        { role: 'system', content: 'You are a concise assistant.' },
        { role: 'user', content: 'What is a media job?' },
        { role: 'assistant', content: 'An asynchronous provider-side task.' },
        { role: 'user', content: 'Summarize that in three words.' }
      ]
    }
  },
  {
    key: 'generation-tool-result',
    label: 'Generation · tool result',
    operation: 'generation',
    request: {
      model: '',
      messages: [
        { role: 'user', content: 'What is the weather in Paris?' },
        {
          role: 'assistant',
          tool_calls: [
            {
              id: 'call_weather',
              type: 'function',
              function: {
                name: 'get_weather',
                arguments: '{"city":"Paris"}'
              }
            }
          ]
        },
        {
          role: 'tool',
          tool_call_id: 'call_weather',
          content: '{"temp_c":18,"condition":"clear"}'
        }
      ],
      tools: [
        {
          type: 'function',
          function: {
            name: 'get_weather',
            description: 'Get weather for a city',
            parameters: {
              type: 'object',
              properties: { city: { type: 'string' } },
              required: ['city']
            }
          }
        }
      ]
    }
  },
  {
    key: 'generation-multimodal',
    label: 'Generation · multimodal',
    operation: 'generation',
    request: {
      model: '',
      messages: [
        {
          role: 'user',
          content: [
            { type: 'text', text: 'Describe this image.' },
            {
              type: 'image_url',
              image_url: {
                url: 'data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=='
              }
            }
          ]
        }
      ]
    }
  },
  {
    key: 'anthropic-messages',
    label: 'Generation · Anthropic messages',
    operation: 'generation',
    surface: 'anthropic',
    request: {
      model: '',
      max_tokens: 1024,
      system: 'You are a concise assistant.',
      messages: [{ role: 'user', content: 'Say hello in one sentence.' }]
    }
  },
  {
    key: 'gemini-generate',
    label: 'Generation · Gemini generate',
    operation: 'generation',
    surface: 'gemini',
    request: {
      contents: [
        { role: 'user', parts: [{ text: 'Say hello in one sentence.' }] }
      ]
    }
  },
  {
    key: 'embeddings',
    label: 'Embeddings',
    operation: 'embeddings',
    request: {
      model: '',
      input: ['First document to embed', 'Second document to embed']
    }
  },
  {
    key: 'moderation',
    label: 'Moderation',
    operation: 'moderation',
    request: {
      model: '',
      input: 'Text to classify for policy categories.'
    }
  },
  {
    key: 'rerank',
    label: 'Rerank',
    operation: 'rerank',
    request: {
      model: '',
      query: 'asynchronous media lifecycle',
      documents: [
        'A media job tracks provider-side generation state.',
        'Static assets are served from the edge.',
        'Reconciliation refreshes job progress.'
      ]
    }
  },
  {
    key: 'token-count',
    label: 'Token count',
    operation: 'token_count',
    request: {
      model: '',
      messages: [{ role: 'user', content: 'Count the tokens in this message.' }]
    }
  }
];

export function templateFor(key: string): PlaygroundTemplate | undefined {
  return playgroundTemplates.find((template) => template.key === key);
}
