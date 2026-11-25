package notestore

import (
	"container/list"
	"sync"
)

// lru keeps values up to a total size, dropping the least recently used.
type lru[K comparable, V any] struct {
	mu           sync.Mutex
	limit, bytes int64
	items        map[K]*list.Element
	order        list.List
}

type lruItem[K comparable, V any] struct {
	key   K
	value V
	size  int64
}

func newLRU[K comparable, V any](limit int64) *lru[K, V] {
	return &lru[K, V]{limit: max(limit, 1<<20), items: map[K]*list.Element{}}
}

func (c *lru[K, V]) get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*lruItem[K, V]).value, true
	}
	var zero V
	return zero, false
}

// put keeps value unless it alone exceeds the limit.
func (c *lru[K, V]) put(key K, value V, size int64) {
	if size > c.limit {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		item := e.Value.(*lruItem[K, V])
		c.bytes += size - item.size
		item.value, item.size = value, size
		c.order.MoveToFront(e)
	} else {
		c.items[key] = c.order.PushFront(&lruItem[K, V]{key, value, size})
		c.bytes += size
	}
	for c.bytes > c.limit {
		e := c.order.Back()
		item := e.Value.(*lruItem[K, V])
		c.order.Remove(e)
		delete(c.items, item.key)
		c.bytes -= item.size
	}
}

func (c *lru[K, V]) remove(key K) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[key]; ok {
		c.order.Remove(e)
		delete(c.items, key)
		c.bytes -= e.Value.(*lruItem[K, V]).size
	}
}
