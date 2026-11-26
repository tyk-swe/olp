import {
  nativeAt,
  nativeObject,
  stringifyNativeJSON,
  type NativeObject,
  type NativeValue
} from '#lib/nativeJson.js';

const maxRows = 128;

const dimensionPaths: readonly (readonly string[])[] = [
  ['dimensions'],
  ['output_dimension'],
  ['outputDimensionality'],
  ['parameters', 'outputDimensionality']
];

const typedEmbeddingKeys: ReadonlySet<string> = new Set([
  'float',
  'int8',
  'uint8',
  'binary',
  'ubinary',
  'base64'
]);

const countFieldNames = [
  'input_tokens',
  'inputTokens',
  'totalTokens',
  'total_tokens',
  'token_count',
  'tokenCount',
  'cachedContentTokenCount'
] as const;

type VectorShape = {
  layout: string;
  logicalShape: string;
  storageShape: string;
};

type VectorRow = VectorShape & {
  position: number;
  inputIndex: string;
  dtype: string;
};

type RerankRow = {
  position: number;
  inputIndex: string;
  inputId: string;
  score: string;
};

function atPath(
  value: unknown,
  path: readonly string[]
): NativeValue | undefined {
  return nativeObject(value) ? nativeAt(value, path) : undefined;
}

function exact(value: unknown): string {
  return value === undefined ? 'Unknown' : stringifyNativeJSON(value);
}

function dimension(request: unknown): string | null {
  for (const path of dimensionPaths) {
    const value = atPath(request, path);
    if (value !== undefined && value !== null) return exact(value);
  }
  return null;
}

function base64Bytes(value: string): number | null {
  if (
    value.length % 4 ||
    !/^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/.test(
      value
    )
  )
    return null;
  return (
    (value.length / 4) * 3 -
    (value.endsWith('==') ? 2 : value.endsWith('=') ? 1 : 0)
  );
}

function isPackedDtype(dtype: string): boolean {
  return dtype === 'binary' || dtype === 'ubinary';
}

function isSparseCoordinates(value: unknown): value is readonly unknown[] {
  return (
    Array.isArray(value) &&
    value.length > 0 &&
    value.every((entry) => nativeObject(entry) && Object.hasOwn(entry, 'index'))
  );
}

function isNestedArray(value: unknown): value is readonly unknown[][] {
  return (
    Array.isArray(value) &&
    value.length > 0 &&
    value.every((entry): entry is unknown[] => Array.isArray(entry))
  );
}

function formatDenseArrayShape(
  length: number,
  dtype: string,
  declared: string | null
): VectorShape {
  const packed = isPackedDtype(dtype);
  return {
    layout: packed ? 'Packed binary' : 'Dense array',
    logicalShape: declared ?? String(length * (packed ? 8 : 1)),
    storageShape: packed
      ? `${length} stored byte${length === 1 ? '' : 's'}`
      : `${length} stored values`
  };
}

function inferBase64LogicalShape(bytes: number | null, dtype: string): string {
  if (bytes === null) return 'Unknown';
  if (isPackedDtype(dtype)) return String(bytes * 8);
  if (dtype === 'int8' || dtype === 'uint8') return String(bytes);
  if (dtype === 'float' && bytes % 4 === 0) return String(bytes / 4);
  return 'Unknown';
}

function formatBase64Shape(
  encoded: string,
  dtype: string,
  declared: string | null
): VectorShape {
  const bytes = base64Bytes(encoded);
  return {
    layout: 'Base64 storage',
    logicalShape: declared ?? inferBase64LogicalShape(bytes, dtype),
    storageShape:
      bytes === null ? 'Opaque encoded value' : `${bytes} stored bytes`
  };
}

function formatMultivectorShape(
  vectors: readonly unknown[][],
  declared: string | null
): VectorShape {
  const lengths = vectors.map((entry) => entry.length);
  const total = lengths.reduce((sum, length) => sum + length, 0);
  const uniform = lengths.every((length) => length === lengths[0]);
  return {
    layout: 'Multivector',
    logicalShape:
      declared ?? (uniform ? `${lengths.length} × ${lengths[0]}` : 'Unknown'),
    storageShape: `${lengths.length} token vectors · ${total} stored values`
  };
}

