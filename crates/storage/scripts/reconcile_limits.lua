-- Refund unused tokens from a reservation that still belongs to the active
-- fixed UTC-minute window.
-- KEYS: stable rate hash
-- ARGV: reservation window_id, refund_tokens

local stored_window = redis.call("HGET", KEYS[1], "window")
if stored_window == false or stored_window ~= ARGV[1] then
  return 0
end

local refund = tonumber(ARGV[2])
if refund == nil or refund <= 0 then
  return 0
end

local updated = redis.call("HINCRBY", KEYS[1], "tpm", -refund)
if tonumber(updated) < 0 then
  redis.call("HSET", KEYS[1], "tpm", 0)
end
return 1
