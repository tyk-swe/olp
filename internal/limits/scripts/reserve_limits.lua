-- Atomic fixed-UTC-minute RPM/TPM and expiring concurrency reservation.
-- Response v2:
--   {version, status, dimension_or_error, retry_after_ms, window_id,
--    concurrency_lease_expires_at_ms}
-- followed, when the call asked for it, by
--   {rpm_limit, rpm_remaining, tpm_limit, tpm_remaining, window_remaining_ms}
-- status: 1 = granted, 0 = rejected, -1 = malformed state/arguments.
-- A grant or a rejection answers the five allowance fields exactly when
-- report_allowance is 1, and the six fields alone otherwise, so a caller that
-- does not read the allowance is not made to receive it. A status of -1 answers
-- the six fields alone whatever was asked, because it can come before the call
-- has been read.
-- A limit of 0 means that dimension is unlimited and its remaining is 0. A
-- remaining count is what the limit leaves once this request's reservation is
-- counted, and a rejected request reserves nothing, so its counts are what the
-- window still holds. A counter already above its limit leaves 0.
-- window_remaining_ms is the time to the end of the fixed UTC minute the counts
-- belong to, and is 0 when neither a request nor a token limit applies.
--
-- KEYS: stable rate hash, concurrency zset. Both keys must carry the same
--       Valkey Cluster hash tag.
-- ARGV: rpm_limit, tpm_limit, requested_tokens, concurrency_limit, lease_id,
--       lease_ttl_ms, report_allowance (0 or 1), and optionally share_class,
--       share_percent, saturation_percent. A zero limit means that dimension is
--       unlimited.
--
-- Capacity shares divide each limit between admission classes. While a
-- dimension's use, this request included, stays within saturation_percent of
-- its limit, any class may take idle capacity; beyond it, the class is held to
-- share_percent of the limit. A class's requests and tokens are counted in the
-- rate hash as rpm:<class> and tpm:<class>, and its concurrency leases are the
-- members named <class>.<id>, so every class is judged against the same
-- windows in the same atomic step.

local RESPONSE_VERSION = 2
local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local MINUTE_MS = 60000

local function failure(reason, window_id)
  return {RESPONSE_VERSION, -1, reason, 0, window_id or 0, 0}
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

if #KEYS ~= 2 or (#ARGV ~= 7 and #ARGV ~= 10) then
  return failure("invalid_arguments")
end

