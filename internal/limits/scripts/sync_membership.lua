local MAX_SAFE_INTEGER_TEXT = "9007199254740991"

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

if #KEYS ~= 1 or #ARGV < 2 then
  return redis.error_reply("invalid membership arguments")
end
local generation = parse_safe_unsigned_integer(ARGV[1])
local ceiling = parse_safe_unsigned_integer(ARGV[2])
if generation == nil or ceiling == nil or (ceiling == 0) ~= (#ARGV == 2) then
  return redis.error_reply("invalid membership arguments")
end

local active = {}
for index = 3, #ARGV do
  local member = ARGV[index]
  if type(member) ~= "string" or #member < 1 or #member > 128 or string.sub(member, 1, 1) == "@" then
    return redis.error_reply("invalid membership arguments")
  end
  active[member] = true
end

local kind = redis.call("TYPE", KEYS[1]).ok
if kind ~= "none" and kind ~= "hash" then
  return redis.error_reply("invalid membership state")
end
local stored = redis.call("HGET", KEYS[1], "@generation")
if stored ~= false then
  local installed = parse_safe_unsigned_integer(stored)
  if installed == nil then
    return redis.error_reply("invalid membership state")
  end
  if generation <= installed then
    return 0
  end
end

for _, member in ipairs(redis.call("HKEYS", KEYS[1])) do
  if string.sub(member, 1, 1) ~= "@" and not active[member] then
    redis.call("HSET", KEYS[1], member, "-" .. ARGV[1])
  end
end
for index = 3, #ARGV do
  redis.call("HSET", KEYS[1], ARGV[index], ARGV[1])
end
redis.call("HSET", KEYS[1], "@generation", ARGV[1], "@ceiling", ARGV[2])
return 1
