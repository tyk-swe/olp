export type EntityOption = { value: string; label: string; detail?: string };

export type EntitySource = () => {
  readonly data: EntityOption[] | undefined;
  readonly isError: boolean;
  readonly isEnabled: boolean;
  readonly isPending: boolean;
};

export const named = (entity: { id: string; name: string }): EntityOption => ({
  value: entity.id,
  label: entity.name
});
