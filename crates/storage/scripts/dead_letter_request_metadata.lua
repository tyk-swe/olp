-- Retain a poison event before acknowledging and deleting its source entry.
-- KEYS: source stream, dead-letter stream
-- ARGV: consumer group, source ID, payload, dead-letter max entries

if #KEYS ~= 2 or #ARGV ~= 4
    or type(ARGV[1]) ~= "string" or #ARGV[1] < 1
    or type(ARGV[2]) ~= "string" or #ARGV[2] < 1
    or type(ARGV[3]) ~= "string"
    or type(ARGV[4]) ~= "string"
    or string.match(ARGV[4], "^%d+$") == nil
    or tonumber(ARGV[4]) == nil or tonumber(ARGV[4]) < 1 then
  return redis.error_reply("invalid request metadata dead-letter arguments")
end

local source_type = redis.call("TYPE", KEYS[1]).ok
local dead_letter_type = redis.call("TYPE", KEYS[2]).ok
if (source_type ~= "none" and source_type ~= "stream")
    or (dead_letter_type ~= "none" and dead_letter_type ~= "stream") then
  return redis.error_reply("invalid request metadata dead-letter state")
end
if source_type == "none"
    or #redis.call("XRANGE", KEYS[1], ARGV[2], ARGV[2], "COUNT", 1) == 0 then
  return ""
end
local pending = redis.call(
  "XPENDING",
  KEYS[1],
  ARGV[1],
  ARGV[2],
  ARGV[2],
  1
)
if #pending ~= 1 or pending[1][1] ~= ARGV[2] then
  return redis.error_reply("request metadata source entry was not pending")
end

local dead_letter_id = redis.call(
  "XADD",
  KEYS[2],
  "MAXLEN",
  "~",
  ARGV[4],
  "*",
  "source_id",
  ARGV[2],
  "event",
  ARGV[3]
)
local acknowledged = redis.call("XACK", KEYS[1], ARGV[1], ARGV[2])
if acknowledged ~= 1 then
  return redis.error_reply("request metadata source entry was not pending")
end
redis.call("XDEL", KEYS[1], ARGV[2])
return dead_letter_id
