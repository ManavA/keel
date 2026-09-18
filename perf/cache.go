package perf

import (
	"container/list"
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry is one cached response: status, every response header, and the
// body. Replaying all three on a hit is what lets a cached 201 come back as
// 201 (not 200) and lets a handler's own Cache-Control or Content-Encoding
// survive a cache hit unchanged.
type Entry struct {
	Status int
	Header http.Header
	Body   []byte
}

// size estimates Entry's memory footprint: the body plus a rough accounting
// of header bytes. It is an approximation used only to bound MemoryStore,
// not an exact byte count.
func (e Entry) size() int64 {
	n := int64(len(e.Body))
	for k, vs := range e.Header {
		n += int64(len(k))
		for _, v := range vs {
			n += int64(len(v))
		}
	}
	return n
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

// DefaultMaxBytes bounds a MemoryStore's total size when MemoryStoreOptions
// leaves MaxBytes unset.
const DefaultMaxBytes = 64 << 20 // 64 MiB

// DefaultMaxEntryBytes bounds a single entry's size when MemoryStoreOptions
// leaves MaxEntryBytes unset. An entry over this size is not cached at all,
// so one large response cannot by itself evict everything else in the
// store.
const DefaultMaxEntryBytes = 4 << 20 // 4 MiB

// MemoryStoreOptions configures NewMemoryStore. The zero value uses
// DefaultMaxBytes and DefaultMaxEntryBytes.
type MemoryStoreOptions struct {
	MaxBytes      int64
	MaxEntryBytes int64
}

func (o MemoryStoreOptions) withDefaults() MemoryStoreOptions {
	if o.MaxBytes <= 0 {
		o.MaxBytes = DefaultMaxBytes
	}
	if o.MaxEntryBytes <= 0 {
		o.MaxEntryBytes = DefaultMaxEntryBytes
	}
	return o
}

// MemoryStore is an in-process, size-bounded Store: least-recently-used
// entries are evicted once the total estimated size exceeds MaxBytes, and
// each entry also expires on its own ttl. Each replica of a service has its
// own MemoryStore, so Invalidate only reaches keys this instance holds — a
// multi-replica deployment needing cross-instance invalidation needs a
// shared Store, which is exactly the case Store as an interface exists for.
type MemoryStore struct {
	mu            sync.Mutex
	maxBytes      int64
	maxEntryBytes int64
	curBytes      int64
	order         *list.List // front = most recently used
	items         map[string]*list.Element
	byTag         map[string]map[string]struct{} // tag -> set of keys
}

type memEntry struct {
	key       string
	entry     Entry
	size      int64
	expiresAt time.Time
	tags      []string
}

// NewMemoryStore returns a MemoryStore bounded by opts.
func NewMemoryStore(opts MemoryStoreOptions) *MemoryStore {
	opts = opts.withDefaults()
	return &MemoryStore{
		maxBytes:      opts.MaxBytes,
		maxEntryBytes: opts.MaxEntryBytes,
		order:         list.New(),
		items:         make(map[string]*list.Element),
		byTag:         make(map[string]map[string]struct{}),
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

// Set implements Store. An entry larger than MaxEntryBytes is not stored at
// all; Set returns silently, the same way a cache write that raced a
// concurrent eviction would.
func (s *MemoryStore) Set(key string, entry Entry, ttl time.Duration, tags []string) {
	size := entry.size()

	s.mu.Lock()
	defer s.mu.Unlock()

	if size > s.maxEntryBytes {
		return
	}

	if el, ok := s.items[key]; ok {
		s.removeLocked(el)
	}

	me := &memEntry{key: key, entry: entry, size: size, expiresAt: time.Now().Add(ttl), tags: tags}
	el := s.order.PushFront(me)
	s.items[key] = el
	s.curBytes += size
	for _, tag := range tags {
		if s.byTag[tag] == nil {
			s.byTag[tag] = make(map[string]struct{})
		}
		s.byTag[tag][key] = struct{}{}
	}

	for s.curBytes > s.maxBytes {
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
	s.curBytes -= me.size
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

// DefaultKey keys on method, path, raw query, and whether the client accepts
// a gzip response, so a compressed and an uncompressed representation of the
// same URL are never stored under the same key. A handler whose response
// also varies on something else — a tenant header, an Accept-Language —
// must supply its own KeyFunc that folds in that value too, or two different
// responses will share one cache entry.
//
// DefaultKey does not need to account for authentication: ResponseCache
// never caches a request carrying Authorization or Cookie, regardless of
// KeyFunc, so a caller cannot make a private response cacheable by mistake
// through a custom key alone.
func DefaultKey(r *http.Request) string {
	enc := "identity"
	if acceptsGzip(r) {
		enc = "gzip"
	}
	return r.Method + " " + r.URL.Path + "?" + r.URL.RawQuery + " " + enc
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

// ResponseCache caches GET/HEAD responses in store, tagged via AddCacheTag
// for explicit invalidation.
//
// It refuses to read or write the cache at all for a request carrying an
// Authorization or Cookie header: caching such a request's response under a
// key derived only from the URL would serve one caller's response to
// another, and a KeyFunc that forgets to fold in the credential reproduces
// this even with a custom key. Handlers that require per-user responses
// must sit behind this refusal, not rely on it as their only defense.
//
// It refuses to store a response that carries Set-Cookie; a Cache-Control
// of private, no-store, no-cache, or max-age=0; or a Vary naming anything
// other than Accept-Encoding (the only header DefaultKey folds into the
// key) — and never stores a non-2xx response, since caching a transient
// failure would turn it into a lasting one for as long as the entry's ttl
// runs.
func ResponseCache(store Store, opts CacheOptions) func(http.Handler) http.Handler {
	opts = opts.withDefaults()

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !cacheableRequest(r) {
				next.ServeHTTP(w, r)
				return
			}

			key := opts.KeyFunc(r)
			if entry, ok := store.Get(key); ok {
				for k, vs := range entry.Header {
					w.Header()[k] = append([]string(nil), vs...)
				}
				w.Header().Set("X-Cache", "HIT")
				w.WriteHeader(entry.Status)
				_, _ = w.Write(entry.Body)
				return
			}

			tags := &[]string{}
			ctx := context.WithValue(r.Context(), tagsKey{}, tags)
			rec := &cacheRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r.WithContext(ctx))

			if cacheableResponse(rec.status, rec.Header()) {
				store.Set(key, Entry{
					Status: rec.status,
					Header: cloneHeader(rec.Header()),
					Body:   rec.buf,
				}, opts.TTL, *tags)
			}
		})
	}
}

// cacheableRequest reports whether ResponseCache should look at the cache
// for r at all. GET/HEAD only, and never for a request carrying credentials.
func cacheableRequest(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
		return false
	}
	return true
}

// cacheableResponse reports whether a response may be stored: a 2xx status,
// no Set-Cookie, a Cache-Control that does not forbid or immediately expire
// storage, and a Vary that names nothing this cache does not already fold
// into its key.
func cacheableResponse(status int, header http.Header) bool {
	if status < 200 || status >= 300 {
		return false
	}
	if header.Get("Set-Cookie") != "" {
		return false
	}
	for _, directive := range strings.Split(header.Get("Cache-Control"), ",") {
		d := strings.ToLower(strings.TrimSpace(directive))
		switch {
		case d == "private", d == "no-store":
			return false
		// no-cache means "store it, but revalidate with the origin before
		// every use" — this cache has no revalidation path, so honoring it
		// means refusing to store rather than serving an unvalidated copy.
		case d == "no-cache":
			return false
		// max-age=0 marks the response stale on arrival; storing it for the
		// configured TTL anyway would ignore that.
		case strings.HasPrefix(d, "max-age="):
			if n, err := strconv.Atoi(strings.TrimPrefix(d, "max-age=")); err == nil && n <= 0 {
				return false
			}
		}
	}
	// DefaultKey folds in Accept-Encoding, so a Vary naming only that is
	// already accounted for. A Vary naming anything else — including "*" —
	// means the response depends on something the key does not vary by,
	// and storing it under one key would replay it across every value of
	// whatever the key is missing.
	for _, v := range strings.Split(header.Get("Vary"), ",") {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" && v != "accept-encoding" {
			return false
		}
	}
	return true
}

func cloneHeader(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, vs := range h {
		out[k] = append([]string(nil), vs...)
	}
	return out
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

// Flush lets a handler that type-asserts http.Flusher directly (rather than
// going through http.ResponseController, which already uses Unwrap) still
// find one. Without this, a streaming handler placed under ResponseCache
// silently loses the ability to flush.
func (r *cacheRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
