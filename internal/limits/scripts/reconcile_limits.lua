-- Reconcile actual tokens against a reservation that still belongs to the
-- active fixed UTC-minute window. Reconciliation is idempotent per lease so
-- callers may safely retry an ambiguous transport failure.
-- KEYS: stable rate hash
-- ARGV: reservation window_id, token_adjustment, lease_id, and the share class
--       the lease was counted in, if any.

local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local MAX_SAFE_INTEGER = tonumber(MAX_SAFE_INTEGER_TEXT)

local function parse_safe_unsigned_integer(raw)
  if type(raw) ~= "string" or string.match(raw, "^%d+$") == nil then
    return nil
  end
  local normalized = string.gsub(raw, "^0+", "")
  if normalized == "" then
    normalized = "0"
  end
  if #normalized > #MAX_SAFE_INTEGER_TEXT
      or (#normalized == #MAX_SAFE_INTEGER_TEXT and normalized > MAX_SAFE_INTEGER_TEXT) then
    return nil
  end
  return tonumber(normalized)
end


local function parse_safe_signed_integer(raw)
  if type(raw) ~= "string" or string.match(raw, "^-?%d+$") == nil then
    return nil
  end
  local negative = string.sub(raw, 1, 1) == "-"
  local digits = negative and string.sub(raw, 2) or raw
  local normalized = string.gsub(digits, "^0+", "")
  if normalized == "" then
    normalized = "0"
  end
  if #normalized > #MAX_SAFE_INTEGER_TEXT
      or (#normalized == #MAX_SAFE_INTEGER_TEXT and normalized > MAX_SAFE_INTEGER_TEXT) then
    return nil
  end
  local value = tonumber(normalized)
  if negative and value ~= 0 then
    return -value
  end
  return value
end

local stored_window = redis.call("HGET", KEYS[1], "window")
if stored_window == false or stored_window ~= ARGV[1] then
  return 0
end

local adjustment = parse_safe_signed_integer(ARGV[2])
if adjustment == nil or adjustment == 0 then
  return 0
end

local lease_id = ARGV[3]
if type(lease_id) ~= "string" or #lease_id < 1 or #lease_id > 128 then
  return redis.error_reply("invalid lease ID")
end
local reconciliation_field = "reconciled:" .. lease_id
if redis.call("HEXISTS", KEYS[1], reconciliation_field) == 1 then
  return 0
end

local class = ARGV[4]
if class ~= nil and string.match(class, "^[a-z]+$") == nil then
  return redis.error_reply("invalid share class")
end

local current_tpm = parse_safe_unsigned_integer(redis.call("HGET", KEYS[1], "tpm"))
if current_tpm == nil then
  return redis.error_reply("invalid token state")
end

local function adjusted(current)
  if adjustment > 0 and current > MAX_SAFE_INTEGER - adjustment then
    -- The configured limit cannot exceed this ceiling, so saturation preserves
    -- fail-closed enforcement without storing an inexact Lua number.
    return MAX_SAFE_INTEGER
  end
  return math.max(0, current + adjustment)
end

-- Update the token counts and idempotence marker in one command. A script
-- runtime error cannot leave a successful adjustment without its marker.
if class ~= nil then
  local class_field = "tpm:" .. class
  local class_tpm = redis.call("HGET", KEYS[1], class_field)
  class_tpm = class_tpm == false and 0 or parse_safe_unsigned_integer(class_tpm)
  if class_tpm == nil then
    return redis.error_reply("invalid token state")
  end
  redis.call("HSET", KEYS[1], "tpm", adjusted(current_tpm), class_field, adjusted(class_tpm),
             reconciliation_field, 1)
else
  redis.call("HSET", KEYS[1], "tpm", adjusted(current_tpm), reconciliation_field, 1)
end
return 1
