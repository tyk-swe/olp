UPDATE olp.code_accounts accounts
SET allowance = jsonb_set(accounts.allowance, '{windows}', (
    SELECT coalesce(jsonb_agg(
        entries.entry || jsonb_build_object(
            'unit', coalesce(nullif(entries.entry->>'unit', ''), 'percent'),
            'evidence', coalesce(nullif(entries.entry->>'evidence', ''), 'passive'),
            'used_value', CASE WHEN entries.entry ? 'used_value' THEN entries.entry->'used_value' ELSE 'null'::jsonb END,
            'remaining_value', CASE WHEN entries.entry ? 'remaining_value' THEN entries.entry->'remaining_value' ELSE 'null'::jsonb END
        ) ORDER BY entries.ordinality), '[]'::jsonb)
    FROM jsonb_array_elements(accounts.allowance->'windows') WITH ORDINALITY AS entries(entry, ordinality)
))
WHERE accounts.allowance IS NOT NULL
  AND jsonb_typeof(accounts.allowance->'windows') = 'array';
