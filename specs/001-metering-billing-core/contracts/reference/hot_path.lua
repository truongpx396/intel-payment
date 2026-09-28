#!lua name=intelpay
--[[
Reference hot path for intel-payment — a Redis Functions library (Redis >= 7.0).
Normative contract: ../hot-path-consistency.md. Verified by scripts/verify-hot-path.sh
against Redis in CLUSTER mode.

Every function touches only keys sharing ONE hash tag, the shard tag {s<n>}:

  meta:{s<n>}                                shard state: gen, frozen
  acct:{s<n>}:<tag>                          hot account: gen, seq, b:<pool>..., blocked
  idem:{s<n>}:<tag>:<op>:<idem_key>          "<fingerprint>|<seq>", PX HotIdemTTL
  ctr:{s<n>}:<tag>:<limit>:<subject>:<bkt>   window counter, PEXPIRE ... NX
  billing:outbox:{s<n>}                      the intent stream

Every successful mutation increments `seq` by exactly one and XADDs exactly one intent.

Precision: Redis Lua numbers are IEEE doubles. Balances and amounts on the hot tier are exact
within +/-2^53 (9,007,199,254,740,991 units); the service refuses any operation whose amount
exceeds MaxOperationAmount (default 2^50), which keeps every balance far inside that range.
Numbers are formatted with %d so no value is ever rendered in exponent notation.
]]

local function fmt(n) return string.format('%d', n) end

-- HGET/GET return false for a missing value; treat it as zero.
local function num(v) return tonumber(v or '0') or 0 end

local function split(s)
  local out = {}
  if s == nil or s == '' then return out end
  for part in string.gmatch(s, '([^,]+)') do table.insert(out, part) end
  return out
end

-- The shard is live and the account was built in the shard's current generation.
-- Returns the generation, or nil plus the status the caller must act on.
local function live(meta, acct)
  local m = redis.call('HMGET', meta, 'gen', 'frozen')
  if not m[1] then return nil, 'COLD_SHARD' end
  if m[2] == '1' then return nil, 'FROZEN' end
  if acct and redis.call('HGET', acct, 'gen') ~= m[1] then return nil, 'COLD_SCOPE' end
  return m[1]
end

-- Same key + same fingerprint is a replay; same key + different fingerprint is a conflict.
local function guard(key, fp)
  local prior = redis.call('GET', key)
  if not prior then return nil end
  local pfp, pseq = string.match(prior, '^(.*)|(%d+)$')
  if pfp == fp then return {'REPLAY', tonumber(pseq)} end
  return {'IDEM_CONFLICT'}
end

-- Backpressure is refused loudly; an intent is never dropped and never trimmed here.
local function full(stream, maxlen)
  return redis.call('XLEN', stream) >= tonumber(maxlen)
end

local function total(acct)
  local h = redis.call('HGETALL', acct)
  local t = 0
  for i = 1, #h, 2 do
    if string.sub(h[i], 1, 2) == 'b:' then t = t + tonumber(h[i + 1]) end
  end
  return t
end

-- The common tail of every mutation: next seq, the guard, exactly one intent.
local function commit(c, op, draws, extra)
  local seq = redis.call('HINCRBY', c.acct, 'seq', 1)
  redis.call('SET', c.idem, c.fp .. '|' .. fmt(seq), 'PX', c.ttl)
  local f = {'v', '1', 'op', op, 'realm', c.realm, 'kind', c.kind, 'id', c.id,
             'gen', c.gen, 'seq', fmt(seq), 'idem', c.idem_key, 'fp', c.fp, 'draws', draws}
  for i = 1, #extra do table.insert(f, extra[i]) end
  table.insert(f, 'p'); table.insert(f, c.payload)
  redis.call('XADD', c.stream, '*', unpack(f))
  return seq
end

