package release

const recordInstallStatusSQL = `INSERT INTO olp.runtime_install_status(gateway_instance,desired_generation,installed_generation,failed,authority_sequence)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT(gateway_instance) DO UPDATE SET desired_generation=EXCLUDED.desired_generation,
 installed_generation=EXCLUDED.installed_generation,failed=EXCLUDED.failed,authority_sequence=EXCLUDED.authority_sequence,checked_at=now()`

const expireInstallStatusSQL = `DELETE FROM olp.runtime_install_status WHERE checked_at<now()-interval '1 day'`
