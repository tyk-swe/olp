-- Undo a provider admission that did not dispatch. A lease can be refunded
-- once, only in its original minute; it cannot later reconcile token usage.
-- KEYS: stable rate hash, concurrency zset (same cluster hash tag).
-- ARGV: reservation window, reserved requests, reserved tokens, lease ID.
local MAX_SAFE_INTEGER = 9007199254740991
local function unsigned(raw)
  if type(raw) ~= 'string' or string.match(raw, '^%d+$') == nil then return nil end
  local value = tonumber(raw)
  if value == nil or value > MAX_SAFE_INTEGER then return nil end
  return value
end

local requests = unsigned(ARGV[2])
local tokens = unsigned(ARGV[3])
local lease = ARGV[4]
if requests == nil or requests > 1 or tokens == nil
    or type(lease) ~= 'string' or #lease < 1 or #lease > 128 then
  return redis.error_reply('invalid refund arguments')
end
local state = redis.call('HMGET', KEYS[1], 'window', 'rpm', 'tpm')
local refunded = 'refunded:' .. lease
local reconciled = 'reconciled:' .. lease
if state[1] == ARGV[1] and redis.call('HEXISTS', KEYS[1], refunded) == 0 then
  if redis.call('HEXISTS', KEYS[1], reconciled) == 1 then
    return redis.error_reply('cannot refund a reconciled lease')
  end
  local rpm = unsigned(state[2])
  local tpm = unsigned(state[3])
  if rpm == nil or tpm == nil then return redis.error_reply('invalid rate state') end
  redis.call('HSET', KEYS[1], 'rpm', math.max(0, rpm - requests),
             'tpm', math.max(0, tpm - tokens), refunded, 1, reconciled, 1)
end
redis.call('ZREM', KEYS[2], lease)
return 1
