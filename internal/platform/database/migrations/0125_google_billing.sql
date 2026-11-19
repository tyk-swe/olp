ALTER TABLE olp.provider_billing_imports
    DROP CONSTRAINT provider_billing_imports_source_check,
    ADD CONSTRAINT provider_billing_imports_source_check
        CHECK (source IN ('openai_costs', 'anthropic_cost_report', 'aws_cur', 'google_bigquery'));
