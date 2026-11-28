import { describe, expect, it } from 'vitest';

import { modesFor, surfacesFor } from './routeOperations';

describe('modesFor', () => {
  it('offers webrtc as its own realtime simulation mode', () => {
    expect(modesFor('realtime')).toEqual(['realtime', 'webrtc']);
  });

  it('keeps non-realtime operations on their existing modes', () => {
    expect(modesFor('generation')).toEqual(['unary', 'streaming']);
    expect(modesFor('embeddings')).toEqual(['unary']);
  });
});

describe('surfacesFor', () => {
  it('offers the native surface for duplex realtime simulation', () => {
    expect(surfacesFor('realtime')).toEqual(['openai', 'native', 'gemini']);
  });

  it('offers the standard OpenAI surface for embeddings', () => {
    expect(surfacesFor('embeddings')).toEqual(['openai']);
  });
});
