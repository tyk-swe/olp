export const operationOptions = [
  ['generation', 'Text generation'],
  ['decision', 'Decisions'],
  ['classification', 'Classification'],
  ['scoring', 'Scoring'],
  ['embeddings', 'Embeddings'],
  ['rerank', 'Rerank'],
  ['token_count', 'Token counting'],
  ['image_generation', 'Image generation'],
  ['image_edit', 'Image editing'],
  ['image_variation', 'Image variations'],
  ['speech', 'Speech'],
  ['transcription', 'Transcription'],
  ['translation', 'Audio translation'],
  ['video_create', 'Create video'],
  ['video_list', 'List videos'],
  ['video_get', 'Video status'],
  ['video_content', 'Video content'],
  ['video_delete', 'Delete video'],
  ['ocr', 'OCR'],
  ['search', 'Web search'],
  ['moderation', 'Moderation'],
  ['batch', 'Batches'],
  ['fine_tuning', 'Fine-tuning'],
  ['vector_store', 'Vector stores'],
  ['conversation', 'Conversations'],
  ['container', 'Containers'],
  ['eval', 'Evaluations'],
  ['skill', 'Skills'],
  ['context_cache', 'Context caches'],
  ['retrieval', 'Retrieval'],
  ['agent_resource', 'Managed agents'],
  ['realtime', 'Realtime sessions'],
  ['bedrock_invoke', 'Bedrock InvokeModel'],
  ['passthrough', 'Governed pass-through (uncertified)']
] as const;

export function surfacesFor(operation: string): string[] {
  if (operation === 'passthrough') return ['native'];
  if (operation === 'agent_resource') return ['gemini'];
  if (operation === 'bedrock_invoke') return ['bedrock'];
  if (operation === 'decision') return ['openai', 'gemini'];
  if (operation === 'generation')
    return ['openai', 'anthropic', 'gemini', 'bedrock'];
  if (operation === 'token_count') return ['openai', 'anthropic', 'gemini'];
  if (operation === 'realtime') return ['openai', 'native', 'gemini'];
  return ['openai'];
}

export function modesFor(operation: string): string[] {
  if (operation === 'video_create') return ['async'];
  if (operation === 'realtime') return ['realtime', 'webrtc'];
  if (
    [
      'generation',
      'image_generation',
      'image_edit',
      'speech',
      'transcription',
      'bedrock_invoke'
    ].includes(operation)
  ) {
    return ['unary', 'streaming'];
  }
  return ['unary'];
}
