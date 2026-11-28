export type FieldErrors<Field extends string> = Partial<
  Readonly<Record<Field, string>>
>;

export type Valid<Value> = { ok: true; value: Value };

export type Validation<Value, Field extends string> =
  Valid<Value> | { ok: false; fieldErrors: FieldErrors<Field> };

export function validated<Value, Field extends string>(
  fieldErrors: FieldErrors<Field>,
  value: () => Value
): Validation<Value, Field> {
  return Object.values(fieldErrors).some(Boolean)
    ? { ok: false, fieldErrors }
    : { ok: true, value: value() };
}
