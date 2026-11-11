local page = redis.call(
  "XAUTOCLAIM",
  KEYS[1],
  ARGV[1],
  ARGV[2],
  ARGV[3],
  ARGV[4],
  "COUNT",
  ARGV[5]
)

if #page == 3 then
  for _, deleted_id in ipairs(page[3]) do
    redis.call("XADD", KEYS[1], "*", "deleted_pending_id", deleted_id)
  end
end

return page
