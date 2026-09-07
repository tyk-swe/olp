local RESPONSE_VERSION = 1
local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local DAY_MS = 86400000

local function failure(reason)
  return {RESPONSE_VERSION, -1, reason, 0, 0}
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
  return fraction == "" and integer or integer .. "." .. fraction
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
  return day, month_id, (day + 1) * DAY_MS - now_ms,
    days_from_civil(next_year, next_month, 1) * DAY_MS - now_ms
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
    return "missing", "0", 0
  end
  if present ~= #values then
    return nil
  end
  local stored_window = parse_safe_unsigned_integer(values[1])
  local accrued = normalize_decimal(values[2])
  local unpriced = 0
  if #values == 3 then
    unpriced = parse_safe_unsigned_integer(values[3])
  end
  if stored_window == nil or accrued == nil or unpriced == nil or stored_window > current_window then
    return nil
  end
  if stored_window ~= current_window then
    return "stale", "0", 0
  end
  return "current", accrued, unpriced
end

if #KEYS ~= 2 or #ARGV ~= 6 then
  return failure("invalid_arguments")
end

local snapshot_day = parse_safe_unsigned_integer(ARGV[1])
local daily_accrued = normalize_decimal(ARGV[2])
local snapshot_month = parse_safe_unsigned_integer(ARGV[3])
local monthly_accrued = normalize_decimal(ARGV[4])
local unpriced = parse_safe_unsigned_integer(ARGV[5])
local override = parse_safe_unsigned_integer(ARGV[6])
if snapshot_day == nil or daily_accrued == nil or snapshot_month == nil
    or monthly_accrued == nil or unpriced == nil or override == nil then
  return failure("invalid_arguments")
end

local now_ms = current_time(override)
if now_ms == nil then
  return failure("invalid_server_time")
end
local day_window, month_window, day_ttl, month_ttl = windows(now_ms)
if day_ttl < 1 or month_ttl < 1 then
  return failure("invalid_server_time")
end

local daily_state, current_daily = read_state(KEYS[1], {"window", "accrued"}, day_window)
if daily_state == nil then
  daily_state, current_daily = "malformed", "0"
end
local monthly_state, current_monthly, current_unpriced = read_state(
  KEYS[2], {"window", "accrued", "unpriced"}, month_window
)
if monthly_state == nil then
  monthly_state, current_monthly, current_unpriced = "malformed", "0", 0
end

local reconciled_daily = 0
if snapshot_day == day_window then
  if daily_state ~= "current" then
    redis.call("DEL", KEYS[1])
    redis.call("HSET", KEYS[1], "window", day_window, "accrued", daily_accrued)
  elseif compare_decimal(current_daily, daily_accrued) < 0 then
    redis.call("HSET", KEYS[1], "accrued", daily_accrued)
  end
  redis.call("PEXPIRE", KEYS[1], day_ttl)
  reconciled_daily = 1
end

local reconciled_monthly = 0
if snapshot_month == month_window then
  if monthly_state ~= "current" then
    redis.call("DEL", KEYS[2])
    redis.call(
      "HSET", KEYS[2], "window", month_window, "accrued", monthly_accrued,
      "unpriced", unpriced
    )
  else
    if compare_decimal(current_monthly, monthly_accrued) < 0 then
      redis.call("HSET", KEYS[2], "accrued", monthly_accrued)
    end
    if current_unpriced < unpriced then
      redis.call("HSET", KEYS[2], "unpriced", unpriced)
    end
  end
  redis.call("PEXPIRE", KEYS[2], month_ttl)
  reconciled_monthly = 1
end

return {RESPONSE_VERSION, 1, "ok", reconciled_daily, reconciled_monthly}
