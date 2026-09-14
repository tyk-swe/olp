-- Report the live quota usage of one lookup without reserving anything.
-- KEYS: stable rate hash, concurrency zset (same cluster hash tag).
-- Counters from an older fixed UTC minute read as zero, and only leases that
-- have not yet expired against the server clock count as concurrent.
local t = redis.call('TIME')
local w = math.floor(tonumber(t[1]) / 60)
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local r = redis.call('HMGET', KEYS[1], 'window', 'rpm', 'tpm')
local rpm = 0
local tpm = 0
if tonumber(r[1]) == w then
  rpm = tonumber(r[2]) or 0
  tpm = tonumber(r[3]) or 0
end
return {rpm, tpm, redis.call('ZCOUNT', KEYS[2], '(' .. now, '+inf')}
