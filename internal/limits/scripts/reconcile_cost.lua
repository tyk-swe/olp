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
  -- Fifteen digits or fewer always fit exactly.
  if #raw < 16 then
    return tonumber(raw)
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


-- BEGIN cost_pending (byte-identical in reserve_cost, settle_cost and reconcile_cost; a Go test enforces it)
-- Pending reservations live in two keys that share the cost owner's hash tag:
--   pending: hash, field "sum" (the total) and one field per lease holding its amount
--   expiry:  zset, lease -> expiry in Unix milliseconds
-- Reservations are advisory, derived and short lived; the accrued totals stay
-- authoritative.
--
-- An amount carries up to 12 fractional digits, more than a Lua number holds, so
-- it travels as two exact integers: whole units and the fraction in units of
-- 10^-12. An amount only becomes text through format_amount and only comes from
-- text through parse_amount, because Lua writes a number of more than 14 digits
-- in exponent form: a limb must never be concatenated or sent to a command as it
-- is. An amount of 10^15 whole units or more is held as exactly 10^15, which is
-- above every limit (each is below 10^12) and below the 2^53 Lua counts exactly.
local SCALE = 12
local UNIT = 1000000000000
local HUGE = 1000000000000000
-- Turns a fraction of n digits into units of 10^-12: its unit is 10^(12 - n).
local FRACTION_UNIT = {
  [0] = 1, 10, 100, 1000, 10000, 100000, 1000000, 10000000, 100000000, 1000000000,
  10000000000, 100000000000,
}
-- One call retires at most this many lapsed leases. Each costs a few
-- microseconds inside the single thread that serves every client of the Valkey,
-- so this cap and not the size of a backlog bounds how long one reservation or
-- settlement can hold it. A lease past the cap keeps counting until a later call
-- retires it, which only ever holds a budget back for longer than it should.
local SWEEP_LIMIT = 256

