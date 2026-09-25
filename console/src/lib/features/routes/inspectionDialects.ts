import type { components } from '$lib/api/schema';

type Dialect = components['schemas']['SimulationDialect'];
type OperationDialect = components['schemas']['OperationDialect'];

/** The simulation request contract validates dialect against this closed
 * enum; a registry row the contract does not name cannot be sent, so catalog
 * rows outside it are never offered for inspection. */
const admissible = new Set<string>([
  'openai-chat',
  'openai-responses',
  'anthropic-messages',
  'gemini-generate-content',
  'openai-input-tokens',
  'anthropic-count-tokens',
  'gemini-count-tokens',
  'openai-embeddings',
  'openai-moderation',
  'rerank',
  'voyage-embeddings',
  'tei-embeddings',
  'tei-sparse-embeddings',
  'tei-multivector-embeddings',
  'gemini-embeddings',
  'gemini-batch-embeddings',
  'vertex-embeddings',
  'bedrock-embeddings',
  'voyage-rerank',
  'tei-rerank',
  'tei-classification',
  'tei-scoring',
  'bedrock-count-tokens',
  'tei-tokenize'
]);

/** The native request dialects the management inspector admits for this
 * operation, surface and transport mode, taken from the registered dialect
 * catalog rather than a console-maintained list. */
export function inspectionDialects(
  catalog: readonly OperationDialect[],
  operation: string,
  surface: string,
  mode?: string
): Dialect[] {
  const seen = new Set<string>();
  const out: Dialect[] = [];
  for (const dialect of catalog) {
    if (dialect.operation !== operation || dialect.surface !== surface)
      continue;
    if (mode !== undefined && dialect.mode !== mode) continue;
    if (seen.has(dialect.id) || !admissible.has(dialect.id)) continue;
    seen.add(dialect.id);
    out.push(dialect.id as Dialect);
  }
  return out;
}
