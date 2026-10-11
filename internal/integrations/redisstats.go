package integrations

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"time"
)

// RedisStats is what a Redis says about itself.
type RedisStats struct {
	Kind        string  `json:"kind"`
	CollectedMs float64 `json:"collected_ms"`
	TookMs      float64 `json:"took_ms"`
	Remote      bool    `json:"remote"`

	Version string `json:"version"`
	Mode    string `json:"mode"`
	UptimeS int64  `json:"uptime_s"`

	Memory  RedisMemory  `json:"memory"`
	Clients RedisClients `json:"clients"`

	OpsPerSec     int64         `json:"ops_per_sec"`
	TotalCommands int64         `json:"total_commands"`
	Keyspace      RedisKeyspace `json:"keyspace"`
	Databases     []RedisDB     `json:"databases"`
	// Commands is where the server's time went, by command, since it
	// started. Unlike live capture this has real durations.
	Commands    []RedisCommand   `json:"commands"`
	SlowLog     RedisSlowLog     `json:"slow_log"`
	BiggestKeys RedisBiggestKeys `json:"biggest_keys"`
	Incomplete  []SectionError   `json:"incomplete"`
}

type RedisMemory struct {
	UsedBytes int64 `json:"used_bytes"`
	RSSBytes  int64 `json:"rss_bytes"`
	PeakBytes int64 `json:"peak_bytes"`
	// MaxBytes is the maxmemory setting; 0 means no limit.
	MaxBytes      int64   `json:"max_bytes"`
	Policy        string  `json:"policy"`
	Fragmentation float64 `json:"fragmentation"`
}

type RedisClients struct {
	Connected int64 `json:"connected"`
	Blocked   int64 `json:"blocked"`
	Max       int64 `json:"max"`
}

type RedisKeyspace struct {
	Keys   int64 `json:"keys"`
	Hits   int64 `json:"hits"`
	Misses int64 `json:"misses"`
	// HitRate is hits over lookups, 0 to 1; absent before any lookup.
	HitRate *float64 `json:"hit_rate"`
	Expired int64    `json:"expired"`
	Evicted int64    `json:"evicted"`
}

type RedisDB struct {
	DB       int   `json:"db"`
	Keys     int64 `json:"keys"`
	Expiring int64 `json:"expiring"`
	AvgTTLMs int64 `json:"avg_ttl_ms"`
}

type RedisCommand struct {
	Command string  `json:"command"`
	Calls   int64   `json:"calls"`
	TotalMs float64 `json:"total_ms"`
	AvgMs   float64 `json:"avg_ms"`
}

type RedisSlowLog struct {
	// ThresholdMs is how slow a command must be to be logged; absent when
	// the server wouldn't say.
	ThresholdMs *float64       `json:"threshold_ms"`
	Entries     []RedisSlowCmd `json:"entries"`
}

type RedisSlowCmd struct {
	ID         int64   `json:"id"`
	AtMs       float64 `json:"at_ms"`
	DurationMs float64 `json:"duration_ms"`
	Command    string  `json:"command"`
	Client     string  `json:"client,omitempty"`
	ClientName string  `json:"client_name,omitempty"`
}

type RedisBiggestKeys struct {
	DB int `json:"db"`
	// Sampled is how many keys were looked at, of Total in the database.
	// Complete says that was all of them.
	Sampled  int64      `json:"sampled"`
	Total    int64      `json:"total"`
	Complete bool       `json:"complete"`
	Note     string     `json:"note"`
	Keys     []RedisKey `json:"keys"`
}

type RedisKey struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Bytes int64  `json:"bytes"`
}

// SectionError says one part of the stats could not be read, and why. The
// rest still stands.
type SectionError struct {
	Section string `json:"section"`
	Message string `json:"message"`
}

const (
	// The key sample stops at whichever comes first. SCAN is the only
	// way to walk a keyspace without stopping the server; KEYS would
	// block every other client for the length of the walk.
	sampleKeys   = 1000
	sampleBudget = time.Second
	scanCount    = "200"
	topKeys      = 10
	slowEntries  = 10
	topCommands  = 10
)

