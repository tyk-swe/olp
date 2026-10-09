local RESPONSE_VERSION = 1
local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local DAY_MS = 86400000
local PENDING_RETRY_MS = 1000

local function failure(reason)
  return {RESPONSE_VERSION, -1, reason, 0, 0, 0}
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
local function pending_total(pending_key, expiry_key, now_ms, read_only)
  local stored = redis.pcall("HGET", pending_key, "sum")
  local leases = redis.pcall("ZCARD", expiry_key)
  if type(stored) == "table" or type(leases) == "table" then
    if not read_only then drop_pending(pending_key, expiry_key) end
    return 0, 0
  end
  if stored == false then
    -- Nothing is held, or a hash has lost its total.
    if not read_only and (leases > 0 or redis.call("EXISTS", pending_key) == 1) then
      drop_pending(pending_key, expiry_key)
    end
    return 0, 0
  end
  local sum_hi, sum_lo = parse_amount(stored)
  if sum_hi == nil or leases < 1 or redis.call("HLEN", pending_key) ~= leases + 1 then
    if not read_only then drop_pending(pending_key, expiry_key) end
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
  if read_only then
    return sum_hi, sum_lo
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

local function read_state(key, expected_fields, current_window, now_ms, fallback_ttl)
  local base_window = current_window
  local count = #expected_fields
  expected_fields[count+1] = "starts_at"
  expected_fields[count+2] = "ends_at"
  local values = redis.pcall("HMGET", key, unpack(expected_fields))
  -- A key of another type answers with an error, not an array.
  if values.err ~= nil then
    return nil,0,0,fallback_ttl,base_window
  end
  local present = 0
  for index = 1, count do
    if values[index] ~= false then
      present = present + 1
    end
  end
  if present == 0 then
    return "missing", 0, 0, fallback_ttl, current_window
  end
  if present ~= count then
    return nil,0,0,fallback_ttl,base_window
  end
  local ttl = fallback_ttl
  if values[count+1] ~= false or values[count+2] ~= false then
    local first = parse_safe_unsigned_integer(values[count+1])
    local last = parse_safe_unsigned_integer(values[count+2])
    if first == nil or last == nil or first >= last then return nil,0,0,fallback_ttl,base_window end
    if now_ms < first or now_ms >= last then return "stale", 0, 0, fallback_ttl, current_window end
    current_window = parse_safe_unsigned_integer(values[1])
    ttl = last - now_ms
  end
  local stored_window = parse_safe_unsigned_integer(values[1])
  local accrued_hi, accrued_lo = parse_amount(values[2])
  if stored_window == nil or accrued_hi == nil or stored_window > current_window then
    return nil,0,0,fallback_ttl,base_window
  end
  if count == 3 and parse_safe_unsigned_integer(values[3]) == nil then
    return nil,0,0,fallback_ttl,base_window
  end
  if stored_window ~= current_window then
    return "stale", 0, 0, fallback_ttl, current_window
  end
  return "current", accrued_hi, accrued_lo, ttl, current_window
end