local rpm_limit = parse_safe_unsigned_integer(ARGV[1])
local tpm_limit = parse_safe_unsigned_integer(ARGV[2])
local requested_tokens = parse_safe_unsigned_integer(ARGV[3])
local concurrency_limit = parse_safe_unsigned_integer(ARGV[4])
local lease_id = ARGV[5]
local lease_ttl = parse_safe_unsigned_integer(ARGV[6])
local report_allowance = ARGV[7]
local share_class = nil
local share_percent = 0
local saturation_percent = 100
if #ARGV == 10 then
  share_class = ARGV[8]
  share_percent = parse_safe_unsigned_integer(ARGV[9])
  saturation_percent = parse_safe_unsigned_integer(ARGV[10])
  if type(share_class) ~= "string" or string.match(share_class, "^[a-z]+$") == nil
      or #share_class > 16 or share_percent == nil or share_percent > 100
      or saturation_percent == nil or saturation_percent < 1 or saturation_percent > 100
      or string.sub(lease_id, 1, #share_class + 1) ~= share_class .. "." then
    return failure("invalid_arguments")
  end
end

if rpm_limit == nil or tpm_limit == nil or requested_tokens == nil
    or concurrency_limit == nil or lease_ttl == nil
    or (rpm_limit > 0 and rpm_limit < 1)
    or (tpm_limit > 0 and tpm_limit < 1)
    or (concurrency_limit > 0 and concurrency_limit < 1)
    or (tpm_limit > 0 and requested_tokens < 1)
    or lease_ttl < 1
    or (report_allowance ~= "0" and report_allowance ~= "1")
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
local class_rpm = 0
local class_tpm = 0

-- What a limit leaves once used is spent. A counter above its limit, which a
-- reconciliation or a lowered limit can leave, allows nothing rather than a
-- negative amount.
local function allowance(limit, used)
  if limit == 0 or used >= limit then
    return 0
  end
  return limit - used
end

-- portion is percent of limit, rounded down, without forming limit * percent,
-- which can exceed Lua's exact integers for a large token limit.
local function portion(limit, percent)
  return math.floor(limit / 100) * percent + math.floor((limit % 100) * percent / 100)
end

-- within reports whether a class may hold class_used of a dimension whose use
-- would be total_used: freely up to the saturation point, and to its share
-- beyond it.
local function within(limit, total_used, class_used)
  return total_used <= portion(limit, saturation_percent)
    or class_used <= portion(limit, share_percent)
end

-- The answer to a request the state was readable for. rpm_used and tpm_used are
-- the counters after this request's reservation, which for a rejection are the
-- counters as they were.
local function decision(status, detail, retry_after_ms, lease_expires_at_ms, rpm_used, tpm_used)
  if report_allowance == "0" then
    return {
      RESPONSE_VERSION,
      status,
      detail,
      retry_after_ms,
      window_id,
      lease_expires_at_ms
    }
  end
  return {
    RESPONSE_VERSION,
    status,
    detail,
    retry_after_ms,
    window_id,
    lease_expires_at_ms,
    rpm_limit,
    allowance(rpm_limit, rpm_used),
    tpm_limit,
    allowance(tpm_limit, tpm_used),
    rate_enabled and window_remaining_ms or 0
  }
end

if rate_enabled then
  local kind = redis.call("TYPE", KEYS[1]).ok
  if kind ~= "none" and kind ~= "hash" then
    return failure("malformed_rate_state", window_id)
  end
  local state = redis.call("HMGET", KEYS[1], "window", "rpm", "tpm")
  local present = 0
  for index = 1, 3 do
    if state[index] ~= false then
      present = present + 1
    end
  end

  if present ~= 0 and present ~= 3 then
    return failure("malformed_rate_state", window_id)
  end

  if present == 3 then
    local stored_window = parse_safe_unsigned_integer(state[1])
    local stored_rpm = parse_safe_unsigned_integer(state[2])
    local stored_tpm = parse_safe_unsigned_integer(state[3])
    if stored_window == nil or stored_rpm == nil or stored_tpm == nil
        or stored_window > window_id then
      return failure("malformed_rate_state", window_id)
    end
    if stored_window == window_id then
      rate_is_current = true
      rpm = stored_rpm
      tpm = stored_tpm
    end
  end

  if share_class ~= nil and rate_is_current then
    local held = redis.call("HMGET", KEYS[1], "rpm:" .. share_class, "tpm:" .. share_class)
    class_rpm = held[1] == false and 0 or parse_safe_unsigned_integer(held[1])
    class_tpm = held[2] == false and 0 or parse_safe_unsigned_integer(held[2])
    if class_rpm == nil or class_tpm == nil then
      return failure("malformed_rate_state", window_id)
    end
  end
end

if rpm_limit > 0 and rpm >= rpm_limit then
  return decision(0, "rpm", window_remaining_ms, 0, rpm, tpm)
end
-- Subtraction avoids forming a potentially inexact sum near Lua's largest
-- exactly representable integer.
if tpm_limit > 0
    and (requested_tokens > tpm_limit or tpm > tpm_limit - requested_tokens) then
  return decision(0, "tpm", window_remaining_ms, 0, rpm, tpm)
end
-- Both sums stay within their limits here, so they are exact.
if share_class ~= nil then
  if rpm_limit > 0 and not within(rpm_limit, rpm + 1, class_rpm + 1) then
    return decision(0, "rpm", window_remaining_ms, 0, rpm, tpm)
  end
  if tpm_limit > 0
      and not within(tpm_limit, tpm + requested_tokens, class_tpm + requested_tokens) then
    return decision(0, "tpm", window_remaining_ms, 0, rpm, tpm)
  end
end

local concurrency = 0
local newest_concurrency_expiry = 0
-- A class refused its share of an idle dimension waits for the next lease to
-- end, or a second when none is held.
local concurrency_retry_ms = 1000
if concurrency_limit > 0 then
  local kind = redis.call("TYPE", KEYS[2]).ok
  if kind ~= "none" and kind ~= "zset" then
    return failure("malformed_concurrency_state", window_id)
  end
  redis.call("ZREMRANGEBYSCORE", KEYS[2], "-inf", now_ms)
  concurrency = tonumber(redis.call("ZCARD", KEYS[2]))
  if concurrency > 0 then
    local oldest = redis.call("ZRANGE", KEYS[2], 0, 0, "WITHSCORES")
    if type(oldest) ~= "table" or #oldest ~= 2
        or not is_safe_unsigned_integer(oldest[2]) then
      return failure("malformed_concurrency_state", window_id)
    end
    local oldest_expiry = tonumber(oldest[2])
    if oldest_expiry <= now_ms then
      return failure("malformed_concurrency_state", window_id)
    end
    local newest = redis.call("ZRANGE", KEYS[2], -1, -1, "WITHSCORES")
    if type(newest) ~= "table" or #newest ~= 2
        or not is_safe_unsigned_integer(newest[2]) then
      return failure("malformed_concurrency_state", window_id)
    end
    newest_concurrency_expiry = tonumber(newest[2])
    if newest_concurrency_expiry < oldest_expiry then
      return failure("malformed_concurrency_state", window_id)
    end
    if concurrency >= concurrency_limit then
      return decision(0, "concurrency", oldest_expiry - now_ms, 0, rpm, tpm)
    end
    concurrency_retry_ms = oldest_expiry - now_ms
  elseif concurrency >= concurrency_limit then
    -- This is unreachable for a positive configured limit, but fail safely if
    -- the representation or command behavior ever changes.
    return failure("malformed_concurrency_state", window_id)
  end
  if share_class ~= nil then
    local prefix = share_class .. "."
    local class_concurrency = 0
    if concurrency > 0 then
      for _, member in ipairs(redis.call("ZRANGE", KEYS[2], 0, -1)) do
        if string.sub(member, 1, #prefix) == prefix then
          class_concurrency = class_concurrency + 1
        end
      end
    end
    if not within(concurrency_limit, concurrency + 1, class_concurrency + 1) then
      return decision(0, "concurrency", concurrency_retry_ms, 0, rpm, tpm)
    end
  end
end

-- Mutate capacity only after every dimension has admitted the reservation.
if rate_enabled then
  if not rate_is_current then
    -- Reconciliation markers belong to the previous fixed window. Delete the
    -- stale hash before initializing the new one so markers cannot accumulate
    -- indefinitely on continuously active API keys.
    redis.call("DEL", KEYS[1])
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
  if share_class ~= nil then
    if rpm_limit > 0 then
      redis.call("HINCRBY", KEYS[1], "rpm:" .. share_class, 1)
    end
    if tpm_limit > 0 then
      redis.call("HINCRBY", KEYS[1], "tpm:" .. share_class, requested_tokens)
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

-- A limited counter stays within its limit here, so the sums are exact.
return decision(1, "ok", 0, lease_expires_at_ms, rpm + 1, tpm + requested_tokens)
