export type Result<T> = { ok: true; value: T } | { ok: false; error: unknown };

/** A read reports its problem as a value so the caller decides how it renders. */
export function captureResult<T>(read: () => T): Result<T> {
  try {
    return { ok: true, value: read() };
  } catch (error) {
    return { ok: false, error };
  }
}

export async function readResult<T>(
  read: () => Promise<T>
): Promise<Result<T>> {
  try {
    return { ok: true, value: await read() };
  } catch (error) {
    return { ok: false, error };
  }
}