-- Shared preamble: liveness, idempotency, backpressure. Returns (ctx) or (nil, reply).
local function begin(keys, fp, ttl, maxlen, realm, kind, id, idem_key, payload)
  local gen, st = live(keys[1], keys[2])
  if not gen then return nil, {st} end
  local g = guard(keys[3], fp)
  if g then return nil, g end
  if full(keys[4], maxlen) then return nil, {'BACKPRESSURE'} end
  return {meta = keys[1], acct = keys[2], idem = keys[3], stream = keys[4], gen = gen,
          fp = fp, ttl = ttl, realm = realm, kind = kind, id = id,
          idem_key = idem_key, payload = payload}
end

--[[ ip_admit (read-only)
KEYS: meta, acct, counter...
Returns {'OK', seq, blocked, {pool, balance, ...}, {counter values...}} or {status}. ]]
local function admit(keys, _)
  local gen, st = live(keys[1], keys[2])
  if not gen then return {st} end
  local h = redis.call('HGETALL', keys[2])
  local pools, seq, blocked = {}, 0, 0
  for i = 1, #h, 2 do
    if string.sub(h[i], 1, 2) == 'b:' then
      table.insert(pools, string.sub(h[i], 3)); table.insert(pools, tonumber(h[i + 1]))
    elseif h[i] == 'seq' then seq = tonumber(h[i + 1])
    elseif h[i] == 'blocked' then blocked = 1 end
  end
  local ctr = {}
  for i = 3, #keys do table.insert(ctr, num(redis.call('GET', keys[i]))) end
  return {'OK', seq, blocked, pools, ctr}
end

--[[ ip_debit — settle usage.
KEYS: meta, acct, idem, stream, counter...
ARGV: 1 amount (>= 0)  2 fp  3 idem_ttl_ms  4 stream_maxlen
      5 eligible pools, priority order, comma-separated; the LAST absorbs any overshoot
      6 realm  7 kind  8 id  9 idem_key  10 payload (JSON)
      11 low-water watch "<pool>:<threshold>" or ""
      12.. (increment, ttl_ms) per counter key
Returns {'APPLIED', seq, total} | {'REPLAY', seq} | {status}. ]]
local function debit(keys, args)
  local amount = tonumber(args[1])
  if not amount or amount < 0 then return redis.error_reply('ERR amount must be >= 0') end
  local pools = split(args[5])
  if #pools == 0 then return redis.error_reply('ERR no eligible pool') end
  local c, reply = begin(keys, args[2], args[3], args[4], args[6], args[7], args[8], args[9], args[10])
  if not c then return reply end

  local wpool, wthr, wbefore
  if args[11] ~= '' then
    local p, t = string.match(args[11], '^([^:]+):(-?%d+)$')
    wpool, wthr = p, tonumber(t)
    wbefore = num(redis.call('HGET', c.acct, 'b:' .. wpool))
  end

  local remaining, draws = amount, {}
  for i, pool in ipairs(pools) do
    if remaining <= 0 then break end
    local take = remaining
    if i < #pools then
      take = math.min(remaining, math.max(num(redis.call('HGET', c.acct, 'b:' .. pool)), 0))
    end
    if take > 0 then
      redis.call('HINCRBY', c.acct, 'b:' .. pool, -take)
      table.insert(draws, pool .. ':' .. fmt(-take))
      remaining = remaining - take
    end
  end

  local extra = {}
  if wpool and wbefore >= wthr and num(redis.call('HGET', c.acct, 'b:' .. wpool)) < wthr then
    extra = {'lw', args[11]}
  end

  for i = 5, #keys do
    local a = 12 + (i - 5) * 2
    redis.call('INCRBY', keys[i], args[a])
    redis.call('PEXPIRE', keys[i], args[a + 1], 'NX')
  end

  local seq = commit(c, 'usage', table.concat(draws, ';'), extra)
  return {'APPLIED', seq, total(c.acct)}
end

