DELETE FROM olp.provider_billing_days bd
WHERE bd.source IN ('openai_costs', 'anthropic_cost_report')
  AND (bd.last_success_at IS NULL OR bd.last_success_at <= (
      SELECT applied_at FROM olp.migrations WHERE version = '0127_billing_import_history.sql'))
  AND NOT EXISTS (
      SELECT 1 FROM olp.provider_invoiced_costs ic
      WHERE ic.provider_id = bd.provider_id AND ic.day = bd.day);

UPDATE olp.provider_billing_imports bi
SET (imported_from, imported_through) = (
    SELECT lower(span), upper(span) - 1
    FROM (
        SELECT unnest(range_agg(daterange(day, day, '[]'))) AS span
        FROM olp.provider_billing_days bd
        WHERE bd.provider_id = bi.provider_id AND bd.source = bi.source AND bd.scope = bi.scope
    ) spans
    ORDER BY upper(span) DESC
    LIMIT 1
)
WHERE bi.source IN ('openai_costs', 'anthropic_cost_report');
