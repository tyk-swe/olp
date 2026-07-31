local max_length = tonumber(ARGV[1])

if redis.call("XLEN", KEYS[1]) >= max_length then
  return 0
end

redis.call("XADD", KEYS[1], "*", "event", ARGV[2])
return 1