--[[ ip_grant — add or remove credits in one pool (payments, refunds, chargebacks, admin).
KEYS: meta, acct, idem, stream
ARGV: 1 pool  2 amount (signed)  3 fp  4 idem_ttl_ms  5 stream_maxlen
      6 negative-balance policy: allow_debt | clamp_to_zero | block_and_flag
      7 realm  8 kind  9 id  10 idem_key  11 payload
A negative grant under clamp_to_zero / block_and_flag may not take the pool below
min(balance_before, 0); the shortfall is reported as a writeoff, which the writer books as
its own ledger row so balance == SUM(ledger) still holds.
Returns {'APPLIED', seq, total, writeoff} | {'REPLAY', seq} | {status}. ]]
local function grant(keys, args)
  local amount = tonumber(args[2])
  if not amount then return redis.error_reply('ERR amount') end
  local c, reply = begin(keys, args[3], args[4], args[5], args[7], args[8], args[9], args[10], args[11])
  if not c then return reply end
  local pool, policy = args[1], args[6]

  local before = num(redis.call('HGET', c.acct, 'b:' .. pool))
  local after, wo = before + amount, 0
  if amount < 0 and policy ~= 'allow_debt' then
    local floor = math.min(before, 0)
    if after < floor then wo = floor - after; after = floor end
    if wo > 0 and policy == 'block_and_flag' then redis.call('HSET', c.acct, 'blocked', '1') end
  end
  local delta = after - before
  if delta ~= 0 then redis.call('HINCRBY', c.acct, 'b:' .. pool, delta) end

  local extra = {}
  if wo > 0 then extra = {'wo', fmt(wo)} end
  local seq = commit(c, 'grant', pool .. ':' .. fmt(delta), extra)
  return {'APPLIED', seq, total(c.acct), wo}
end

--[[ ip_transfer_out — phase 1 of a transfer, on the SOURCE shard only.
KEYS: meta, acct, idem, stream
ARGV: 1 pool  2 amount (> 0)  3 fp  4 idem_ttl_ms  5 stream_maxlen
      6 realm  7 kind  8 id  9 idem_key  10 payload (carries the destination)
Checks the HOT balance — the most current figure — and refuses rather than overdrawing.
Returns {'APPLIED', seq, total} | {'INSUFFICIENT', balance} | {'REPLAY', seq} | {status}. ]]
local function transfer_out(keys, args)
  local amount = tonumber(args[2])
  if not amount or amount <= 0 then return redis.error_reply('ERR amount must be > 0') end
  local c, reply = begin(keys, args[3], args[4], args[5], args[6], args[7], args[8], args[9], args[10])
  if not c then return reply end
  local pool = args[1]
  local bal = num(redis.call('HGET', c.acct, 'b:' .. pool))
  if bal < amount then return {'INSUFFICIENT', bal} end
  redis.call('HINCRBY', c.acct, 'b:' .. pool, -amount)
  local seq = commit(c, 'transfer_out', pool .. ':' .. fmt(-amount), {})
  return {'APPLIED', seq, total(c.acct)}
end

--[[ ip_apply_delta — writer-issued hot mutations: transfer_in, correction, expiry.
KEYS: meta, acct, idem, stream
ARGV: 1 op  2 pool  3 delta (signed)  4 fp  5 idem_ttl_ms  6 stream_maxlen
      7 realm  8 kind  9 id  10 idem_key  11 payload
      12 "1" = cap a negative delta at the pool's available balance (expiry pre-application)
Returns {'APPLIED', seq, total, applied_delta} | {'REPLAY', seq} | {status}. ]]
local function apply_delta(keys, args)
  local op = args[1]
  if op ~= 'transfer_in' and op ~= 'correction' and op ~= 'expiry' then
    return redis.error_reply('ERR op must be transfer_in|correction|expiry')
  end
  local delta = tonumber(args[3])
  if not delta then return redis.error_reply('ERR delta') end
  local c, reply = begin(keys, args[4], args[5], args[6], args[7], args[8], args[9], args[10], args[11])
  if not c then return reply end
  local pool = args[2]
  if args[12] == '1' and delta < 0 then
    local bal = num(redis.call('HGET', c.acct, 'b:' .. pool))
    delta = -math.min(-delta, math.max(bal, 0))
  end
  if delta ~= 0 then redis.call('HINCRBY', c.acct, 'b:' .. pool, delta) end
  local seq = commit(c, op, pool .. ':' .. fmt(delta), {})
  return {'APPLIED', seq, total(c.acct), delta}
