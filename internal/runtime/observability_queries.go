package runtime

const recordInstallStatusSQL = `INSERT INTO olp.runtime_install_status(gateway_instance,desired_generation,installed_generation,failed)
 VALUES($1,$2,$3,$4) ON CONFLICT(gateway_instance) DO UPDATE SET desired_generation=EXCLUDED.desired_generation,
 installed_generation=EXCLUDED.installed_generation,failed=EXCLUDED.failed,checked_at=now()`
