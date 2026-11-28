import { parseNativeJSON } from './nativeJsonParser.js';
import {
  IncompleteJSON,
  NativeNumber,
  defineDataProperty,
  nativeEntries,
  nativeObject,
  rememberOrder,
  type NativeObject,
  type NativePath,
  type NativeValue
} from './nativeJsonValue.js';

export { parseNativeJSON } from './nativeJsonParser.js';
export {
  IncompleteJSON,
  NativeNumber,
  nativeEntries,
  nativeObject,
  type NativeObject,
  type NativePath,
  type NativeValue
} from './nativeJsonValue.js';

function cloneNativeTree(
  value: unknown,
  options: { wrapNumbers: boolean; omitUndefined: boolean }
): unknown {
  const ancestors = new Set<object>();
  const copy = (node: unknown): unknown => {
    if (node === null || typeof node === 'string' || typeof node === 'boolean')
      return node;
    if (typeof node === 'number') {
      if (!Number.isFinite(node))
        throw new Error('Non-finite values are not JSON numbers.');
      return options.wrapNumbers
        ? new NativeNumber(Object.is(node, -0) ? '-0' : String(node))
        : node;
    }
    if (node === undefined) return undefined;
    if (typeof node !== 'object')
      throw new Error('A JSON submission must contain JSON values.');
    if (node instanceof NativeNumber || node instanceof IncompleteJSON)
      return node;
    if (ancestors.has(node))
      throw new Error('JSON cannot contain a cyclic reference.');
    ancestors.add(node);
    try {
      if (Array.isArray(node)) {
        return Object.freeze(
          Array.from(node, (item) =>
            options.omitUndefined && item === undefined ? null : copy(item)
          )
        );
      }
      const prototype = Object.getPrototypeOf(node);
      if (
        prototype !== Object.prototype &&
        prototype !== null &&
        !(node instanceof NativeConfiguration)
      ) {
        throw new Error('A JSON submission must contain JSON values.');
      }
      const entries = nativeEntries(node).filter(
        ([, entry]) => !options.omitUndefined || entry !== undefined
      );
      const result: Record<string, unknown> = {};
      for (const [key, entry] of entries) {
        defineDataProperty(result, key, copy(entry));
      }
      rememberOrder(
        result,
        entries.map(([key]) => key)
      );
      return Object.freeze(result);
    } finally {
      ancestors.delete(node);
    }
  };
  return copy(value);
}

/** Own a JSON submission without coercing native number spellings or losing
 * remembered object-member order. Immutable number wrappers are safe to share. */
export function snapshotNativeJSON<T>(value: T): T {
  return cloneNativeTree(value, {
    wrapNumbers: false,
    omitUndefined: false
  }) as T;
}

/** Clone a plain or native value into a frozen native JSON tree, preserving
 * member order and wrapping finite numbers as exact NativeNumber tokens. */
export function normalizeNativeJSON(value: unknown): NativeValue {
  if (value === undefined) throw new Error('A JSON value is required.');
  return cloneNativeTree(value, {
    wrapNumbers: true,
    omitUndefined: true
  }) as NativeValue;
}

export function stringifyNativeJSON(value: unknown, indent = 0): string {
  return renderJSON(value, indent, false);
}

export function nativeAt(
  value: NativeValue,
  path: NativePath
): NativeValue | undefined {
  let current: NativeValue | undefined = value;
  for (const key of path) {
    if (!nativeObject(current) || !Object.hasOwn(current, key))
      return undefined;
    current = current[key];
  }
  return current;
}

/** Member replacement is atomic; arrays/schemas never recursively merge.
 * Undefined means explicit removal, while null is an ordinary native value. */
export function replaceNative(
  value: NativeValue,
  path: NativePath,
  next: NativeValue | undefined
): NativeValue {
  if (next === undefined && nativeAt(value, path) === undefined) return value;
  if (!path.length) {
    if (next === undefined)
      throw new Error('The root document cannot be removed.');
    return next;
  }
  const [key, ...rest] = path;
  const object: NativeObject = Object.create(null);
  if (nativeObject(value)) {
    for (const [name, entry] of nativeEntries(value))
      defineDataProperty(object, name, entry);
  } else if (value !== undefined && value !== null) {
    throw new Error('An object is required at this configuration path.');
  }
  if (rest.length) {
    const child = Object.hasOwn(object, key!) ? object[key!]! : null;
    defineDataProperty(object, key!, replaceNative(child, rest, next));
  } else if (next === undefined) delete object[key!];
  else defineDataProperty(object, key!, next);
  rememberOrder(object, [
    ...(nativeObject(value)
      ? nativeEntries(value)
          .map(([name]) => name)
          .filter((name) => Object.hasOwn(object, name))
      : []),
    ...Object.keys(object).filter(
      (name) => !nativeObject(value) || !Object.hasOwn(value, name)
    )
  ]);
  return object;
}

