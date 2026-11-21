-- Record shared quota membership at one release generation. An entry is the
-- generation that made the member current, or its negation once the member
-- left, so a replica holding an older snapshot cannot reserve in this group
-- after the move. Entries are never deleted here, which keeps the group
-- accountable while leases taken under it can still settle.
-- KEYS: the group's members hash, sharing the group's hash tag.
-- ARGV: generation, then member and state pairs, state 1 for a current
--       member and 0 for one that left.
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

if #KEYS ~= 1 or #ARGV < 3 or #ARGV % 2 ~= 1 then
  return redis.error_reply("invalid membership arguments")
end
local generation = parse_safe_unsigned_integer(ARGV[1])
if generation == nil then
  return redis.error_reply("invalid membership arguments")
end
local kind = redis.call("TYPE", KEYS[1]).ok
if kind ~= "none" and kind ~= "hash" then
  return redis.error_reply("invalid membership state")
end

local applied = 0
for index = 2, #ARGV, 2 do
  local member = ARGV[index]
  local state = ARGV[index + 1]
  if type(member) ~= "string" or #member < 1 or #member > 128 or (state ~= "0" and state ~= "1") then
    return redis.error_reply("invalid membership arguments")
  end
  local stored = redis.call("HGET", KEYS[1], member)
  local stored_generation = -1
  if stored ~= false then
    local departed = string.sub(stored, 1, 1) == "-"
    stored_generation = parse_safe_unsigned_integer(departed and string.sub(stored, 2) or stored)
    if stored_generation == nil then
      return redis.error_reply("invalid membership state")
    end
  end
  if generation >= stored_generation then
    redis.call("HSET", KEYS[1], member, state == "1" and generation or "-" .. generation)
    applied = applied + 1
  end
end
return applied
