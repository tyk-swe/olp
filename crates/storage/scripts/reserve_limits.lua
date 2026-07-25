-- Atomic fixed-UTC-minute RPM/TPM and expiring concurrency reservation.
-- Response v1:
--   {version, status, dimension_or_error, retry_after_ms, window_id,
--    concurrency_lease_expires_at_ms}
-- status: 1 = granted, 0 = rejected, -1 = malformed state/arguments.
--
-- KEYS: stable rate hash, concurrency zset. Both keys must carry the same
--       Valkey Cluster hash tag.
-- ARGV: rpm_limit, tpm_limit, requested_tokens, concurrency_limit, lease_id,
--       lease_ttl_ms. A zero limit means that dimension is unlimited.

local RESPONSE_VERSION = 1
local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local MINUTE_MS = 60000

local function failure(reason)
  return {RESPONSE_VERSION, -1, reason, 0, 0, 0}
end

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
  if #normalized == #MAX_SAFE_INTEGER_TEXT and normalized > MAX_SAFE_INTEGER_TEXT then
    return false
  end
  return true
end

local function parse_safe_unsigned_integer(raw)
  if not is_safe_unsigned_integer(raw) then
    return nil
  end
  return tonumber(raw)
end

if #KEYS ~= 2 or #ARGV ~= 6 then
  return failure("invalid_arguments")
end

local rpm_limit = parse_safe_unsigned_integer(ARGV[1])
local tpm_limit = parse_safe_unsigned_integer(ARGV[2])
local requested_tokens = parse_safe_unsigned_integer(ARGV[3])
local concurrency_limit = parse_safe_unsigned_integer(ARGV[4])
local lease_id = ARGV[5]
local lease_ttl = parse_safe_unsigned_integer(ARGV[6])

if rpm_limit == nil or tpm_limit == nil or requested_tokens == nil
    or concurrency_limit == nil or lease_ttl == nil
    or (rpm_limit > 0 and rpm_limit < 1)
    or (tpm_limit > 0 and tpm_limit < 1)
    or (concurrency_limit > 0 and concurrency_limit < 1)
    or (tpm_limit > 0 and requested_tokens < 1)
    or lease_ttl < 1
    or type(lease_id) ~= "string" or #lease_id < 1 or #lease_id > 128 then
  return failure("invalid_arguments")
end

-- Valkey is the only clock authority. Seconds are currently small enough that
-- seconds * 1000 is exactly representable by Lua 5.1's IEEE-754 number. Check
-- the textual value before conversion so this remains true as the epoch grows.
local server_time = redis.call("TIME")
if type(server_time) ~= "table" or #server_time ~= 2
    or not is_safe_unsigned_integer(server_time[1])
    or not is_safe_unsigned_integer(server_time[2]) then
  return failure("invalid_server_time")
end
local seconds = tonumber(server_time[1])
local microseconds = tonumber(server_time[2])
if microseconds >= 1000000 or seconds > 9007199254739 then
  return failure("invalid_server_time")
end

local now_ms = seconds * 1000 + math.floor(microseconds / 1000)
local window_id = math.floor(seconds / 60)
local elapsed_in_minute_ms = (seconds % 60) * 1000 + math.floor(microseconds / 1000)
local window_remaining_ms = MINUTE_MS - elapsed_in_minute_ms
if window_remaining_ms < 1 or window_remaining_ms > MINUTE_MS then
  return failure("invalid_server_time")
end

local lease_expires_at_ms = 0
if concurrency_limit > 0 then
  if lease_ttl > 9007199254740991 - now_ms then
    return failure("invalid_arguments")
  end
  lease_expires_at_ms = now_ms + lease_ttl
end

local rate_enabled = rpm_limit > 0 or tpm_limit > 0
local rate_is_current = false
local rpm = 0
local tpm = 0