// The query cache's generic object copier assigns keys such as __proto__.
// Keep each complete native configuration opaque to structural sharing while
// retaining ordinary, read-only generated-contract properties for consumers.
class NativeConfiguration {
  [key: string]: NativeValue;
  constructor(value: NativeObject) {
    for (const [key, entry] of nativeEntries(value)) {
      Object.defineProperty(this, key, {
        value: freezeNative(entry),
        enumerable: true
      });
    }
    rememberOrder(
      this,
      nativeEntries(value).map(([key]) => key)
    );
    Object.freeze(this);
  }
}

export function parseManagementJSON(
  source: string,
  nativePaths: readonly NativePath[] = [],
  maxBytes?: number
): unknown {
  const normalize = (
    value: NativeValue,
    path: readonly (string | number)[]
  ): unknown => {
    if (
      nativePaths.some(
        (nativePath) =>
          nativePath.length === path.length &&
          nativePath.every(
            (part, index) =>
              part === path[index] ||
              (part === '*' && typeof path[index] === 'number')
          )
      )
    )
      return nativeObject(value)
        ? new NativeConfiguration(value)
        : freezeNative(value);
    if (value instanceof NativeNumber) return Number(value.source);
    if (Array.isArray(value))
      return value.map((item, index) => normalize(item, [...path, index]));
    if (!nativeObject(value)) return value;
    const entries = nativeEntries(value).map(
      ([key, child]) => [key, normalize(child, [...path, key])] as const
    );
    const result = Object.fromEntries(entries);
    rememberOrder(
      result,
      entries.map(([key]) => key)
    );
    return result;
  };
  return normalize(parseNativeJSON(source, maxBytes), []);
}

export function nativeIssue(value: NativeValue): string | null {
  if (value instanceof IncompleteJSON) return value.issue;
  if (Array.isArray(value)) {
    for (const item of value) {
      const issue = nativeIssue(item);
      if (issue) return issue;
    }
  } else if (nativeObject(value)) {
    for (const item of Object.values(value)) {
      const issue = nativeIssue(item);
      if (issue) return issue;
    }
  }
  return null;
}

export function freezeNative(value: NativeValue): NativeValue {
  if (Array.isArray(value)) value.forEach(freezeNative);
  else if (nativeObject(value)) Object.values(value).forEach(freezeNative);
  if (value !== null && typeof value === 'object') Object.freeze(value);
  return value;
}

/** The editor may display an unfinished fragment. API serialization still
 * uses stringifyNativeJSON, whose incomplete sentinel always throws. */
export function renderNativeDraft(value: NativeValue): string {
  return renderJSON(value, 2, true);
}

function renderJSON(
  value: unknown,
  indentation: number,
  incomplete: boolean
): string {
  const ancestors = new Set<object>();
  const render = (node: unknown, depth: number): string => {
    if (node instanceof IncompleteJSON) {
      if (!incomplete) throw new Error(node.issue);
      return node.source;
    }
    if (node instanceof NativeNumber) return node.source;
    if (node === null || typeof node === 'string' || typeof node === 'boolean')
      return JSON.stringify(node);
    if (typeof node === 'number') {
      if (!Number.isFinite(node))
        throw new Error('Non-finite values are not JSON numbers.');
      return Object.is(node, -0) ? '-0' : String(node);
    }
    if (typeof node !== 'object') throw new Error('A JSON value is required.');
    if (ancestors.has(node))
      throw new Error('JSON cannot contain a cyclic reference.');
    ancestors.add(node);
    const padding = ' '.repeat(depth * indentation);
    const next = ' '.repeat((depth + 1) * indentation);
    const join = (open: string, close: string, entries: string[]) =>
      !entries.length
        ? open + close
        : indentation
          ? `${open}\n${entries.map((entry) => next + entry).join(',\n')}\n${padding}${close}`
          : open + entries.join(',') + close;
    let result: string;
    if (Array.isArray(node))
      result = join(
        '[',
        ']',
        Array.from(node, (item) =>
          item === undefined ? 'null' : render(item, depth + 1)
        )
      );
    else
      result = join(
        '{',
        '}',
        nativeEntries(node)
          .filter(([, entry]) => entry !== undefined)
          .map(
            ([key, entry]) =>
              `${JSON.stringify(key)}:${indentation ? ' ' : ''}${render(entry, depth + 1)}`
          )
      );
    ancestors.delete(node);
    return result;
  };
  return render(value, 0);
}
