-- Undo a provider admission that did not dispatch. A lease can be refunded
-- once, only in its original minute; it cannot later reconcile token usage.
-- KEYS: stable rate hash, concurrency zset (same cluster hash tag).
-- ARGV: reservation window, reserved requests, reserved combined, input and
--       output tokens, lease ID, and the share class the lease was counted
--       in, if any.
local MAX_SAFE_INTEGER = 9007199254740991
local function unsigned(raw)
  if type(raw) ~= 'string' or string.match(raw, '^%d+$') == nil then return nil end
  local value = tonumber(raw)
  if value == nil or value > MAX_SAFE_INTEGER then return nil end
  return value
end

local function counter(raw)
  if raw == false then return 0 end
  return unsigned(raw)
end

local requests = unsigned(ARGV[2])
local tokens = unsigned(ARGV[3])
local input = unsigned(ARGV[4])
local output = unsigned(ARGV[5])
local lease = ARGV[6]
local class = ARGV[7]
if requests == nil or requests > 1 or tokens == nil or input == nil or output == nil
    or type(lease) ~= 'string' or #lease < 1 or #lease > 128
    or (class ~= nil and string.match(class, '^[a-z]+$') == nil) then
  return redis.error_reply('invalid refund arguments')
end
local state = redis.call('HMGET', KEYS[1], 'window', 'rpm', 'tpm', 'itpm', 'otpm')
local refunded = 'refunded:' .. lease
local reconciled = 'reconciled:' .. lease
if state[1] == ARGV[1] and redis.call('HEXISTS', KEYS[1], refunded) == 0 then
  if redis.call('HEXISTS', KEYS[1], reconciled) == 1 then
    return redis.error_reply('cannot refund a reconciled lease')
  end
  local rpm = unsigned(state[2])
  local tpm = unsigned(state[3])
  local itpm = counter(state[4])
  local otpm = counter(state[5])
  if rpm == nil or tpm == nil or itpm == nil or otpm == nil then return redis.error_reply('invalid rate state') end
  redis.call('HSET', KEYS[1], 'rpm', math.max(0, rpm - requests),
             'tpm', math.max(0, tpm - tokens),
             'itpm', math.max(0, itpm - input),
             'otpm', math.max(0, otpm - output), refunded, 1, reconciled, 1)
  if class ~= nil then
    local held = redis.call('HMGET', KEYS[1], 'rpm:' .. class, 'tpm:' .. class, 'itpm:' .. class, 'otpm:' .. class)
    redis.call('HSET', KEYS[1],
               'rpm:' .. class, math.max(0, (unsigned(held[1]) or 0) - requests),
               'tpm:' .. class, math.max(0, (unsigned(held[2]) or 0) - tokens),
               'itpm:' .. class, math.max(0, (unsigned(held[3]) or 0) - input),
               'otpm:' .. class, math.max(0, (unsigned(held[4]) or 0) - output))
  end
end
redis.call('ZREM', KEYS[2], lease)
return 1