function describeVectorStorage(
  observed: unknown,
  dtype: string,
  declared: string | null
): VectorShape {
  if (isSparseCoordinates(observed)) {
    return {
      layout: 'Sparse coordinates',
      logicalShape: declared ?? 'Unknown',
      storageShape: `${observed.length} index/value pairs`
    };
  }
  if (isNestedArray(observed)) {
    return formatMultivectorShape(observed, declared);
  }
  if (Array.isArray(observed)) {
    return formatDenseArrayShape(observed.length, dtype, declared);
  }
  if (typeof observed === 'string') {
    return formatBase64Shape(observed, dtype, declared);
  }
  return {
    layout: 'Native / unknown',
    logicalShape: declared ?? 'Unknown',
    storageShape: 'Unknown'
  };
}

function extractVectorItems(response: unknown): readonly unknown[] {
  const fromData = atPath(response, ['data']);
  if (Array.isArray(fromData)) return fromData;
  const fromPredictions = atPath(response, ['predictions']);
  if (Array.isArray(fromPredictions)) return fromPredictions;
  const fromEmbeddings = atPath(response, ['embeddings']);
  if (Array.isArray(fromEmbeddings)) return fromEmbeddings;
  return Array.isArray(response) ? response : [response];
}

function typedVectorRows(
  embeddings: NativeObject,
  declared: string | null
): VectorRow[] | null {
  const groups = Object.entries(embeddings).filter(([type]) =>
    typedEmbeddingKeys.has(type)
  );
  if (groups.length === 0) return null;
  return groups
    .flatMap(([dtype, group]) =>
      Array.isArray(group)
        ? group.slice(0, maxRows).map((storage, index) => ({
            inputIndex: String(index),
            dtype,
            ...describeVectorStorage(storage, dtype, declared)
          }))
        : []
    )
    .slice(0, maxRows)
    .map((row, position) => ({ position, ...row }));
}

function standardVectorRows(
  response: unknown,
  request: unknown,
  declared: string | null
): VectorRow[] {
  const items = extractVectorItems(response);
  const dtypeValue = atPath(request, ['output_dtype']);
  const dtype = typeof dtypeValue === 'string' ? dtypeValue : 'Not declared';
  return items.slice(0, maxRows).map((item, position) => {
    const observed =
      atPath(item, ['embedding']) ??
      atPath(item, ['embeddings', 'values']) ??
      atPath(item, ['values']) ??
      item;
    const inputIndex = atPath(item, ['index']);
    return {
      position,
      inputIndex: inputIndex === undefined ? 'Not reported' : exact(inputIndex),
      dtype,
      ...describeVectorStorage(observed, dtype, declared)
    };
  });
}

export function vectorRows(response: unknown, request?: unknown): VectorRow[] {
  const declared = dimension(request);
  const fromEmbeddings = atPath(response, ['embeddings']);
  if (nativeObject(fromEmbeddings)) {
    const typed = typedVectorRows(fromEmbeddings, declared);
    if (typed !== null) return typed;
  }
  return standardVectorRows(response, request, declared);
}

export function rerankRows(response: unknown, request?: unknown): RerankRow[] {
  const results = atPath(response, ['results']);
  const data = atPath(response, ['data']);
  const items = Array.isArray(results)
    ? results
    : Array.isArray(data)
      ? data
      : Array.isArray(response)
        ? response
        : [];
  const documents = atPath(request, ['documents']);
  return items.slice(0, maxRows).map((item, position) => {
    const index = atPath(item, ['index']);
    const parsed = index === undefined ? NaN : Number(exact(index));
    const document =
      Array.isArray(documents) && Number.isSafeInteger(parsed)
        ? documents[parsed]
        : undefined;
    const originalId = atPath(document, ['id']);
    const returnedId = atPath(item, ['id']);
    return {
      position,
      inputIndex: index === undefined ? 'Unknown' : exact(index),
      inputId:
        originalId !== undefined
          ? exact(originalId)
          : returnedId !== undefined
            ? exact(returnedId)
            : 'Not reported',
      score: exact(atPath(item, ['relevance_score']) ?? atPath(item, ['score']))
    };
  });
}

export function nativeCountFields(
  response: unknown
): { name: string; value: string }[] {
  if (!nativeObject(response)) return [];
  return countFieldNames.flatMap((name) => {
    const value = nativeAt(response, [name]);
    return value === undefined ? [] : [{ name, value: exact(value) }];
  });
}
