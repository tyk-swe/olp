-- Refund unused tokens from a reservation that still belongs to the active
-- fixed UTC-minute window. Reconciliation is idempotent per lease so callers
-- may safely retry an ambiguous transport failure.
-- KEYS: stable rate hash
-- ARGV: reservation window_id, refund_tokens, lease_id

local MAX_SAFE_INTEGER_TEXT = "9007199254740991"

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

local stored_window = redis.call("HGET", KEYS[1], "window")
if stored_window == false or stored_window ~= ARGV[1] then
  return 0
end

local refund = parse_safe_unsigned_integer(ARGV[2])
if refund == nil or refund <= 0 then
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

local current_tpm = parse_safe_unsigned_integer(redis.call("HGET", KEYS[1], "tpm"))
if current_tpm == nil then
  return redis.error_reply("invalid token state")
end
local updated = current_tpm - refund
if updated < 0 then
  updated = 0
end
-- Update the token count and idempotence marker in one command. A script
-- runtime error cannot leave a successful refund without its marker.
redis.call("HSET", KEYS[1], "tpm", updated, reconciliation_field, 1)
return 1
