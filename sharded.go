package cache

import (
	"crypto/rand"
	"math"
	"math/big"
	insecurerand "math/rand"
	"os"
	"runtime"
	"sync/atomic"
	"time"
)

// ShardedCache This is an experimental and unexported (for now) attempt at making a cache
// with better algorithmic complexity than the standard one, namely by
// preventing write locks of the entire cache when an item is added. As of the
// time of writing, the overhead of selecting buckets results in cache
// operations being about twice as slow as for the standard cache with small
// total cache sizes, and faster for larger ones.
//
// See cache_test.go for a few benchmarks.
type ShardedCache struct {
	*shardedCache
}

type shardedCache struct {
	seed      uint32
	m         uint32
	count     uint32
	onEvicted func(string, interface{})
	cs        []*cache
	janitor   *shardedJanitor
}

// djb2 with better shuffling. 5x faster than FNV with the hash.Hash overhead.
func djb33(seed uint32, k string) uint32 {
	var (
		l = uint32(len(k))
		d = 5381 + seed + l
		i = uint32(0)
	)
	// Why is all this 5x faster than a for loop?
	if l >= 4 {
		for i < l-4 {
			d = (d * 33) ^ uint32(k[i])
			d = (d * 33) ^ uint32(k[i+1])
			d = (d * 33) ^ uint32(k[i+2])
			d = (d * 33) ^ uint32(k[i+3])
			i += 4
		}
	}
	switch l - i {
	case 1:
	case 2:
		d = (d * 33) ^ uint32(k[i])
	case 3:
		d = (d * 33) ^ uint32(k[i])
		d = (d * 33) ^ uint32(k[i+1])
	case 4:
		d = (d * 33) ^ uint32(k[i])
		d = (d * 33) ^ uint32(k[i+1])
		d = (d * 33) ^ uint32(k[i+2])
	}
	return d ^ (d >> 16)
}

func (sc *shardedCache) bucket(k string) *cache {
	return sc.cs[djb33(sc.seed, k)%sc.m]
}
func (sc *shardedCache) SetDefault(k string, x interface{}) {
	c := sc.bucket(k)
	c.Set(k, x, c.defaultExpiration)
	atomic.AddUint32(&sc.count, 1)
}
func (sc *shardedCache) Set(k string, x interface{}, d time.Duration) {
	c := sc.bucket(k)
	c.Set(k, x, d)
	atomic.AddUint32(&sc.count, 1)
}

func (sc *shardedCache) SetRenew(k string, x interface{}, d time.Duration) {
	c := sc.bucket(k)
	c.Set(k, x, d)
}

func (sc *shardedCache) Add(k string, x interface{}, d time.Duration) error {
	c := sc.bucket(k)
	if sc.onEvicted != nil {
		c.OnEvicted(sc.onEvicted)
	}
	return c.Add(k, x, d)
}

func (sc *shardedCache) Replace(k string, x interface{}, d time.Duration) error {
	c := sc.bucket(k)
	if sc.onEvicted != nil {
		c.OnEvicted(sc.onEvicted)
	}
	return c.Replace(k, x, d)
}

func (sc *shardedCache) Get(k string) (interface{}, bool) {
	return sc.bucket(k).Get(k)
}

func (sc *shardedCache) Increment(k string, n int64) error {
	return sc.bucket(k).Increment(k, n)
}

func (sc *shardedCache) IncrementFloat(k string, n float64) error {
	return sc.bucket(k).IncrementFloat(k, n)
}

func (sc *shardedCache) Decrement(k string, n int64) error {
	return sc.bucket(k).Decrement(k, n)
}

func (sc *shardedCache) Delete(k string) {
	sc.bucket(k).Delete(k)
	atomic.AddUint32(&sc.count, ^uint32(0))

}

func (sc *shardedCache) DeleteExpired() {
	for _, v := range sc.cs {
		count := v.DeleteExpired()
		if count > 0 {
			atomic.AddUint32(&sc.count, ^uint32(count-1))
		}
	}
}

func (sc *shardedCache) OnEvicted(f func(string, interface{})) {
	sc.onEvicted = f
}

// Returns the items in the cache. This may include items that have expired,
// but have not yet been cleaned up. If this is significant, the Expiration
// fields of the items should be checked. Note that explicit synchronization
// is needed to use a cache and its corresponding Items() return values at
// the same time, as the maps are shared.
func (sc *shardedCache) Items() []map[string]Item {
	res := make([]map[string]Item, len(sc.cs))
	for i, v := range sc.cs {
		res[i] = v.Items()
	}
	return res
}

func (sc *shardedCache) ItemCount() uint32 {
	return atomic.LoadUint32(&sc.count)
}

func (sc *shardedCache) Flush() {
	for _, v := range sc.cs {
		v.Flush()
		atomic.AddUint32(&sc.count, ^uint32(0))
	}
}

type shardedJanitor struct {
	Interval time.Duration
	stop     chan bool
}

func (j *shardedJanitor) Run(sc *shardedCache) {
	j.stop = make(chan bool)
	tick := time.Tick(j.Interval)
	for {
		select {
		case <-tick:
			sc.DeleteExpired()

		case <-j.stop:
			return
		}
	}
}

func stopShardedJanitor(sc *ShardedCache) {
	sc.janitor.stop <- true
}

func runShardedJanitor(sc *shardedCache, ci time.Duration) {
	j := &shardedJanitor{
		Interval: ci,
	}
	sc.janitor = j
	go j.Run(sc)
}

func newShardedCache(n int, de time.Duration) *shardedCache {
	max := big.NewInt(0).SetUint64(uint64(math.MaxUint32))
	rnd, err := rand.Int(rand.Reader, max)
	var seed uint32
	if err != nil {
		os.Stderr.Write([]byte("WARNING: go-cache's newShardedCache failed to read from the system CSPRNG (/dev/urandom or equivalent.) Your system's security may be compromised. Continuing with an insecure seed.\n"))
		seed = insecurerand.Uint32()
	} else {
		seed = uint32(rnd.Uint64())
	}
	sc := &shardedCache{
		seed: seed,
		m:    uint32(n),
		cs:   make([]*cache, n),
	}
	for i := 0; i < n; i++ {
		c := &cache{
			defaultExpiration: de,
			items:             map[string]Item{},
		}
		sc.cs[i] = c
	}
	return sc
}

// NewSharded sc
func NewSharded(defaultExpiration, cleanupInterval time.Duration, shards int) *ShardedCache {
	if defaultExpiration == 0 {
		defaultExpiration = -1
	}
	sc := newShardedCache(shards, defaultExpiration)
	atomic.StoreUint32(&sc.count, 0)
	SC := &ShardedCache{sc}
	if cleanupInterval > 0 {
		runShardedJanitor(sc, cleanupInterval)
		runtime.SetFinalizer(SC, stopShardedJanitor)
	}
	return SC
}