if rate_enabled then
  local state = redis.call("HMGET", KEYS[1], "window", "rpm", "tpm")
  local present = 0
  for index = 1, 3 do
    if state[index] ~= false then
      present = present + 1
    end
  end

  if present ~= 0 and present ~= 3 then
    return {RESPONSE_VERSION, -1, "malformed_rate_state", 0, window_id, 0}
  end

  if present == 3 then
    local stored_window = parse_safe_unsigned_integer(state[1])
    local stored_rpm = parse_safe_unsigned_integer(state[2])
    local stored_tpm = parse_safe_unsigned_integer(state[3])
    if stored_window == nil or stored_rpm == nil or stored_tpm == nil
        or stored_window > window_id then
      return {RESPONSE_VERSION, -1, "malformed_rate_state", 0, window_id, 0}
    end
    if stored_window == window_id then
      rate_is_current = true
      rpm = stored_rpm
      tpm = stored_tpm
    end
  end
end

if rpm_limit > 0 and rpm >= rpm_limit then
  return {RESPONSE_VERSION, 0, "rpm", window_remaining_ms, window_id, 0}
end
-- Subtraction avoids forming a potentially inexact sum near Lua's largest
-- exactly representable integer.
if tpm_limit > 0
    and (requested_tokens > tpm_limit or tpm > tpm_limit - requested_tokens) then
  return {RESPONSE_VERSION, 0, "tpm", window_remaining_ms, window_id, 0}
end

local concurrency = 0
local newest_concurrency_expiry = 0
if concurrency_limit > 0 then
  redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", now_ms)
  concurrency = tonumber(redis.call("ZCARD", KEYS[2]))
  if concurrency > 0 then
    local oldest = redis.call("ZRANGE", KEYS[2], 0, 0, "WITHSCORES")
    if type(oldest) ~= "table" or #oldest ~= 2
        or not is_safe_unsigned_integer(oldest[2]) then
      return {RESPONSE_VERSION, -1, "malformed_concurrency_state", 0, window_id, 0}
    end
    local oldest_expiry = tonumber(oldest[2])
    if oldest_expiry <= now_ms then
      return {RESPONSE_VERSION, -1, "malformed_concurrency_state", 0, window_id, 0}
    end
    local newest = redis.call("ZRANGE", KEYS[2], -1, -1, "WITHSCORES")
    if type(newest) ~= "table" or #newest ~= 2
        or not is_safe_unsigned_integer(newest[2]) then
      return {RESPONSE_VERSION, -1, "malformed_concurrency_state", 0, window_id, 0}
    end
    newest_concurrency_expiry = tonumber(newest[2])
    if newest_concurrency_expiry < oldest_expiry then
      return {RESPONSE_VERSION, -1, "malformed_concurrency_state", 0, window_id, 0}
    end
    if concurrency >= concurrency_limit then
      return {
        RESPONSE_VERSION,
        0,
        "concurrency",
        oldest_expiry - now_ms,
        window_id,
        0
      }
    end
  elseif concurrency >= concurrency_limit then
    -- This is unreachable for a positive configured limit, but fail safely if
    -- the representation or command behavior ever changes.
    return {RESPONSE_VERSION, -1, "malformed_concurrency_state", 0, window_id, 0}
  end
end

-- Mutate capacity only after every dimension has admitted the reservation.
if rate_enabled then
  if not rate_is_current then
    redis.call(
      "HSET",
      KEYS[1],
      "window",
      window_id,
      "rpm",
      rpm_limit > 0 and 1 or 0,
      "tpm",
      tpm_limit > 0 and requested_tokens or 0
    )
    redis.call("PEXPIRE", KEYS[1], window_remaining_ms)
  else
    if rpm_limit > 0 then
      redis.call("HINCRBY", KEYS[1], "rpm", 1)
    end
    if tpm_limit > 0 then
      redis.call("HINCRBY", KEYS[1], "tpm", requested_tokens)
    end
    -- Repair manually-created state without moving the fixed UTC boundary.
    if redis.call("PTTL", KEYS[1]) < 1 then
      redis.call("PEXPIRE", KEYS[1], window_remaining_ms)
    end
  end
end

if concurrency_limit > 0 then
  redis.call("ZADD", KEYS[2], lease_expires_at_ms, lease_id)
  if lease_expires_at_ms > newest_concurrency_expiry then
    newest_concurrency_expiry = lease_expires_at_ms
  end
  redis.call("PEXPIRE", KEYS[2], newest_concurrency_expiry - now_ms)
end

return {RESPONSE_VERSION, 1, "ok", 0, window_id, lease_expires_at_ms}
