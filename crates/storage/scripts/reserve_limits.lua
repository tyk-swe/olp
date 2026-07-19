-- Atomic fixed-window RPM/TPM and expiring concurrency reservation.
-- KEYS: rpm counter, tpm counter, concurrency zset
-- ARGV: now_ms, window_ttl_ms, rpm_limit, tpm_limit, requested_tokens,
--       concurrency_limit, lease_id, lease_ttl_ms

local now = tonumber(ARGV[1])
local window = tonumber(ARGV[2])
local rpm_limit = tonumber(ARGV[3])
local tpm_limit = tonumber(ARGV[4])
local requested_tokens = tonumber(ARGV[5])
local concurrency_limit = tonumber(ARGV[6])
local lease_id = ARGV[7]
local lease_ttl = tonumber(ARGV[8])

redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now)

local rpm = tonumber(redis.call('GET', KEYS[1]) or '0')
local tpm = tonumber(redis.call('GET', KEYS[2]) or '0')
local concurrency = tonumber(redis.call('ZCARD', KEYS[3]))

if rpm_limit > 0 and rpm + 1 > rpm_limit then
  local retry = redis.call('PTTL', KEYS[1])
  if retry < 1 then retry = window end
  return {0, 'rpm', retry}
end
-- Subtraction avoids a comparison on a potentially inexact sum near Lua's
-- largest exactly representable integer.
if tpm_limit > 0 and (requested_tokens > tpm_limit or tpm > tpm_limit - requested_tokens) then
  local retry = redis.call('PTTL', KEYS[2])
  if retry < 1 then retry = window end
  return {0, 'tpm', retry}
end
if concurrency_limit > 0 and concurrency + 1 > concurrency_limit then
  local oldest = redis.call('ZRANGE', KEYS[3], 0, 0, 'WITHSCORES')
  local retry = lease_ttl
  if #oldest == 2 then retry = math.max(1, tonumber(oldest[2]) - now) end
  return {0, 'concurrency', retry}
end

if rpm_limit > 0 then
  redis.call('INCR', KEYS[1])
  -- Do not extend a fixed window on every request. Repair a missing expiry,
  -- but otherwise retain the deadline established by the first reservation.
  if redis.call('PTTL', KEYS[1]) < 0 then
    redis.call('PEXPIRE', KEYS[1], window)
  end
end
if tpm_limit > 0 then
  redis.call('INCRBY', KEYS[2], requested_tokens)
  if redis.call('PTTL', KEYS[2]) < 0 then
    redis.call('PEXPIRE', KEYS[2], window)
  end
end
if concurrency_limit > 0 then
  redis.call('ZADD', KEYS[3], now + lease_ttl, lease_id)
  redis.call('PEXPIRE', KEYS[3], lease_ttl + window)
end

return {1, 'ok', 0}