-- KEYS: day balance, month balance, pending reservations, reservation expiries, week balance
--       (all five carry the cost owner's Valkey Cluster hash tag).
-- ARGV: daily_limit, monthly_limit, server time override (tests only), amount,
--       lease_id, lease_ttl_ms, weekly_limit. An empty limit disables that window; an amount
--       of 0 is a request nothing could price, which is judged on accrued spend
--       alone and leaves nothing pending. "check" reads availability including
--       pending spend without modifying any key.
if not ((#KEYS == 4 and #ARGV == 6) or (#KEYS == 5 and (#ARGV == 7 or #ARGV == 8))) then
  return failure("invalid_arguments")
end

-- A limit is positive and below 10^12, which is as large as the API accepts: past
-- that, an amount could not be told from one that saturates.
local function parse_limit(raw)
  local hi, lo = parse_amount(raw)
  if hi == nil or hi >= UNIT or (hi == 0 and lo == 0) then
    return nil
  end
  return hi, lo
end

local weekly_limit = ARGV[7] or ""
local daily_hi, daily_lo, monthly_hi, monthly_lo, weekly_hi, weekly_lo
if ARGV[1] ~= "" then
  daily_hi, daily_lo = parse_limit(ARGV[1])
end
if ARGV[2] ~= "" then
  monthly_hi, monthly_lo = parse_limit(ARGV[2])
end
if weekly_limit ~= "" then weekly_hi, weekly_lo = parse_limit(weekly_limit) end
local override = parse_safe_unsigned_integer(ARGV[3])
local read_only = ARGV[4] == "check"
local amount_hi, amount_lo = 0, 0
if not read_only then
  amount_hi, amount_lo = parse_amount(ARGV[4])
end
local lease_ttl = parse_safe_unsigned_integer(ARGV[6])
local priced = amount_hi ~= nil and (amount_hi > 0 or amount_lo > 0)
if override == nil or amount_hi == nil or lease_ttl == nil
    or (priced and (lease_ttl < 1 or not valid_lease(ARGV[5])))
    or (ARGV[1] ~= "" and daily_hi == nil)
    or (ARGV[2] ~= "" and monthly_hi == nil)
    or (weekly_limit ~= "" and weekly_hi == nil) then
  return failure("invalid_arguments")
end

local now_ms = current_time(override)
if now_ms == nil then
  return failure("invalid_server_time")
end
local day_window, month_window, day_ttl, month_ttl = windows(now_ms)
local week_window = math.floor((math.floor(now_ms / DAY_MS) + 3) / 7)
local week_ttl = (week_window * 7 + 4) * DAY_MS - now_ms
if day_window == nil then
  return failure("invalid_server_time")
end

local daily_state, daily_spent_hi, daily_spent_lo = "disabled", 0, 0
if daily_hi ~= nil then
  daily_state, daily_spent_hi, daily_spent_lo, day_ttl, day_window = read_state(KEYS[1], {"window", "accrued"}, day_window, now_ms, day_ttl)
  if daily_state == nil then
    return {RESPONSE_VERSION, -1, "malformed_daily_cost_state", 0, day_window, month_window}
  end
end
local monthly_state, monthly_spent_hi, monthly_spent_lo = "disabled", 0, 0
if monthly_hi ~= nil then
  monthly_state, monthly_spent_hi, monthly_spent_lo, month_ttl, month_window = read_state(
    KEYS[2], {"window", "accrued", "unpriced"}, month_window, now_ms, month_ttl
  )
  if monthly_state == nil then
    return {RESPONSE_VERSION, -1, "malformed_monthly_cost_state", 0, day_window, month_window}
  end
end

local weekly_state, weekly_spent_hi, weekly_spent_lo = "disabled", 0, 0
if weekly_hi ~= nil then
  weekly_state, weekly_spent_hi, weekly_spent_lo, week_ttl, week_window = read_state(
    KEYS[5], {"window", "accrued"}, week_window, now_ms, week_ttl
  )
  if weekly_state == nil then
    return {RESPONSE_VERSION, -1, "malformed_weekly_cost_state", 0, day_window, month_window}
  end
end

-- Optional grants are immutable authority evidence, not caller input. They
-- add to an existing cap only during their original period and server-time span.
if ARGV[8] ~= nil then
  local ok, grants = pcall(cjson.decode, ARGV[8])
  if not ok or type(grants) ~= "table" or #grants > 24 then return failure("invalid_arguments") end
  for _, grant in ipairs(grants) do
    local hi, lo = parse_limit(grant.amount)
    if hi == nil or type(grant.window_id) ~= "number" or type(grant.starts_at) ~= "number"
        or type(grant.expires_at) ~= "number" or grant.expires_at <= grant.starts_at
        or (grant.window ~= "day" and grant.window ~= "week" and grant.window ~= "month") then
      return failure("invalid_arguments")
    end
    if grant.starts_at <= now_ms and now_ms < grant.expires_at then
      if grant.window == "day" and grant.window_id == day_window and daily_hi ~= nil then
        daily_hi, daily_lo = add_amount(daily_hi, daily_lo, hi, lo)
      elseif grant.window == "week" and grant.window_id == week_window and weekly_hi ~= nil then
        weekly_hi, weekly_lo = add_amount(weekly_hi, weekly_lo, hi, lo)
      elseif grant.window == "month" and grant.window_id == month_window and monthly_hi ~= nil then
        monthly_hi, monthly_lo = add_amount(monthly_hi, monthly_lo, hi, lo)
      end
    end
  end
end

if daily_hi ~= nil and compare_amount(daily_spent_hi, daily_spent_lo, daily_hi, daily_lo) >= 0 then
  return {RESPONSE_VERSION, 0, "daily_cost", day_ttl, day_window, month_window}
end
if monthly_hi ~= nil and compare_amount(monthly_spent_hi, monthly_spent_lo, monthly_hi, monthly_lo) >= 0 then
  return {RESPONSE_VERSION, 0, "monthly_cost", month_ttl, day_window, month_window}
end
if weekly_hi ~= nil and compare_amount(weekly_spent_hi, weekly_spent_lo, weekly_hi, weekly_lo) >= 0 then
  return {RESPONSE_VERSION, 0, "weekly_cost", week_ttl, day_window, month_window}
end

-- Only authoritative snapshots may initialize a window. Missing state can
-- also mean eviction or data loss, even while Valkey itself is reachable.
if daily_hi ~= nil and daily_state ~= "current" then
  return {RESPONSE_VERSION, -1, "uninitialized_daily_cost_state", 0, day_window, month_window}
end
if monthly_hi ~= nil and monthly_state ~= "current" then
  return {RESPONSE_VERSION, -1, "uninitialized_monthly_cost_state", 0, day_window, month_window}
end
if weekly_hi ~= nil and weekly_state ~= "current" then
  return {RESPONSE_VERSION, -1, "uninitialized_weekly_cost_state", 0, day_window, month_window}
end

if read_only then
  local held_hi, held_lo = pending_total(KEYS[3], KEYS[4], now_ms, true)
  local daily_total_hi, daily_total_lo = add_amount(daily_spent_hi, daily_spent_lo, held_hi, held_lo)
  if daily_hi ~= nil and compare_amount(daily_total_hi, daily_total_lo, daily_hi, daily_lo) >= 0 then
    return {RESPONSE_VERSION, 0, "daily_cost", math.min(day_ttl, PENDING_RETRY_MS), day_window, month_window}
  end
  local monthly_total_hi, monthly_total_lo = add_amount(monthly_spent_hi, monthly_spent_lo, held_hi, held_lo)
  if monthly_hi ~= nil and compare_amount(monthly_total_hi, monthly_total_lo, monthly_hi, monthly_lo) >= 0 then
    return {RESPONSE_VERSION, 0, "monthly_cost", math.min(month_ttl, PENDING_RETRY_MS), day_window, month_window}
  end
  local weekly_total_hi, weekly_total_lo = add_amount(weekly_spent_hi, weekly_spent_lo, held_hi, held_lo)
  if weekly_hi ~= nil and compare_amount(weekly_total_hi, weekly_total_lo, weekly_hi, weekly_lo) >= 0 then
    return {RESPONSE_VERSION, 0, "weekly_cost", math.min(week_ttl, PENDING_RETRY_MS), day_window, month_window}
  end
  return {RESPONSE_VERSION, 1, "ok", 0, day_window, month_window}
end

-- A priced request must fit beside what is already spent and what requests in
-- flight may still spend. Retiring expired leases is the only write a
-- rejection makes, and it changes no balance.
local held_hi, held_lo = 0, 0
if priced then
  held_hi, held_lo = pending_total(KEYS[3], KEYS[4], now_ms)
  -- Repeated deliveries never charge a lease twice. A larger estimate must
  -- fit beside the other leases before replacing this lease's amount. A lapsed
  -- lease outside the bounded sweep is retired and reserved afresh.
  local recorded = redis.call("ZSCORE", KEYS[4], ARGV[5])
  if recorded ~= false then
    local prior_hi, prior_lo = parse_amount(redis.call("HGET", KEYS[3], ARGV[5]))
    if prior_hi == nil then
      return failure("invalid_pending_amount")
    end
    if tonumber(recorded) > now_ms then
      if compare_amount(amount_hi, amount_lo, prior_hi, prior_lo) <= 0 then
        return {RESPONSE_VERSION, 1, "ok", 0, day_window, month_window}
      end
    else
      release_lease(KEYS[3], KEYS[4], ARGV[5])
    end
    held_hi, held_lo = sub_amount(held_hi, held_lo, prior_hi, prior_lo)
  end
  local pending_hi, pending_lo = held_hi, held_lo
  held_hi, held_lo = add_amount(pending_hi, pending_lo, amount_hi, amount_lo)

  -- How long to wait before a window that cannot hold the request may admit it,
  -- or nil when it can. Waiting only helps when in-flight reservations are what
  -- stands in the way: spent plus this request alone would still fit.
  local function wait_for(limit_hi, limit_lo, spent_hi, spent_lo, window_ttl)
    local total_hi, total_lo = add_amount(spent_hi, spent_lo, held_hi, held_lo)
    if compare_amount(total_hi, total_lo, limit_hi, limit_lo) <= 0 then
      return nil
    end
    local alone_hi, alone_lo = add_amount(spent_hi, spent_lo, amount_hi, amount_lo)
    if compare_amount(alone_hi, alone_lo, limit_hi, limit_lo) <= 0 then
      return math.min(window_ttl, PENDING_RETRY_MS)
    end
    return window_ttl
  end
  -- The request is admitted only when every refusing window clears, so the hint
  -- is the longest wait of them, reported under the window that sets it.
  local refused, retry = nil, 0
  if daily_hi ~= nil then
    local wait = wait_for(daily_hi, daily_lo, daily_spent_hi, daily_spent_lo, day_ttl)
    if wait ~= nil then
      refused, retry = "daily_cost", wait
    end
  end
  if monthly_hi ~= nil then
    local wait = wait_for(monthly_hi, monthly_lo, monthly_spent_hi, monthly_spent_lo, month_ttl)
    if wait ~= nil and (refused == nil or wait > retry) then
      refused, retry = "monthly_cost", wait
    end
  end
  if weekly_hi ~= nil then
    local wait = wait_for(weekly_hi, weekly_lo, weekly_spent_hi, weekly_spent_lo, week_ttl)
    if wait ~= nil and (refused == nil or wait > retry) then
      refused, retry = "weekly_cost", wait
    end
  end
  if refused ~= nil then
    -- The suffix says the budget is not spent but cannot hold this request beside
    -- what is, which is not the refusal an exhausted budget makes above.
    return {RESPONSE_VERSION, 0, refused .. "_estimate", retry, day_window, month_window}
  end
end

if daily_hi ~= nil and redis.call("PTTL", KEYS[1]) < 1 then
  redis.call("PEXPIRE", KEYS[1], day_ttl)
end
if monthly_hi ~= nil and redis.call("PTTL", KEYS[2]) < 1 then
  redis.call("PEXPIRE", KEYS[2], month_ttl)
end
if weekly_hi ~= nil and redis.call("PTTL", KEYS[5]) < 1 then
  redis.call("PEXPIRE", KEYS[5], week_ttl)
end
if priced then
  local expires_ms = now_ms + lease_ttl
  store_lease(KEYS[3], KEYS[4], ARGV[5], amount_hi, amount_lo, held_hi, held_lo, expires_ms, false)
  outlive(KEYS[3], KEYS[4], expires_ms, now_ms)
end

return {RESPONSE_VERSION, 1, "ok", 0, day_window, month_window}
