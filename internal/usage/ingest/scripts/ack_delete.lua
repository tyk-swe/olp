-- Acknowledges one delivery and removes its entry in a single server-side step.
-- The two must not drift apart: an acknowledged entry left in the stream would
-- inflate the group's lag forever, and a deleted-but-unacknowledged entry would
-- be reported as lost. PostgreSQL has already committed when this runs.
local acknowledged = redis.call("XACK", KEYS[1], ARGV[1], ARGV[2])
local deleted = redis.call("XDEL", KEYS[1], ARGV[2])
return { acknowledged, deleted }
