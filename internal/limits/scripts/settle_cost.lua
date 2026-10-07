-- Settle one request's pending cost reservation: replace its amount with what
-- the request actually cost, or drop it when that is zero. A lease that is not
-- there (already accrued, expired, or never recorded) is left alone and never
-- recreated, which is what keeps a late or repeated settlement from counting
-- spend twice.
-- Response v1: {version, status, detail, settled, 0}
-- status: 1 = ok (settled: 1 when a lease was found), -1 = invalid arguments.
-- KEYS: pending reservations, reservation expiries (same cost owner hash tag).
-- ARGV: lease_id, actual amount ("0" releases), ttl_ms: the most the lease may
--       outlive this call while PostgreSQL prices the request. It only ever
--       shortens the lease granted at admission.

local RESPONSE_VERSION = 1
local MAX_SAFE_INTEGER_TEXT = "9007199254740991"

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

if #KEYS ~= 2 or #ARGV ~= 3 then
  return failure("invalid_arguments")
end

local lease_id = ARGV[1]
local amount_hi, amount_lo = parse_amount(ARGV[2])
local ttl = parse_safe_unsigned_integer(ARGV[3])
local release = amount_hi ~= nil and amount_hi == 0 and amount_lo == 0
if not valid_lease(lease_id) or amount_hi == nil or ttl == nil or (not release and ttl < 1) then
  return failure("invalid_arguments")
end
local now_ms = current_time(0)
if now_ms == nil then
  return failure("invalid_server_time")
end

local sum_hi, sum_lo = pending_total(KEYS[1], KEYS[2], now_ms)
local held = redis.call("HGET", KEYS[1], lease_id)
if held == false then
  return {RESPONSE_VERSION, 1, "ok", 0, 0}
end
-- A lease past its expiry no longer counts, so settling must not revive it even
-- when the sweep above stopped before reaching it.
local expiry = redis.call("ZSCORE", KEYS[2], lease_id)
if expiry ~= false and tonumber(expiry) <= now_ms then
  remove_lease(KEYS[1], KEYS[2], lease_id, held, sum_hi, sum_lo)
  return {RESPONSE_VERSION, 1, "ok", 0, 0}
end
if release then
  remove_lease(KEYS[1], KEYS[2], lease_id, held, sum_hi, sum_lo)
else
  local held_hi, held_lo = parse_amount(held)
  if held_hi ~= nil then
    sum_hi, sum_lo = sub_amount(sum_hi, sum_lo, held_hi, held_lo)
  end
  sum_hi, sum_lo = add_amount(sum_hi, sum_lo, amount_hi, amount_lo)
  store_lease(KEYS[1], KEYS[2], lease_id, amount_hi, amount_lo, sum_hi, sum_lo, now_ms + ttl, true)
end
return {RESPONSE_VERSION, 1, "ok", 1, 0}
