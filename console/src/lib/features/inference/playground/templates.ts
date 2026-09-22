import type { PlaygroundOperation } from '$lib/features/inference/playground/api';

export type PlaygroundTemplate = {
  key: string;
  label: string;
  operation: PlaygroundOperation;
  surface?: 'openai' | 'anthropic' | 'gemini';
  nativeDialect?: string;
  request: Record<string, unknown>;
};

export const playgroundTemplates: PlaygroundTemplate[] = [
  {
    key: 'generation-negotiated-tools',
    label: 'Generation · negotiated two-tool workflow',
    operation: 'generation',
    surface: 'openai',
    request: {
      model: '',
      messages: [
        {
          role: 'user',
          content: 'Weather and time in Paris?'
        }
      ],
      tools: [
        {
          type: 'function',
          function: {
            name: 'weather',
            description: 'Weather in a city',
            parameters: {
              type: 'object',
              properties: { city: { type: 'string' } },
              required: ['city']
            }
          }
        },
        {
          type: 'function',
          function: {
            name: 'clock',
            description: 'Time in a zone',
            parameters: {
              type: 'object',
              properties: { zone: { type: 'string' } },
              required: ['zone']
            }
          }
        }
      ]
    }
  },
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
    key: 'voyage-packed-embeddings',
    label: 'Embeddings · Voyage packed binary',
    operation: 'embeddings',
    nativeDialect: 'voyage-embeddings',
    request: {
      model: '',
      input: ['First document', 'Second document'],
      output_dtype: 'ubinary',
      output_dimension: 16,
      encoding_format: 'base64'
    }
  },
  {
    key: 'tei-sparse-embeddings',
    label: 'Embeddings · TEI sparse',
    operation: 'embeddings',
    nativeDialect: 'tei-sparse-embeddings',
    request: { inputs: 'A document to embed' }
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
    key: 'tei-rerank',
    label: 'Rerank · TEI raw scores',
    operation: 'rerank',
    nativeDialect: 'tei-rerank',
    request: {
      query: 'media lifecycle',
      texts: [
        'A media job tracks generation state.',
        'A static asset is cached.'
      ],
      raw_scores: true
    }
  },
  {
    key: 'tei-classification',
    label: 'Classification · TEI predict',
    operation: 'classification',
    nativeDialect: 'tei-classification',
    request: { inputs: ['A neutral classification example.'], raw_scores: true }
  },
  {
    key: 'tei-scoring',
    label: 'Scoring · TEI similarity',
    operation: 'scoring',
    nativeDialect: 'tei-scoring',
    request: {
      inputs: {
        source_sentence: 'A media job',
        sentences: ['Async task', 'Static file']
      }
    }
  },
  {
    key: 'tei-tokenize',
    label: 'Token count · TEI tokenize',
    operation: 'token_count',
    nativeDialect: 'tei-tokenize',
    request: { inputs: 'Count native tokens.', add_special_tokens: true }
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
