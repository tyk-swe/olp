-- Report live quota usage without turning malformed counters into idle zeros.
-- KEYS: stable rate hash, concurrency zset (same cluster hash tag).
local MAX_SAFE_INTEGER_TEXT = "9007199254740991"
local function unsigned(raw)
  if type(raw) ~= "string" or string.match(raw, "^%d+$") == nil then return nil end
  local normalized = string.gsub(raw, "^0+", "")
  if normalized == "" then normalized = "0" end
  if #normalized > #MAX_SAFE_INTEGER_TEXT
      or (#normalized == #MAX_SAFE_INTEGER_TEXT and normalized > MAX_SAFE_INTEGER_TEXT) then
    return nil
  end
  return tonumber(normalized)
end

local t = redis.call("TIME")
local w = math.floor(tonumber(t[1]) / 60)
local now = tonumber(t[1]) * 1000 + math.floor(tonumber(t[2]) / 1000)
local r = redis.call("HMGET", KEYS[1], "window", "rpm", "tpm")
local rpm, tpm = 0, 0
if r[1] ~= false or r[2] ~= false or r[3] ~= false then
  local window = unsigned(r[1])
  local requests, tokens = unsigned(r[2]), unsigned(r[3])
  if window == nil or requests == nil or tokens == nil or window > w then
    return redis.error_reply("invalid rate state")
  end
  if window == w then rpm, tpm = requests, tokens end
end
return {rpm, tpm, redis.call("ZCOUNT", KEYS[2], "(" .. now, "+inf")}
