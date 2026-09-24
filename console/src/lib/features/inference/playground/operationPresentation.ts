import { nativeObject, stringifyNativeJSON } from '$lib/json/nativeJson';

function field(value: unknown, key: string): unknown {
  return nativeObject(value) && Object.hasOwn(value, key)
    ? value[key]
    : undefined;
}

function member(value: unknown, ...path: string[]): unknown {
  return path.reduce<unknown>((at, key) => field(at, key), value);
}

function exact(value: unknown): string {
  return value === undefined ? 'Unknown' : stringifyNativeJSON(value);
}

function dimension(request: unknown): string | null {
  for (const path of [
    ['dimensions'],
    ['output_dimension'],
    ['outputDimensionality'],
    ['parameters', 'outputDimensionality']
  ]) {
    const value = member(request, ...path);
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

export type VectorRow = {
  position: number;
  inputIndex: string;
  layout: string;
  dtype: string;
  logicalShape: string;
  storageShape: string;
};

/** Shape is descriptive only. Dtype/dimensions are reported as known only when
 * the native request or observed storage establishes them. Raw result bytes
 * remain available separately and are never converted to float vectors. */
export function vectorRows(response: unknown, request?: unknown): VectorRow[] {
  const fromData = field(response, 'data');
  const fromPredictions = field(response, 'predictions');
  const fromEmbeddings = field(response, 'embeddings');
  if (nativeObject(fromEmbeddings)) {
    const typed = Object.entries(fromEmbeddings).filter(([type]) =>
      ['float', 'int8', 'uint8', 'binary', 'ubinary', 'base64'].includes(type)
    );
    if (typed.length) {
      const rows: VectorRow[] = [];
      const declared = dimension(request);
      for (const [dtype, group] of typed) {
        if (!Array.isArray(group)) continue;
        for (const [index, storage] of group.entries()) {
          if (rows.length === 128) return rows;
          const packed = dtype === 'binary' || dtype === 'ubinary';
          const bytes =
            typeof storage === 'string' ? base64Bytes(storage) : null;
          const stored = Array.isArray(storage) ? storage.length : null;
          const logical =
            declared ??
            (stored !== null ? String(stored * (packed ? 8 : 1)) : 'Unknown');
          rows.push({
            position: rows.length,
            inputIndex: String(index),
            layout: packed
              ? 'Packed binary'
              : dtype === 'base64'
                ? 'Base64 storage'
                : 'Dense array',
            dtype,
            logicalShape: logical,
            storageShape:
              stored !== null
                ? packed
                  ? `${stored} stored byte${stored === 1 ? '' : 's'}`
                  : `${stored} stored values`
                : bytes !== null
                  ? `${bytes} stored bytes`
                  : 'Opaque encoded value'
          });
        }
      }
      return rows;
    }
  }
  const items = Array.isArray(fromData)
    ? fromData
    : Array.isArray(fromPredictions)
      ? fromPredictions
      : Array.isArray(fromEmbeddings)
        ? fromEmbeddings
        : Array.isArray(response)
          ? response
          : [response];
  const dtypeValue = field(request, 'output_dtype');
  const dtype = typeof dtypeValue === 'string' ? dtypeValue : 'Not declared';
  const declaredDimensions = dimension(request);
  return items.slice(0, 128).map((item, position) => {
    const observed =
      field(item, 'embedding') ??
      member(item, 'embeddings', 'values') ??
      field(item, 'values') ??
      item;
    const inputIndex = field(item, 'index');
    const sparse =
      Array.isArray(observed) &&
      observed.every(
        (entry) => nativeObject(entry) && Object.hasOwn(entry, 'index')
      );
    const multi =
      Array.isArray(observed) &&
      observed.length > 0 &&
      observed.every((entry) => Array.isArray(entry));
    let layout = 'Native / unknown';
    let logicalShape = declaredDimensions ?? 'Unknown';
    let storageShape = 'Unknown';
    if (sparse) {
      layout = 'Sparse coordinates';
      storageShape = `${observed.length} index/value pairs`;
    } else if (multi) {
      layout = 'Multivector';
      const lengths = (observed as unknown[][]).map((entry) => entry.length);
      storageShape = `${lengths.length} token vectors · ${lengths.reduce((sum, length) => sum + length, 0)} stored values`;
      if (
        !declaredDimensions &&
        lengths.every((length) => length === lengths[0])
      )
        logicalShape = `${lengths.length} × ${lengths[0]}`;
    } else if (Array.isArray(observed)) {
      const packed = dtype === 'binary' || dtype === 'ubinary';
      layout = packed ? 'Packed binary' : 'Dense array';
      storageShape = `${observed.length} ${packed ? 'stored bytes' : 'stored values'}`;
      if (!declaredDimensions)
        logicalShape = String(observed.length * (packed ? 8 : 1));
    } else if (typeof observed === 'string') {
      layout = 'Base64 storage';
      const bytes = base64Bytes(observed);
      storageShape =
        bytes === null ? 'Opaque encoded value' : `${bytes} stored bytes`;
      if (!declaredDimensions && bytes !== null) {
        if (dtype === 'binary' || dtype === 'ubinary')
          logicalShape = String(bytes * 8);
        else if (dtype === 'int8' || dtype === 'uint8')
          logicalShape = String(bytes);
        else if (dtype === 'float' && bytes % 4 === 0)
          logicalShape = String(bytes / 4);
      }
    }
    return {
      position,
      inputIndex: inputIndex === undefined ? 'Not reported' : exact(inputIndex),
      layout,
      dtype,
      logicalShape,
      storageShape
    };
  });
}

export type RerankRow = {
  position: number;
  inputIndex: string;
  inputId: string;
  score: string;
};

/** Preserve result order and native numeric score spelling, including ties. */
export function rerankRows(response: unknown, request?: unknown): RerankRow[] {
  const results = field(response, 'results');
  const data = field(response, 'data');
  const items = Array.isArray(results)
    ? results
    : Array.isArray(data)
      ? data
      : Array.isArray(response)
        ? response
        : [];
  const documents = field(request, 'documents');
  return items.slice(0, 128).map((item, position) => {
    const index = field(item, 'index');
    const parsed = index === undefined ? NaN : Number(exact(index));
    const document =
      Array.isArray(documents) && Number.isSafeInteger(parsed)
        ? documents[parsed]
        : undefined;
    const originalId = field(document, 'id');
    const returnedId = field(item, 'id');
    return {
      position,
      inputIndex: index === undefined ? 'Unknown' : exact(index),
      inputId:
        originalId !== undefined
          ? exact(originalId)
          : returnedId !== undefined
            ? exact(returnedId)
            : 'Not reported',
      score: exact(field(item, 'relevance_score') ?? field(item, 'score'))
    };
  });
}

export function nativeCountFields(
  response: unknown
): { name: string; value: string }[] {
  if (!nativeObject(response)) return [];
  const names = [
    'input_tokens',
    'inputTokens',
    'totalTokens',
    'total_tokens',
    'token_count',
    'tokenCount',
    'cachedContentTokenCount'
  ];
  return names.flatMap((name) => {
    const value = field(response, name);
    return value === undefined ? [] : [{ name, value: exact(value) }];
  });
}
