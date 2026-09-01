local RESPONSE_VERSION = 1
local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local DAY_MS = 86400000

local function failure(reason)
  return {RESPONSE_VERSION, -1, reason, 0, 0, 0}
end

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

local function normalize_decimal(raw)
  if type(raw) ~= "string" or #raw == 0 or #raw > 96
      or string.match(raw, "^%d+%.?%d*$") == nil
      or string.sub(raw, -1) == "." then
    return nil
  end
  local integer, fraction = string.match(raw, "^(%d+)%.?(%d*)$")
  integer = string.gsub(integer, "^0+", "")
  if integer == "" then
    integer = "0"
  end
  fraction = string.gsub(fraction, "0+$", "")
  if fraction == "" then
    return integer
  end
  return integer .. "." .. fraction
end

local function decimal_parts(raw)
  local normalized = normalize_decimal(raw)
  if normalized == nil then
    return nil
  end
  local integer, fraction = string.match(normalized, "^(%d+)%.?(%d*)$")
  return integer, fraction
end

local function compare_decimal(left, right)
  local left_integer, left_fraction = decimal_parts(left)
  local right_integer, right_fraction = decimal_parts(right)
  if left_integer == nil or right_integer == nil then
    return nil
  end
  if #left_integer ~= #right_integer then
    return #left_integer < #right_integer and -1 or 1
  end
  if left_integer ~= right_integer then
    return left_integer < right_integer and -1 or 1
  end
  local scale = math.max(#left_fraction, #right_fraction)
  left_fraction = left_fraction .. string.rep("0", scale - #left_fraction)
  right_fraction = right_fraction .. string.rep("0", scale - #right_fraction)
  if left_fraction == right_fraction then
    return 0
  end
  return left_fraction < right_fraction and -1 or 1
end

local function days_from_civil(year, month, day)
  year = year - (month <= 2 and 1 or 0)
  local era = math.floor(year / 400)
  local year_of_era = year - era * 400
  local adjusted_month = month + (month > 2 and -3 or 9)
  local day_of_year = math.floor((153 * adjusted_month + 2) / 5) + day - 1
  local day_of_era = year_of_era * 365 + math.floor(year_of_era / 4)
    - math.floor(year_of_era / 100) + day_of_year
  return era * 146097 + day_of_era - 719468
end

local function civil_month(days)
  local shifted = days + 719468
  local era = math.floor(shifted / 146097)
  local day_of_era = shifted - era * 146097
  local year_of_era = math.floor((day_of_era - math.floor(day_of_era / 1460)
    + math.floor(day_of_era / 36524) - math.floor(day_of_era / 146096)) / 365)
  local year = year_of_era + era * 400
  local day_of_year = day_of_era
    - (365 * year_of_era + math.floor(year_of_era / 4) - math.floor(year_of_era / 100))
  local adjusted_month = math.floor((5 * day_of_year + 2) / 153)
  local month = adjusted_month + (adjusted_month < 10 and 3 or -9)
  year = year + (month <= 2 and 1 or 0)
  return year, month
end

local function current_time(override)
  if override > 0 then
    return override
  end
  local server_time = redis.call("TIME")
  if type(server_time) ~= "table" or #server_time ~= 2 then
    return nil
  end
  local seconds = parse_safe_unsigned_integer(server_time[1])
  local microseconds = parse_safe_unsigned_integer(server_time[2])
  if seconds == nil or microseconds == nil or microseconds >= 1000000
      or seconds > 9007199254739 then
    return nil
  end
  return seconds * 1000 + math.floor(microseconds / 1000)
end

local function windows(now_ms)
  local day = math.floor(now_ms / DAY_MS)
  local year, month = civil_month(day)
  local month_id = year * 12 + month - 1
  local next_year = year + (month == 12 and 1 or 0)
  local next_month = month == 12 and 1 or month + 1
  local day_remaining = (day + 1) * DAY_MS - now_ms
  local month_remaining = days_from_civil(next_year, next_month, 1) * DAY_MS - now_ms
  if day_remaining < 1 or month_remaining < 1 then
    return nil
  end
  return day, month_id, day_remaining, month_remaining
end

local function read_state(key, expected_fields, current_window)
  local key_type = redis.call("TYPE", key).ok
  if key_type ~= "none" and key_type ~= "hash" then
    return nil
  end
  local values = redis.call("HMGET", key, unpack(expected_fields))
  local present = 0
  for index = 1, #values do
    if values[index] ~= false then
      present = present + 1
    end
  end
  if present == 0 then
    return "missing", "0"
  end
  if present ~= #values then
    return nil
  end
  local stored_window = parse_safe_unsigned_integer(values[1])
  local accrued = normalize_decimal(values[2])
  if stored_window == nil or accrued == nil or stored_window > current_window then
    return nil
  end
  if #values == 3 and parse_safe_unsigned_integer(values[3]) == nil then
    return nil
  end
  if stored_window ~= current_window then
    return "stale", "0"
  end
  return "current", accrued
end

if #KEYS ~= 2 or #ARGV ~= 3 then
  return failure("invalid_arguments")
end

local daily_limit = ARGV[1] == "" and nil or normalize_decimal(ARGV[1])
local monthly_limit = ARGV[2] == "" and nil or normalize_decimal(ARGV[2])
local override = parse_safe_unsigned_integer(ARGV[3])
if override == nil
    or (ARGV[1] ~= "" and (daily_limit == nil or compare_decimal(daily_limit, "0") ~= 1))
    or (ARGV[2] ~= "" and (monthly_limit == nil or compare_decimal(monthly_limit, "0") ~= 1)) then
  return failure("invalid_arguments")
end

local now_ms = current_time(override)
if now_ms == nil then
  return failure("invalid_server_time")
end
local day_window, month_window, day_ttl, month_ttl = windows(now_ms)
if day_window == nil then
  return failure("invalid_server_time")
end

local daily_state, daily_accrued = "disabled", "0"
if daily_limit ~= nil then
  daily_state, daily_accrued = read_state(KEYS[1], {"window", "accrued"}, day_window)
  if daily_state == nil then
    return {RESPONSE_VERSION, -1, "malformed_daily_cost_state", 0, day_window, month_window}
  end
end
local monthly_state, monthly_accrued = "disabled", "0"
if monthly_limit ~= nil then
  monthly_state, monthly_accrued = read_state(
    KEYS[2], {"window", "accrued", "unpriced"}, month_window
  )
  if monthly_state == nil then
    return {RESPONSE_VERSION, -1, "malformed_monthly_cost_state", 0, day_window, month_window}
  end
end

if daily_limit ~= nil and compare_decimal(daily_accrued, daily_limit) >= 0 then
  return {RESPONSE_VERSION, 0, "daily_cost", day_ttl, day_window, month_window}
end
if monthly_limit ~= nil and compare_decimal(monthly_accrued, monthly_limit) >= 0 then
  return {RESPONSE_VERSION, 0, "monthly_cost", month_ttl, day_window, month_window}
end

-- Only authoritative snapshots may initialize a window. Missing state can
-- also mean eviction or data loss, even while Valkey itself is reachable.
if daily_limit ~= nil and daily_state ~= "current" then
  return {RESPONSE_VERSION, -1, "uninitialized_daily_cost_state", 0, day_window, month_window}
end
if monthly_limit ~= nil and monthly_state ~= "current" then
  return {RESPONSE_VERSION, -1, "uninitialized_monthly_cost_state", 0, day_window, month_window}
end

if daily_limit ~= nil and redis.call("PTTL", KEYS[1]) < 1 then
  redis.call("PEXPIRE", KEYS[1], day_ttl)
end
if monthly_limit ~= nil and redis.call("PTTL", KEYS[2]) < 1 then
  redis.call("PEXPIRE", KEYS[2], month_ttl)
end

return {RESPONSE_VERSION, 1, "ok", 0, day_window, month_window}