end

--[[ ip_rehydrate — install booked + open suspense for a stale or new account.
KEYS: meta, acct
ARGV: 1 shard gen as read from Postgres  2 applied_seq  3 blocked ("1"/"0")
      4.. (pool, balance) pairs
Installs ONLY while the account is stale, so concurrent callers converge on one rebuild.
Returns {'OK'} | {'ALREADY'} | {'STALE_GEN', gen} | {status}. ]]
local function rehydrate(keys, args)
  local m = redis.call('HMGET', keys[1], 'gen', 'frozen')
  if not m[1] then return {'COLD_SHARD'} end
  if m[2] == '1' then return {'FROZEN'} end
  if m[1] ~= args[1] then return {'STALE_GEN', tonumber(m[1])} end
  if redis.call('HGET', keys[2], 'gen') == m[1] then return {'ALREADY'} end
  redis.call('DEL', keys[2])
  local h = {'gen', m[1], 'seq', args[2]}
  if args[3] == '1' then table.insert(h, 'blocked'); table.insert(h, '1') end
  for i = 4, #args, 2 do
    table.insert(h, 'b:' .. args[i]); table.insert(h, args[i + 1])
  end
  redis.call('HSET', keys[2], unpack(h))
  return {'OK'}
end

--[[ ip_heal — reconcile's correction of a defect, applied to the HOT side only.
KEYS: meta, acct
ARGV: 1 the seq observed when drift was measured  2.. (pool, target balance) pairs
Compare-and-set on seq: a charge that landed since the measurement is never overwritten.
Returns {'OK'} | {'RACE'} | {status}. ]]
local function heal(keys, args)
  local gen, st = live(keys[1], keys[2])
  if not gen then return {st} end
  if redis.call('HGET', keys[2], 'seq') ~= args[1] then return {'RACE'} end
  for i = 2, #args, 2 do redis.call('HSET', keys[2], 'b:' .. args[i], args[i + 1]) end
  return {'OK'}
end

--[[ ip_shard — shard lifecycle, driven from hot_shards in Postgres.
KEYS: meta
ARGV: open <gen>     install only if absent (first boot, or total loss)
      bump <gen>     raise the generation (every account in the shard becomes stale)
      freeze <why> | unfreeze ]]
local function shard(keys, args)
  local action, cur = args[1], redis.call('HGET', keys[1], 'gen')
  if action == 'open' then
    if cur then return {'OK', tonumber(cur)} end
    redis.call('HSET', keys[1], 'gen', args[2], 'frozen', '0')
    return {'OK', tonumber(args[2])}
  elseif action == 'bump' then
    if cur and tonumber(cur) >= tonumber(args[2]) then return {'OK', tonumber(cur)} end
    redis.call('HSET', keys[1], 'gen', args[2])
    return {'OK', tonumber(args[2])}
  elseif action == 'freeze' then
    redis.call('HSET', keys[1], 'frozen', '1', 'frozen_reason', args[2] or '')
    return {'OK'}
  elseif action == 'unfreeze' then
    redis.call('HSET', keys[1], 'frozen', '0')
    return {'OK'}
  end
  return redis.error_reply('ERR unknown shard action')
end

redis.register_function{function_name = 'ip_admit', callback = admit, flags = {'no-writes'}}
redis.register_function('ip_debit', debit)
redis.register_function('ip_grant', grant)
redis.register_function('ip_transfer_out', transfer_out)
redis.register_function('ip_apply_delta', apply_delta)
redis.register_function('ip_rehydrate', rehydrate)
redis.register_function('ip_heal', heal)
redis.register_function('ip_shard', shard)
