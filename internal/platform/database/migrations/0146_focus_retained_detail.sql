ALTER TABLE olp.attempt_usage_hourly
 ADD COLUMN focus_cost_detail_missing boolean NOT NULL DEFAULT true,
 ADD COLUMN focus_charge_detail_missing boolean NOT NULL DEFAULT true;
