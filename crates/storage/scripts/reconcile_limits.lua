-- Reconcile a token reservation exactly once. Refunds apply only to their
-- original fixed UTC-minute window; positive overage becomes debt in the
-- current window when the request crosses a minute boundary.
-- KEYS: stable rate hash
-- ARGV: reservation window_id, lease_id, reserved_tokens, actual_tokens

local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local MINUTE_MS = 60000
local RECONCILIATION_RETENTION_MS = 900000

local function is_safe_unsigned_integer(raw)
  if type(raw) ~= "string" or string.match(raw, "^%d+$") == nil then
    return false
  end
  local normalized = string.gsub(raw, "^0+", "")
  if normalized == "" then
    normalized = "0"
  end
  if #normalized > #MAX_SAFE_INTEGER_TEXT then
    return false
  end
  return #normalized < #MAX_SAFE_INTEGER_TEXT or normalized <= MAX_SAFE_INTEGER_TEXT
end

if #KEYS ~= 1 or #ARGV ~= 4
    or not is_safe_unsigned_integer(ARGV[1])
    or type(ARGV[2]) ~= "string" or #ARGV[2] < 1 or #ARGV[2] > 128
    or not is_safe_unsigned_integer(ARGV[3])
    or not is_safe_unsigned_integer(ARGV[4]) then
  return -1
end

local server_time = redis.call("TIME")
if type(server_time) ~= "table" or #server_time ~= 2
    or not is_safe_unsigned_integer(server_time[1])
    or not is_safe_unsigned_integer(server_time[2]) then
  return -1
end
local seconds = tonumber(server_time[1])
local microseconds = tonumber(server_time[2])
if microseconds >= 1000000 or seconds > 9007199254739 then
  return -1
end
local current_window = math.floor(seconds / 60)
local reservation_window = tonumber(ARGV[1])
if reservation_window > current_window then
  return -1
end

local marker = "reconciled:" .. ARGV[2]
local key_type = redis.call("TYPE", KEYS[1]).ok
if key_type ~= "none" and key_type ~= "hash" then
  return -1
end
if key_type == "hash" and redis.call("HEXISTS", KEYS[1], marker) == 1 then
  return 0
end

local stored_window = nil
local rpm = 0
local current = 0
if key_type == "hash" then
  local state = redis.call("HMGET", KEYS[1], "window", "rpm", "tpm")
  local present = 0
  for index = 1, 3 do
    if state[index] ~= false then
      present = present + 1
    end
  end
  if present ~= 0 and present ~= 3 then
    return -1
  end
  if present == 3 then
    if not is_safe_unsigned_integer(state[1])
        or not is_safe_unsigned_integer(state[2])
        or not is_safe_unsigned_integer(state[3]) then
      return -1
    end
    stored_window = tonumber(state[1])
    if stored_window > current_window then
      return -1
    end
    if stored_window == current_window then
      rpm = tonumber(state[2])
      current = tonumber(state[3])
    end
  end
end

local reserved = tonumber(ARGV[3])
local actual = tonumber(ARGV[4])
local updated = current
if actual > reserved then
  local charge = actual - reserved
  if charge > tonumber(MAX_SAFE_INTEGER_TEXT) - current then
    updated = tonumber(MAX_SAFE_INTEGER_TEXT)
  else
    updated = current + charge
  end
elseif reserved > actual and stored_window == reservation_window then
  updated = math.max(0, current - (reserved - actual))
end

if stored_window == current_window then
  redis.call("HSET", KEYS[1], "tpm", updated, marker, "1")
elseif actual > reserved then
  redis.call(
    "HSET",
    KEYS[1],
    "window",
    current_window,
    "rpm",
    rpm,
    "tpm",
    updated,
    marker,
    "1"
  )
else
  return 0
end

-- The accounting mutation and marker share the HSET above. Expiration only
-- bounds retained evidence and cannot make a retry double-apply the update.
redis.call(
  "HPEXPIRE",
  KEYS[1],
  RECONCILIATION_RETENTION_MS,
  "FIELDS",
  1,
  marker
)
local elapsed_in_minute_ms = (seconds % 60) * 1000 + math.floor(microseconds / 1000)
local required_ttl = MINUTE_MS - elapsed_in_minute_ms + RECONCILIATION_RETENTION_MS
local current_ttl = redis.call("PTTL", KEYS[1])
if current_ttl >= 0 and current_ttl < required_ttl then
  redis.call("PEXPIRE", KEYS[1], required_ttl)
end
return 1
