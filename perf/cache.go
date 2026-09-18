package perf

import (
	"container/list"
	"context"
	"net/http"
	"sync"
	"time"
)

// Entry is one cached response.
type Entry struct {
	Body        []byte
	ContentType string
	StoredAt    time.Time
}

// Store holds cached response bodies, tagged so a caller can invalidate every
// entry that depended on some piece of data without having to know every URL
// that ever served it. This package ships only MemoryStore; see the package
// doc for why a Redis-backed Store is left as an interface, not shipped.
type Store interface {
	Get(key string) (Entry, bool)
	Set(key string, entry Entry, ttl time.Duration, tags []string)
	Invalidate(tags ...string)
}

// DefaultMaxEntries bounds a MemoryStore created with maxEntries <= 0. Chosen
// so a handler that forgets to size the cache gets a working default instead
// of an unbounded map that becomes the next incident.
const DefaultMaxEntries = 10000

// MemoryStore is an in-process, size-bounded Store: least-recently-used
// entries are evicted once maxEntries is exceeded, and each entry also
// expires on its own ttl. Each replica of a service has its own MemoryStore,
// so Invalidate only reaches keys this instance holds — a multi-replica
// deployment needing cross-instance invalidation needs a shared Store, which
// is exactly the case Store as an interface exists for.
type MemoryStore struct {
	mu         sync.Mutex
	maxEntries int
	order      *list.List // front = most recently used
	items      map[string]*list.Element
	byTag      map[string]map[string]struct{} // tag -> set of keys
}

type memEntry struct {
	key       string
	entry     Entry
	expiresAt time.Time
	tags      []string
}

// NewMemoryStore returns a MemoryStore holding at most maxEntries entries.
// maxEntries <= 0 uses DefaultMaxEntries.
func NewMemoryStore(maxEntries int) *MemoryStore {
	if maxEntries <= 0 {
		maxEntries = DefaultMaxEntries
	}
	return &MemoryStore{
		maxEntries: maxEntries,
		order:      list.New(),
		items:      make(map[string]*list.Element),
		byTag:      make(map[string]map[string]struct{}),
	}
}

// Get implements Store.
func (s *MemoryStore) Get(key string) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	el, ok := s.items[key]
	if !ok {
		return Entry{}, false
	}
	me, ok := el.Value.(*memEntry)
	if !ok {
		return Entry{}, false // unreachable: this store only ever stores *memEntry
	}
	if time.Now().After(me.expiresAt) {
		s.removeLocked(el)
		return Entry{}, false
	}
	s.order.MoveToFront(el)
	return me.entry, true
}

// Set implements Store.
func (s *MemoryStore) Set(key string, entry Entry, ttl time.Duration, tags []string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.items[key]; ok {
		s.removeLocked(el)
	}

	me := &memEntry{key: key, entry: entry, expiresAt: time.Now().Add(ttl), tags: tags}
	el := s.order.PushFront(me)
	s.items[key] = el
	for _, tag := range tags {
		if s.byTag[tag] == nil {
			s.byTag[tag] = make(map[string]struct{})
		}
		s.byTag[tag][key] = struct{}{}
	}

	for s.order.Len() > s.maxEntries {
		oldest := s.order.Back()
		if oldest == nil {
			break
		}
		s.removeLocked(oldest)
	}
}

// Invalidate implements Store. A tag with nothing recorded under it is a
// no-op, not an error.
func (s *MemoryStore) Invalidate(tags ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, tag := range tags {
		for key := range s.byTag[tag] {
			if el, ok := s.items[key]; ok {
				s.removeLocked(el)
			}
		}
		delete(s.byTag, tag)
	}
}

// removeLocked removes el from every index. Callers must hold s.mu.
func (s *MemoryStore) removeLocked(el *list.Element) {
	me, ok := el.Value.(*memEntry)
	if !ok {
		return // unreachable: this store only ever stores *memEntry
	}
	s.order.Remove(el)
	delete(s.items, me.key)
	for _, tag := range me.tags {
		if set, ok := s.byTag[tag]; ok {
			delete(set, me.key)
			if len(set) == 0 {
				delete(s.byTag, tag)
			}
		}
	}
}

// tagsKey is the context key AddCacheTag and ResponseCache share.
type tagsKey struct{}

// AddCacheTag records an invalidation tag against the request being handled,
// so ResponseCache knows what to key the eventual response under. A handler
// serving data for entity "123" calls perf.AddCacheTag(r.Context(), "entity:123")
// before writing its response; a later perf.Invalidate("entity:123") (via the
// Store directly) then purges every cached response tagged with it,
// regardless of which URL served it.
//
// Calling it outside a request ResponseCache wrapped is a harmless no-op —
// there is nowhere for the tag to go, so it is dropped rather than panicking.
func AddCacheTag(ctx context.Context, tag string) {
	if tags, ok := ctx.Value(tagsKey{}).(*[]string); ok {
		*tags = append(*tags, tag)
	}
}

// KeyFunc computes a cache key for a request.
type KeyFunc func(r *http.Request) string

// DefaultKey keys on method, path and raw query. A handler whose response
// also varies on something else — a tenant header, an Accept-Language —
// must supply its own KeyFunc, or two different responses will share one
// cache entry.
func DefaultKey(r *http.Request) string {
	return r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery
}

// CacheOptions configures ResponseCache. The zero value caches for one
// minute, keyed by DefaultKey.
type CacheOptions struct {
	TTL     time.Duration
	KeyFunc KeyFunc
}

func (o CacheOptions) withDefaults() CacheOptions {
	if o.TTL <= 0 {
		o.TTL = time.Minute
	}
	if o.KeyFunc == nil {
		o.KeyFunc = DefaultKey
	}
	return o
}

// ResponseCache caches successful (2xx) GET/HEAD response bodies in store,
// tagged via AddCacheTag for explicit invalidation. It never caches a
// non-2xx response — caching a transient failure would turn it into a
// lasting one for as long as the entry's ttl runs.
func ResponseCache(store Store, opts CacheOptions) func(http.Handler) http.Handler {
	opts = opts.withDefaults()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet && r.Method != http.MethodHead {
				next.ServeHTTP(w, r)
				return
			}

			key := opts.KeyFunc(r)
			if entry, ok := store.Get(key); ok {
				if entry.ContentType != "" {
					w.Header().Set("Content-Type", entry.ContentType)
				}
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(entry.Body)
				return
			}

			tags := &[]string{}
			ctx := context.WithValue(r.Context(), tagsKey{}, tags)
			rec := &cacheRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			if rec.status >= 200 && rec.status < 300 {
				store.Set(key, Entry{
					Body:        rec.buf,
					ContentType: rec.Header().Get("Content-Type"),
					StoredAt:    time.Now(),
				}, opts.TTL, *tags)
			}
		})
	}
}

// cacheRecorder forwards every write live (so the client sees bytes as they
// are produced) while also buffering them, so the same bytes can be stored
// once the handler finishes and its status is known.
type cacheRecorder struct {
	http.ResponseWriter
	status      int
	buf         []byte
	wroteHeader bool
}

func (r *cacheRecorder) WriteHeader(status int) {
	r.status = status
	if !r.wroteHeader {
		r.wroteHeader = true
		r.Header().Set("X-Cache", "MISS")
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *cacheRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.buf = append(r.buf, p...)
	return r.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController and other middleware (Gzip, ETag)
// reach the original writer.
func (r *cacheRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
