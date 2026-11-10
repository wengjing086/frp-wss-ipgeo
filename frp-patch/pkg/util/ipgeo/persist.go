// Copyright 2026 The frp Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipgeo

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// The online service throttles bursts aggressively, so answers are remembered
// across restarts: without persistence every restart of frps would re-query
// every known client at once and get rate limited.

const persistFlushInterval = time.Minute

var (
	persistMu    sync.Mutex
	persistPath  string
	persistDirty bool
)

type persistRecord struct {
	Value    string `json:"value"`
	Deadline int64  `json:"deadline"`
}

// EnablePersistence loads cached answers from the given file and writes them
// back every minute. Errors are ignored, a read only filesystem simply keeps
// the answers in memory for the lifetime of the process.
func EnablePersistence(path string) {
	if path == "" {
		return
	}
	persistMu.Lock()
	persistPath = path
	persistMu.Unlock()

	loadPersisted(path)

	go func() {
		ticker := time.NewTicker(persistFlushInterval)
		defer ticker.Stop()
		for range ticker.C {
			flushPersisted()
		}
	}()
}

// Flush writes the cache back if it changed since the last flush.
func Flush() { flushPersisted() }

func loadPersisted(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var records map[string]persistRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return
	}
	now := time.Now()
	cacheMu.Lock()
	for ip, r := range records {
		if r.Deadline <= now.Unix() {
			continue
		}
		cache[ip] = cacheEntry{value: r.Value, deadline: time.Unix(r.Deadline, 0)}
	}
	cacheMu.Unlock()
}

func flushPersisted() {
	persistMu.Lock()
	path := persistPath
	dirty := persistDirty
	persistDirty = false
	persistMu.Unlock()
	if path == "" || !dirty {
		return
	}

	cacheMu.RLock()
	now := time.Now()
	records := make(map[string]persistRecord, len(cache))
	for ip, e := range cache {
		if now.After(e.deadline) {
			continue
		}
		records[ip] = persistRecord{Value: e.value, Deadline: e.deadline.Unix()}
	}
	cacheMu.RUnlock()

	data, err := json.Marshal(records)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
	}
}

func markDirty() {
	persistMu.Lock()
	persistDirty = true
	persistMu.Unlock()
}
