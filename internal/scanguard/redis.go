package scanguard

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/redis/go-redis/v9"
)

// RedisStore shares bans and counters across every gateway replica.
//
// A client's keys share a hash tag, so the ban script touches one slot and
// works on Redis Cluster too.
type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(c *redis.Client) *RedisStore { return &RedisStore{client: c} }

const redisPrefix = "nina:sg:"

func redisKey(client, kind string) string { return redisPrefix + "{" + client + "}:" + kind }

// redisBan is the stored form of a Ban; times are Unix milliseconds so the
// script can write them.
type redisBan struct {
	Client  string `json:"client"`
	Reason  string `json:"reason"`
	Path    string `json:"path"`
	Strikes int    `json:"strikes"`
	Since   int64  `json:"since"`
	Until   int64  `json:"until"`
}

func (b redisBan) ban() Ban {
	return Ban{
		Client: b.Client, Reason: b.Reason, Path: b.Path, Strikes: b.Strikes,
		Since: time.UnixMilli(b.Since), Until: time.UnixMilli(b.Until),
	}
}

var missScript = redis.NewScript(`
local n = redis.call("INCR", KEYS[1])
if n == 1 then redis.call("PEXPIRE", KEYS[1], ARGV[1]) end
return n
`)

// banScript records the strike, sizes the ban from it and stores the ban, all
// in one step so two replicas banning the same client cannot both count it as
// a first offence.
var banScript = redis.NewScript(`
local base, max, now = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])
local strikes = redis.call("INCR", KEYS[2])
local len = base
for i = 2, strikes do
  if len >= max then break end
  len = len * 2
end
if len > max then len = max end
redis.call("PEXPIRE", KEYS[2], len + max)
local ban = cjson.encode({client = ARGV[4], reason = ARGV[5], path = ARGV[6],
  strikes = strikes, since = now, ["until"] = now + len})
redis.call("SET", KEYS[1], ban, "PX", len)
redis.call("DEL", KEYS[3])
return ban
`)

func (r *RedisStore) BannedFor(ctx context.Context, key string) (time.Duration, error) {
	d, err := r.client.PTTL(ctx, redisKey(key, "ban")).Result()
	if err != nil || d < 0 {
		return 0, err
	}
	return d, nil
}

func (r *RedisStore) Miss(ctx context.Context, key string, window time.Duration) (int, error) {
	n, err := missScript.Run(ctx, r.client, []string{redisKey(key, "miss")}, window.Milliseconds()).Int()
	return n, err
}

func (r *RedisStore) Ban(ctx context.Context, b Ban, base, max time.Duration) (Ban, error) {
	raw, err := banScript.Run(ctx, r.client,
		[]string{redisKey(b.Client, "ban"), redisKey(b.Client, "strikes"), redisKey(b.Client, "miss")},
		base.Milliseconds(), max.Milliseconds(), time.Now().UnixMilli(), b.Client, b.Reason, b.Path,
	).Text()
	if err != nil {
		return b, err
	}
	var rb redisBan
	if err := json.Unmarshal([]byte(raw), &rb); err != nil {
		return b, err
	}
	return rb.ban(), nil
}

func (r *RedisStore) Unban(ctx context.Context, key string) (bool, error) {
	n, err := r.client.Del(ctx, redisKey(key, "ban")).Result()
	if err != nil {
		return false, err
	}
	if err := r.client.Del(ctx, redisKey(key, "strikes"), redisKey(key, "miss")).Err(); err != nil {
		return false, err
	}
	return n > 0, nil
}

func (r *RedisStore) List(ctx context.Context) ([]Ban, error) {
	var keys []string
	iter := r.client.Scan(ctx, 0, redisPrefix+"*:ban", 500).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	out := []Ban{}
	now := time.Now().UnixMilli()
	for _, k := range keys {
		raw, err := r.client.Get(ctx, k).Result()
		if err == redis.Nil {
			continue // expired between the scan and the read
		}
		if err != nil {
			return nil, err
		}
		var rb redisBan
		if json.Unmarshal([]byte(raw), &rb) != nil || rb.Until <= now {
			continue
		}
		out = append(out, rb.ban())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Since.After(out[j].Since) })
	return out, nil
}
