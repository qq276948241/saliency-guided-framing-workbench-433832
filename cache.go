/*
 * Copyright (c) 2014-2026 Christian Muehlhaeuser
 *
 * Permission is hereby granted, free of charge, to any person obtaining a copy
 * of this software and associated documentation files (the "Software"), to deal
 * in the Software without restriction, including without limitation the rights
 * to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
 * copies of the Software, and to permit persons to whom the Software is
 * furnished to do so, subject to the following conditions:
 *
 * The above copyright notice and this permission notice shall be included in all
 * copies or substantial portions of the Software.
 */

package smartcrop

import (
	"container/list"
	"sync"
)

// resultCache is a bounded LRU cache of Result values keyed by the canonical
// image digest, request dimensions, metadata and config fingerprint.
//
// Only completed results are stored: canceled, timed-out and failed requests
// never populate the cache. The cache can be disabled (capacity < 0) and
// cleared deterministically, which the acceptance tests use to assert cleanup.
type resultCache struct {
	mu        sync.Mutex
	capacity  int
	order     *list.List
	items     map[string]*list.Element
	hits      int
	misses    int
	evictions int
}

type cacheEntry struct {
	key    string
	result Result
}

func newResultCache(capacity int) *resultCache {
	return &resultCache{
		capacity: capacity,
		order:    list.New(),
		items:    make(map[string]*list.Element),
	}
}

func (c *resultCache) enabled() bool {
	return c != nil && c.capacity > 0
}

// get returns a deep-enough copy of the cached ranking. Candidate values are
// plain values; the slice is copied so callers can mutate their result without
// corrupting the cache.
func (c *resultCache) get(key string) (Result, bool) {
	if !c.enabled() {
		return Result{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses++
		return Result{}, false
	}
	c.hits++
	c.order.MoveToFront(el)
	entry := el.Value.(*cacheEntry)
	r := entry.result
	r.Top = append([]Candidate(nil), entry.result.Top...)
	return r, true
}

// put stores a completed ranking and evicts the least recently used entry when
// the capacity is exceeded.
func (c *resultCache) put(key string, r Result) {
	if !c.enabled() {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	stored := r
	stored.Top = append([]Candidate(nil), r.Top...)

	if el, ok := c.items[key]; ok {
		el.Value.(*cacheEntry).result = stored
		c.order.MoveToFront(el)
		return
	}

	el := c.order.PushFront(&cacheEntry{key: key, result: stored})
	c.items[key] = el
	for c.order.Len() > c.capacity {
		back := c.order.Back()
		if back == nil {
			break
		}
		c.order.Remove(back)
		delete(c.items, back.Value.(*cacheEntry).key)
		c.evictions++
	}
}

// clear drops every entry. It is safe to call concurrently with get/put.
func (c *resultCache) clear() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.items = make(map[string]*list.Element)
}

// len reports the number of cached entries.
func (c *resultCache) len() int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// snapshot returns cache counters without exposing internal state.
func (c *resultCache) snapshot() (hits, misses, entries, evictions int) {
	if c == nil {
		return 0, 0, 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits, c.misses, c.order.Len(), c.evictions
}
