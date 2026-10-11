package store

import (
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"runtime/debug"
	"time"
)

// Memory is what the daemon's memory is made of, as far as it can tell from
// inside: Go's own heap, and what DuckDB's buffer manager has given out, by
// what for. The process's resident size is more than their sum: DuckDB's
// allocations outside its buffer manager, and pages the system's allocator
// has been handed back and not yet returned, are in neither.
type Memory struct {
	Go     GoMemory     `json:"go"`
	DuckDB DuckDBMemory `json:"duckdb"`
}

type GoMemory struct {
	// HeapAllocBytes is live objects; HeapIdleBytes spans with none, of
	// which HeapReleasedBytes have gone back to the system; SysBytes all
	// Go has from the system, stacks and runtime included.
	HeapAllocBytes    uint64 `json:"heap_alloc_bytes"`
	HeapIdleBytes     uint64 `json:"heap_idle_bytes"`
	HeapReleasedBytes uint64 `json:"heap_released_bytes"`
	SysBytes          uint64 `json:"sys_bytes"`
	Goroutines        int    `json:"goroutines"`
}

type DuckDBMemory struct {
	LimitBytes int64 `json:"limit_bytes"`
	Threads    int64 `json:"threads"`
	// UsedBytes is the buffer manager's total; ByTag splits it.
	UsedBytes int64            `json:"used_bytes"`
	TempBytes int64            `json:"temp_bytes"`
	ByTag     map[string]int64 `json:"by_tag"`
}

// Memory reports where the daemon's memory is. It is for `leadline doctor`
// and for whoever is wondering; nothing in the app reads it.
func (s *Store) Memory(ctx context.Context) (*Memory, error) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	out := &Memory{Go: GoMemory{HeapAllocBytes: ms.HeapAlloc, HeapIdleBytes: ms.HeapIdle,
		HeapReleasedBytes: ms.HeapReleased, SysBytes: ms.Sys, Goroutines: runtime.NumGoroutine()}}
	out.DuckDB.ByTag = map[string]int64{}
	rows, err := s.db.QueryContext(ctx, `SELECT tag, memory_usage_bytes, temporary_storage_bytes FROM duckdb_memory()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var tag string
		var used, temp int64
		if err := rows.Scan(&tag, &used, &temp); err != nil {
			return nil, err
		}
		out.DuckDB.UsedBytes += used
		out.DuckDB.TempBytes += temp
		if used > 0 {
			out.DuckDB.ByTag[tag] = used
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// The limit as a number of bytes: the setting itself reads "488.2 MiB".
	err = s.db.QueryRowContext(ctx, `
SELECT (SELECT CAST(current_setting('threads') AS BIGINT)),
	(SELECT coalesce(max(CASE WHEN value LIKE '%GiB' THEN CAST(replace(value, ' GiB', '') AS DOUBLE) * 1073741824
		WHEN value LIKE '%MiB' THEN CAST(replace(value, ' MiB', '') AS DOUBLE) * 1048576
		WHEN value LIKE '%KiB' THEN CAST(replace(value, ' KiB', '') AS DOUBLE) * 1024 ELSE 0 END), 0)::BIGINT
	 FROM duckdb_settings() WHERE name = 'memory_limit')`).Scan(&out.DuckDB.Threads, &out.DuckDB.LimitBytes)
	return out, err
}

const (
	// tidyAfter is how long the store is left alone after its last
	// statement before it gives memory back: long enough that someone
	// clicking through the app never waits on it.
	tidyAfter = 10 * time.Second
	// idleCache is what DuckDB's cache of the database's blocks is cut to
	// meanwhile. The same blocks are in the system's file cache, which is
	// not this process's memory, so reading them again is cheap.
	idleCache = 64 << 20
)

func (s *Store) used() { s.lastUse.Store(time.Now().UnixNano()) }

// tidyLoop gives memory back once the store has gone quiet.
//
// A daemon that sits in the background all day is asked a burst of
// questions now and then, when someone opens the app. Each big aggregate
// takes working memory, and leaves two things behind: the blocks of the
// database it read, which DuckDB keeps cached up to its memory limit, and
// the freed working memory, which the system's allocator keeps for the
// next allocation. On a store of millions of spans that is hundreds of
// megabytes held for nothing. So ten seconds after the last statement the
// cache is cut down and the allocator asked to return what it holds.
func (s *Store) tidyLoop() {
	defer s.bg.Done()
	tick := time.NewTicker(tidyAfter / 2)
	defer tick.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		last := s.lastUse.Load()
		if last == s.tidied || time.Since(time.Unix(0, last)) < tidyAfter {
			continue
		}
		s.tidy()
		// The statements tidy ran moved lastUse; that is not use.
		s.tidied = s.lastUse.Load()
	}
}

// tidy drops DuckDB's block cache and returns freed memory to the system.
func (s *Store) tidy() {
	if s.memory > idleCache {
		// Lowering the limit evicts down to it; then it goes straight back
		// up. No write lands in between: one that found the limit low
		// could fail, and its rows would be lost. A read that starts in
		// those few milliseconds works within the low limit, spilling to
		// disk if it is a big one.
		s.wmu.Lock()
		if s.uconn != nil && s.exec(fmt.Sprintf(`SET memory_limit = '%dMiB'`, idleCache>>20)) == nil {
			if err := s.exec(fmt.Sprintf(`SET memory_limit = '%dMiB'`, max(64, s.memory>>20))); err != nil {
				slog.Error("restoring the memory limit", "err", err)
			}
		}
		s.wmu.Unlock()
	}
	trimAllocator()
	debug.FreeOSMemory()
}
