local MAX_SAFE_INTEGER_TEXT = "9007199254740991"

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

if #KEYS ~= 3 or #ARGV ~= 4
    or type(ARGV[1]) ~= "string"
    or not is_safe_unsigned_integer(ARGV[2]) or tonumber(ARGV[2]) < 1
    or type(ARGV[3]) ~= "string" or #ARGV[3] < 1 or #ARGV[3] > 64
    or not is_safe_unsigned_integer(ARGV[4]) or tonumber(ARGV[4]) < 1 then
  return redis.error_reply("invalid request metadata append arguments")
end

local stream_type = redis.call("TYPE", KEYS[1]).ok
local counter_type = redis.call("TYPE", KEYS[2]).ok
local receipts_type = redis.call("TYPE", KEYS[3]).ok
if (stream_type ~= "none" and stream_type ~= "stream")
    or (counter_type ~= "none" and counter_type ~= "string")
    or (receipts_type ~= "none" and receipts_type ~= "hash") then
  return redis.error_reply("invalid request metadata append state")
end

if receipts_type == "hash" then
  local existing = redis.call("HGET", KEYS[3], ARGV[3])
  if existing ~= false then
    redis.call("HEXPIRE", KEYS[3], ARGV[4], "FIELDS", 1, ARGV[3])
    return {existing, 0}
  end
end

local counter = "0"
if counter_type == "string" then
  counter = redis.call("GET", KEYS[2])
end
if not is_safe_unsigned_integer(counter) then
  return redis.error_reply("invalid request metadata trim counter")
end
local stream_length = stream_type == "stream" and redis.call("XLEN", KEYS[1]) or 0
if stream_length >= tonumber(MAX_SAFE_INTEGER_TEXT)
    or tonumber(counter) > tonumber(MAX_SAFE_INTEGER_TEXT) - stream_length - 1 then
  return redis.error_reply("request metadata trim counter overflow")
end

local id = redis.call(
  "XADD",
  KEYS[1],
  "*",
  "event_id",
  ARGV[3],
  "event",
  ARGV[1]
)
local trimmed = redis.call("XTRIM", KEYS[1], "MAXLEN", "~", ARGV[2])
if trimmed > 0 then
  redis.call("INCRBY", KEYS[2], trimmed)
end
redis.call("HSET", KEYS[3], ARGV[3], id)
redis.call("HEXPIRE", KEYS[3], ARGV[4], "FIELDS", 1, ARGV[3])
return {id, trimmed}