func redisStats(ctx context.Context, c *respConn, t *target) (*RedisStats, error) {
	began := time.Now()
	out := &RedisStats{Kind: KindRedis, Remote: t.remote(),
		Databases: []RedisDB{}, Commands: []RedisCommand{}, Incomplete: []SectionError{}}
	out.SlowLog.Entries = []RedisSlowCmd{}
	out.BiggestKeys.Keys = []RedisKey{}

	// INFO is the one part nothing else stands without.
	v, err := c.do("INFO")
	if err != nil {
		return nil, err
	}
	text, ok := v.(string)
	if !ok {
		return nil, unexpected("INFO", v)
	}
	info := parseInfo(text)
	out.Version = firstOf(info, "redis_version", "valkey_version", "dragonfly_version")
	out.Mode = info["redis_mode"]
	out.UptimeS = infoInt(info, "uptime_in_seconds")
	out.Memory = RedisMemory{
		UsedBytes: infoInt(info, "used_memory"), RSSBytes: infoInt(info, "used_memory_rss"),
		PeakBytes: infoInt(info, "used_memory_peak"), MaxBytes: infoInt(info, "maxmemory"),
		Policy: info["maxmemory_policy"], Fragmentation: infoFloat(info, "mem_fragmentation_ratio"),
	}
	out.Clients = RedisClients{Connected: infoInt(info, "connected_clients"),
		Blocked: infoInt(info, "blocked_clients"), Max: infoInt(info, "maxclients")}
	out.OpsPerSec = infoInt(info, "instantaneous_ops_per_sec")
	out.TotalCommands = infoInt(info, "total_commands_processed")
	out.Keyspace = RedisKeyspace{Hits: infoInt(info, "keyspace_hits"), Misses: infoInt(info, "keyspace_misses"),
		Expired: infoInt(info, "expired_keys"), Evicted: infoInt(info, "evicted_keys")}
	if lookups := out.Keyspace.Hits + out.Keyspace.Misses; lookups > 0 {
		rate := round3(float64(out.Keyspace.Hits) / float64(lookups))
		out.Keyspace.HitRate = &rate
	}
	out.Databases = redisDatabases(info)
	for _, db := range out.Databases {
		out.Keyspace.Keys += db.Keys
	}

	section := func(name string, err error) {
		if err != nil {
			out.Incomplete = append(out.Incomplete, SectionError{Section: name, Message: explain(err, t).Message})
		}
	}
	section("commands", redisCommands(c, out))
	section("slow_log", redisSlowLog(c, out))
	if ctx.Err() == nil {
		section("biggest_keys", redisBiggestKeys(c, t, out))
	}

	out.CollectedMs = ms(time.Now())
	out.TookMs = float64(time.Since(began).Microseconds()) / 1000
	return out, nil
}

func firstOf(info map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := info[k]; v != "" {
			return v
		}
	}
	return ""
}

// redisDatabases reads INFO's keyspace lines: db0:keys=12,expires=3,avg_ttl=0.
func redisDatabases(info map[string]string) []RedisDB {
	out := []RedisDB{}
	for k, v := range info {
		num, ok := strings.CutPrefix(k, "db")
		if !ok {
			continue
		}
		n, err := strconv.Atoi(num)
		if err != nil {
			continue
		}
		db := RedisDB{DB: n}
		for _, field := range strings.Split(v, ",") {
			name, value, _ := strings.Cut(field, "=")
			x, _ := strconv.ParseInt(value, 10, 64)
			switch name {
			case "keys":
				db.Keys = x
			case "expires":
				db.Expiring = x
			case "avg_ttl":
				db.AvgTTLMs = x
			}
		}
		out = append(out, db)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DB < out[j].DB })
	return out
}

// redisCommands reads INFO commandstats: cmdstat_get:calls=3,usec=12,usec_per_call=4.00,...
func redisCommands(c *respConn, out *RedisStats) error {
	v, err := c.do("INFO", "commandstats")
	if err != nil {
		return err
	}
	for k, line := range parseInfo(asString(v)) {
		name, ok := strings.CutPrefix(k, "cmdstat_")
		if !ok {
			continue
		}
		cmd := RedisCommand{Command: strings.ToUpper(strings.ReplaceAll(name, "|", " "))}
		for _, field := range strings.Split(line, ",") {
			fname, value, _ := strings.Cut(field, "=")
			switch fname {
			case "calls":
				cmd.Calls, _ = strconv.ParseInt(value, 10, 64)
			case "usec":
				usec, _ := strconv.ParseFloat(value, 64)
				cmd.TotalMs = usec / 1000
			case "usec_per_call":
				usec, _ := strconv.ParseFloat(value, 64)
				cmd.AvgMs = usec / 1000
			}
		}
		out.Commands = append(out.Commands, cmd)
	}
	sort.Slice(out.Commands, func(i, j int) bool {
		a, b := out.Commands[i], out.Commands[j]
		if a.TotalMs != b.TotalMs {
			return a.TotalMs > b.TotalMs
		}
		return a.Command < b.Command
	})
	if len(out.Commands) > topCommands {
		out.Commands = out.Commands[:topCommands]
	}
	return nil
}

