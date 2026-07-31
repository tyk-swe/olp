-- Reconcile actual tokens against a reservation that still belongs to the
-- active fixed UTC-minute window. Each lease is applied at most once.
-- KEYS: stable rate hash
-- ARGV: reservation window_id, lease_id, signed token delta (actual-reserved)

local MAX_SAFE_INTEGER_TEXT = "9007199254740991"

local function parse_safe_signed_integer(raw)
  if type(raw) ~= "string" or string.match(raw, "^-?%d+$") == nil then
    return nil
  end
  local unsigned = string.gsub(raw, "^-", "")
  local normalized = string.gsub(unsigned, "^0+", "")
  if normalized == "" then
    normalized = "0"
  end
  if #normalized > #MAX_SAFE_INTEGER_TEXT
      or (#normalized == #MAX_SAFE_INTEGER_TEXT and normalized > MAX_SAFE_INTEGER_TEXT) then
    return nil
  end
  return tonumber(raw)
end

if #KEYS ~= 1 or #ARGV ~= 3 or type(ARGV[2]) ~= "string"
    or #ARGV[2] < 1 or #ARGV[2] > 128 then
  return 0
end

local stored_window = redis.call("HGET", KEYS[1], "window")
if stored_window == false or stored_window ~= ARGV[1] then
  return 0
end

local delta = parse_safe_signed_integer(ARGV[3])
local current = parse_safe_signed_integer(redis.call("HGET", KEYS[1], "tpm"))
if delta == nil or current == nil or current < 0 then
  return 0
end

if redis.call("HSETNX", KEYS[1], "reconciled:" .. ARGV[2], 1) == 0 then
  return 0
end

if delta > 0 then
  local maximum = tonumber(MAX_SAFE_INTEGER_TEXT)
  if current > maximum - delta then
    redis.call("HSET", KEYS[1], "tpm", MAX_SAFE_INTEGER_TEXT)
  else
    redis.call("HINCRBY", KEYS[1], "tpm", delta)
  end
elseif delta < 0 then
  local updated = redis.call("HINCRBY", KEYS[1], "tpm", delta)
  if tonumber(updated) < 0 then
    redis.call("HSET", KEYS[1], "tpm", 0)
  end
end
return 1
