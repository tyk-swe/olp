ALTER TABLE olp.export_pending ADD COLUMN delivery_settings jsonb
 CHECK (delivery_settings IS NULL OR jsonb_typeof(delivery_settings)='object');