func redisSlowLog(c *respConn, out *RedisStats) error {
	// A managed Redis often refuses CONFIG; the log itself still reads.
	if v, err := c.do("CONFIG", "GET", "slowlog-log-slower-than"); err == nil {
		if pair := asList(v); len(pair) == 2 {
			if usec, err := strconv.ParseFloat(asString(pair[1]), 64); err == nil {
				threshold := usec / 1000
				out.SlowLog.ThresholdMs = &threshold
			}
		}
	}
	v, err := c.do("SLOWLOG", "GET", strconv.Itoa(slowEntries))
	if err != nil {
		return err
	}
	for _, e := range asList(v) {
		f := asList(e)
		if len(f) < 4 {
			continue
		}
		var args []string
		for _, a := range asList(f[3]) {
			args = append(args, asString(a))
		}
		entry := RedisSlowCmd{
			ID:         asInt(f[0]),
			AtMs:       float64(asInt(f[1])) * 1000,
			DurationMs: float64(asInt(f[2])) / 1000,
			Command:    redisStatement(args),
		}
		if len(f) >= 6 {
			entry.Client, entry.ClientName = asString(f[4]), asString(f[5])
		}
		out.SlowLog.Entries = append(out.SlowLog.Entries, entry)
	}
	return nil
}

// redisBiggestKeys looks at a bounded sample of the keyspace and keeps the
// largest. On a database bigger than the sample it is a sample: the largest
// key of all may not be in it, and the note says so.
func redisBiggestKeys(c *respConn, t *target, out *RedisStats) error {
	b := &out.BiggestKeys
	b.DB, _ = strconv.Atoi(t.database)
	if v, err := c.do("DBSIZE"); err == nil {
		b.Total = asInt(v)
	}
	deadline := time.Now().Add(sampleBudget)
	cursor := "0"
	var keys []RedisKey
	for {
		v, err := c.do("SCAN", cursor, "COUNT", scanCount)
		if err != nil {
			return err
		}
		reply := asList(v)
		if len(reply) != 2 {
			return unexpected("SCAN", v)
		}
		cursor = asString(reply[0])
		var cmds [][]string
		var names []string
		for _, k := range asList(reply[1]) {
			name := asString(k)
			names = append(names, name)
			cmds = append(cmds, []string{"MEMORY", "USAGE", name, "SAMPLES", "5"}, []string{"TYPE", name})
		}
		if len(cmds) > 0 {
			replies, err := c.pipeline(cmds)
			if err != nil {
				return err
			}
			for i, name := range names {
				size, typ := replies[2*i], replies[2*i+1]
				if err := redisCommandError(size); err != nil {
					return err
				}
				if size == nil { // gone since the SCAN
					continue
				}
				keys = append(keys, RedisKey{Key: clipText(name, 200), Type: asString(typ), Bytes: asInt(size)})
			}
			b.Sampled += int64(len(names))
		}
		if cursor == "0" {
			b.Complete = true
			break
		}
		if b.Sampled >= sampleKeys || time.Now().After(deadline) {
			break
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Bytes != keys[j].Bytes {
			return keys[i].Bytes > keys[j].Bytes
		}
		return keys[i].Key < keys[j].Key
	})
	if len(keys) > topKeys {
		keys = keys[:topKeys]
	}
	b.Keys = append(b.Keys, keys...)
	if b.Complete {
		b.Note = "Every key in this database was measured."
	} else {
		b.Note = "A sample: " + strconv.FormatInt(b.Sampled, 10) + " keys were measured out of about " +
			strconv.FormatInt(b.Total, 10) + ", so a larger key may exist. Leadline never walks a whole large keyspace."
	}
	return nil
}

func ms(t time.Time) float64 { return float64(t.UnixNano()) / 1e6 }