-- Whole and fractional units of a plain decimal, or nil for text that is not one
-- or has more than 12 significant fractional digits.
local function parse_amount(raw)
  if type(raw) ~= "string" then
    return nil
  end
  local whole, point, fraction = string.match(raw, "^(%d+)(%.?)(%d*)$")
  if whole == nil or (point ~= "" and fraction == "") then
    return nil
  end
  if #fraction > SCALE then
    fraction = string.gsub(fraction, "0+$", "")
    if #fraction > SCALE then
      return nil
    end
  end
  local hi = tonumber(whole)
  if hi >= HUGE then
    return HUGE, 0
  end
  if fraction == "" then
    return hi, 0
  end
  return hi, tonumber(fraction) * FRACTION_UNIT[SCALE - #fraction]
end

-- The canonical text of an amount: no leading zeros, no trailing fractional zeros.
local function format_amount(hi, lo)
  if lo == 0 then
    return string.format("%d", hi)
  end
  return string.format("%d.%s", hi, (string.gsub(string.format("%012d", lo), "0+$", "")))
end

local function compare_amount(left_hi, left_lo, right_hi, right_lo)
  if left_hi ~= right_hi then
    return left_hi < right_hi and -1 or 1
  end
  if left_lo ~= right_lo then
    return left_lo < right_lo and -1 or 1
  end
  return 0
end

local function add_amount(left_hi, left_lo, right_hi, right_lo)
  local hi, lo = left_hi + right_hi, left_lo + right_lo
  if lo >= UNIT then
    hi, lo = hi + 1, lo - UNIT
  end
  if hi >= HUGE then
    return HUGE, 0
  end
  return hi, lo
end

-- left - right, floored at zero so a drifted sum can never go negative.
local function sub_amount(left_hi, left_lo, right_hi, right_lo)
  if compare_amount(left_hi, left_lo, right_hi, right_lo) <= 0 then
    return 0, 0
  end
  local hi, lo = left_hi - right_hi, left_lo - right_lo
  if lo < 0 then
    hi, lo = hi - 1, lo + UNIT
  end
  return hi, lo
end

local function valid_lease(lease_id)
  return type(lease_id) == "string" and #lease_id >= 32 and #lease_id <= 64
    and string.match(lease_id, "^[0-9a-f%-]+$") ~= nil
end

local function drop_pending(pending_key, expiry_key)
  redis.call("DEL", pending_key, expiry_key)
end

-- The total held once up to SWEEP_LIMIT lapsed leases have been retired. State this
-- script cannot interpret is dropped rather than trusted, so that no command here
-- can raise WRONGTYPE and a damaged reservation never stops a request or an
-- accrual: the hash holds "sum" and one field per lease and the set one member per
-- lease, so keys that disagree mean one of them lost state, and no sum read from
-- either would describe what is held.
local function pending_total(pending_key, expiry_key, now_ms)
  local stored = redis.pcall("HGET", pending_key, "sum")
  local leases = redis.pcall("ZCARD", expiry_key)
  if type(stored) == "table" or type(leases) == "table" then
    drop_pending(pending_key, expiry_key)
    return 0, 0
  end
  if stored == false then
    -- Nothing is held, or a hash has lost its total.
    if leases > 0 or redis.call("EXISTS", pending_key) == 1 then
      drop_pending(pending_key, expiry_key)
    end
    return 0, 0
  end
  local sum_hi, sum_lo = parse_amount(stored)
  if sum_hi == nil or leases < 1 or redis.call("HLEN", pending_key) ~= leases + 1 then
    drop_pending(pending_key, expiry_key)
    return 0, 0
  end
  local expired = redis.call("ZRANGEBYSCORE", expiry_key, "-inf", now_ms, "LIMIT", 0, SWEEP_LIMIT)
  if #expired == 0 then
    return sum_hi, sum_lo
  end
  local amounts = redis.call("HMGET", pending_key, unpack(expired))
  for index = 1, #amounts do
    local hi, lo = parse_amount(amounts[index])
    if hi ~= nil then
      sum_hi, sum_lo = sub_amount(sum_hi, sum_lo, hi, lo)
    end
  end
  redis.call("HDEL", pending_key, unpack(expired))
  redis.call("ZREM", expiry_key, unpack(expired))
  if redis.call("HLEN", pending_key) <= 1 then
    drop_pending(pending_key, expiry_key)
    return 0, 0
  end
  redis.call("HSET", pending_key, "sum", format_amount(sum_hi, sum_lo))
  return sum_hi, sum_lo
end

-- Writes lease at the amount, replacing any earlier one, and the total the caller
-- has worked out for the hash. A settlement passes lower_only so that it can
-- shorten a lease but never extend one: the expiry set when the lease was granted
-- is the backstop, and a settlement that fails must not weaken it.
local function store_lease(pending_key, expiry_key, lease_id, amount_hi, amount_lo, total_hi, total_lo, expires_ms, lower_only)
  redis.call("HSET", pending_key, lease_id, format_amount(amount_hi, amount_lo), "sum", format_amount(total_hi, total_lo))
  if lower_only then
    redis.call("ZADD", expiry_key, "LT", expires_ms, lease_id)
  else
    redis.call("ZADD", expiry_key, expires_ms, lease_id)
  end
end

-- Lets both keys outlive a lease that expires at expires_ms by a second, and
-- whatever they already outlive: the newest lease sets how long they stay.
local function outlive(pending_key, expiry_key, expires_ms, now_ms)
  local ttl = math.max(redis.call("PTTL", pending_key), expires_ms - now_ms + 1000)
  redis.call("PEXPIRE", pending_key, ttl)
  redis.call("PEXPIRE", expiry_key, ttl)
end

-- Removes a lease that the caller has read, holding amount (text), from a total of
-- sum_hi, sum_lo, which the lease is part of.
local function remove_lease(pending_key, expiry_key, lease_id, amount, sum_hi, sum_lo)
  if redis.call("HLEN", pending_key) <= 2 then
    -- This lease and the total are all that is held.
    drop_pending(pending_key, expiry_key)
    return
  end
  local hi, lo = parse_amount(amount)
  if hi ~= nil then
    sum_hi, sum_lo = sub_amount(sum_hi, sum_lo, hi, lo)
  end
  redis.call("HDEL", pending_key, lease_id)
  if type(redis.pcall("ZREM", expiry_key, lease_id)) == "table" then
    drop_pending(pending_key, expiry_key)
    return
  end
  redis.call("HSET", pending_key, "sum", format_amount(sum_hi, sum_lo))
end

-- Removes lease and its amount; absent leases are a no-op, which is what makes
-- every caller idempotent. Returns 1 when something was removed.
local function release_lease(pending_key, expiry_key, lease_id)
  local held = redis.pcall("HMGET", pending_key, lease_id, "sum")
  if held.err ~= nil then
    drop_pending(pending_key, expiry_key)
    return 0
  end
  if held[1] == false then
    return 0
  end
  local sum_hi, sum_lo = parse_amount(held[2])
  if sum_hi == nil then
    sum_hi, sum_lo = 0, 0
  end
  remove_lease(pending_key, expiry_key, lease_id, held[1], sum_hi, sum_lo)
  return 1
end
-- END cost_pending

if #KEYS ~= 4 or #ARGV ~= 7 then
  return failure("invalid_arguments")
end

local snapshot_day = parse_safe_unsigned_integer(ARGV[1])
local daily_accrued = normalize_decimal(ARGV[2])
local snapshot_month = parse_safe_unsigned_integer(ARGV[3])
local monthly_accrued = normalize_decimal(ARGV[4])
local unpriced = parse_safe_unsigned_integer(ARGV[5])
local override = parse_safe_unsigned_integer(ARGV[6])
local request_id = ARGV[7]
if snapshot_day == nil or daily_accrued == nil or snapshot_month == nil
    or monthly_accrued == nil or unpriced == nil or override == nil
    or (request_id ~= "" and not valid_lease(request_id)) then
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

-- The request this snapshot accounts for stops being reserved in the same step
-- that its spend is installed, so the budget never counts it twice, and a replay
-- finds nothing left to remove.
if request_id ~= "" then
  release_lease(KEYS[3], KEYS[4], request_id)
end

return {RESPONSE_VERSION, 1, "ok", reconciled_daily, reconciled_monthly}
