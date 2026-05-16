// fxfiles-analytics — minimal cookieless analytics for AI-generated static
// sites served from public IPFS gateways. See README.md for the design and
// API contract. Token-authenticated; no JWT; pure stdlib.
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	listenAddr       string
	dataDir          string
	allowedSuffixes  []string
	rateLimitPerMin  int
	dailySaltFile    string
	maxRequestBytes  int64
	uniqueVisitorTTL time.Duration
	cleanupInterval  time.Duration
	maxDistinctCIDs  int
}

func loadConfig() (*config, error) {
	c := &config{
		listenAddr:       getenv("LISTEN_ADDR", ":8080"),
		dataDir:          getenv("DATA_DIR", "./data"),
		rateLimitPerMin:  atoiOr(getenv("RATE_LIMIT_PER_MIN", "60"), 60),
		maxRequestBytes:  4 * 1024,
		uniqueVisitorTTL: 30 * 24 * time.Hour, // keep 30 days of daily sets
		cleanupInterval:  6 * time.Hour,
		maxDistinctCIDs:  atoiOr(getenv("MAX_DISTINCT_CIDS", "100000"), 100000),
	}
	gateways := getenv("ALLOWED_GATEWAYS", ".ipfs.dweb.link,.ipfs.cloud.fx.land")
	for _, g := range strings.Split(gateways, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			c.allowedSuffixes = append(c.allowedSuffixes, g)
		}
	}
	c.dailySaltFile = getenv("DAILY_SALT_FILE", filepath.Join(c.dataDir, ".salt"))
	return c, nil
}

// ---------------------------------------------------------------------------
// State: token records + salt rotation, JSON-on-disk persistence
// ---------------------------------------------------------------------------

// tokenRecord stores per-generation aggregate counts. uniqueVisitors is a
// map of "YYYY-MM-DD" → set-of-hashed-visitor-ids so we can collapse repeat
// visits inside one day. Aged-out days are pruned by janitor().
type tokenRecord struct {
	Pageviews      int64                          `json:"pageviews"`
	UniqueVisitors map[string]map[string]struct{} `json:"uniqueVisitors"`
}

func newTokenRecord() *tokenRecord {
	return &tokenRecord{UniqueVisitors: map[string]map[string]struct{}{}}
}

// store is a tiny key-value store wrapping a map[token]*tokenRecord with an
// RWMutex. Atomic snapshot-and-rewrite persistence.
type store struct {
	mu       sync.RWMutex
	records  map[string]*tokenRecord
	salt     []byte
	saltDate string
	dataPath string
	saltPath string
}

func newStore(dataDir, saltFile string) (*store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	s := &store{
		records:  map[string]*tokenRecord{},
		dataPath: filepath.Join(dataDir, "tokens.json"),
		saltPath: saltFile,
	}
	if err := s.loadRecords(); err != nil {
		return nil, fmt.Errorf("load records: %w", err)
	}
	if err := s.loadOrRotateSalt(); err != nil {
		return nil, fmt.Errorf("load salt: %w", err)
	}
	return s, nil
}

func (s *store) loadRecords() error {
	b, err := os.ReadFile(s.dataPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	persisted := map[string]*tokenRecord{}
	if err := json.Unmarshal(b, &persisted); err != nil {
		return err
	}
	// Hydrate missing unique-visitor maps from older snapshots.
	for _, rec := range persisted {
		if rec.UniqueVisitors == nil {
			rec.UniqueVisitors = map[string]map[string]struct{}{}
		}
	}
	s.records = persisted
	return nil
}

func (s *store) persist() error {
	s.mu.RLock()
	defer s.mu.RUnlock()
	b, err := json.Marshal(s.records)
	if err != nil {
		return err
	}
	tmp := s.dataPath + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.dataPath)
}

func (s *store) stats(key string) (pageviews, unique int64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, exists := s.records[key]
	if !exists {
		return 0, 0, false
	}
	var u int64
	for _, day := range rec.UniqueVisitors {
		u += int64(len(day))
	}
	return rec.Pageviews, u, true
}

// recordPageview lazily creates a record for [key] (the IPFS CID) on first
// sight, then increments its pageview counter and adds [visitorHash] to the
// per-day unique-visitor set. Returns [errStoreFull] when the per-instance
// CID cap is exceeded and the CID is new, so callers can return 503/429
// instead of silently dropping. Existing records are always updated.
func (s *store) recordPageview(key, visitorHash, day string, capCIDs int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, exists := s.records[key]
	if !exists {
		if capCIDs > 0 && len(s.records) >= capCIDs {
			return errStoreFull
		}
		rec = newTokenRecord()
		s.records[key] = rec
	}
	rec.Pageviews++
	bucket, ok := rec.UniqueVisitors[day]
	if !ok {
		bucket = map[string]struct{}{}
		rec.UniqueVisitors[day] = bucket
	}
	bucket[visitorHash] = struct{}{}
	return nil
}

// loadOrRotateSalt loads today's salt from disk or rotates it if the file is
// stale. The salt is reseeded at UTC midnight by the janitor goroutine.
func (s *store) loadOrRotateSalt() error {
	today := time.Now().UTC().Format("2006-01-02")
	b, err := os.ReadFile(s.saltPath)
	if err == nil {
		// File format: "YYYY-MM-DD\n<hex>"
		lines := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)
		if len(lines) == 2 && lines[0] == today {
			raw, derr := hex.DecodeString(lines[1])
			if derr == nil && len(raw) >= 16 {
				s.salt = raw
				s.saltDate = today
				return nil
			}
		}
	}
	return s.rotateSalt(today)
}

func (s *store) rotateSalt(day string) error {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	s.mu.Lock()
	s.salt = buf
	s.saltDate = day
	s.mu.Unlock()
	body := fmt.Sprintf("%s\n%s\n", day, hex.EncodeToString(buf))
	tmp := s.saltPath + ".tmp"
	if err := os.MkdirAll(filepath.Dir(s.saltPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.saltPath)
}

// visitorHash returns sha256(salt || ip || ua) truncated to 16 hex chars.
// Truncation is intentional — uniqueness within a single day is enough.
func (s *store) visitorHash(ip, ua string) (string, string) {
	s.mu.RLock()
	salt := s.salt
	day := s.saltDate
	s.mu.RUnlock()
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(ua))
	return hex.EncodeToString(h.Sum(nil)[:8]), day
}

// pruneOldDays drops day-keys older than ttl from every record.
func (s *store) pruneOldDays(ttl time.Duration) {
	cutoff := time.Now().UTC().Add(-ttl).Format("2006-01-02")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.records {
		for day := range rec.UniqueVisitors {
			if day < cutoff {
				delete(rec.UniqueVisitors, day)
			}
		}
	}
}

var errStoreFull = errors.New("store full: distinct CID cap exceeded")

// ---------------------------------------------------------------------------
// Rate limiting: per (token, ip) sliding 1-minute window
// ---------------------------------------------------------------------------

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string][]time.Time
	cap     int
}

func newRateLimiter(perMin int) *rateLimiter {
	return &rateLimiter{buckets: map[string][]time.Time{}, cap: perMin}
}

func (r *rateLimiter) allow(token, ip string) bool {
	key := token + "|" + ip
	cutoff := time.Now().Add(-time.Minute)
	r.mu.Lock()
	defer r.mu.Unlock()
	hits := r.buckets[key]
	// Drop hits older than 60s.
	i := 0
	for i < len(hits) && hits[i].Before(cutoff) {
		i++
	}
	hits = hits[i:]
	if len(hits) >= r.cap {
		r.buckets[key] = hits
		return false
	}
	hits = append(hits, time.Now())
	r.buckets[key] = hits
	return true
}

func (r *rateLimiter) prune() {
	cutoff := time.Now().Add(-time.Minute)
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, hits := range r.buckets {
		i := 0
		for i < len(hits) && hits[i].Before(cutoff) {
			i++
		}
		if i == len(hits) {
			delete(r.buckets, key)
		} else {
			r.buckets[key] = hits[i:]
		}
	}
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

var (
	// CID shape: base58 v0 (`Qm...`) or base32 v1 (`bafy...`/`bafk...`/
	// `bafz...`/`bafyb...`). Lenient on length because IPFS CIDs vary by
	// hash + codec; tighten if you only generate one shape.
	cidPattern = regexp.MustCompile(`^(Qm[1-9A-HJ-NP-Za-km-z]{44}|baf[ykz][a-z0-9]{40,80})$`)
	// Crude bot detector: drop common headless / scraper UA tokens. This is
	// intentionally minimal — see README for the privacy trade-off.
	botPattern = regexp.MustCompile(`(?i)bot|crawler|spider|scrap|wget|curl|http-client|headless|preview`)
)

type server struct {
	cfg     *config
	store   *store
	limiter *rateLimiter
}

func (s *server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/track", s.handleTrack)
	mux.HandleFunc("GET /api/v1/stats/{cid}", s.handleStats)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok")
	})
	return mux
}

type trackPayload struct {
	CID   string `json:"cid"`
	Event string `json:"event"`
	Ref   string `json:"ref"`
}

func (s *server) handleTrack(w http.ResponseWriter, r *http.Request) {
	// Always read the body up to a small cap so an oversized request can't
	// stall us.
	body := http.MaxBytesReader(w, r.Body, s.cfg.maxRequestBytes)
	defer body.Close()
	var p trackPayload
	if err := json.NewDecoder(body).Decode(&p); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if !cidPattern.MatchString(p.CID) || p.Event != "pageview" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Origin / Referer must match an allowed gateway suffix when present.
	if !s.originAllowed(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}

	ua := r.Header.Get("User-Agent")
	if ua == "" || botPattern.MatchString(ua) {
		// Silently drop — bots inflate counts otherwise.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	ip := clientIP(r)
	if !s.limiter.allow(p.CID, ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	visitorHash, day := s.store.visitorHash(ip, ua)
	if err := s.store.recordPageview(p.CID, visitorHash, day, s.cfg.maxDistinctCIDs); err != nil {
		if errors.Is(err, errStoreFull) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		log.Printf("recordPageview error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	// Best-effort persistence; failure to flush isn't fatal for one event.
	go func() {
		if err := s.store.persist(); err != nil {
			log.Printf("persist error: %v", err)
		}
	}()
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !cidPattern.MatchString(cid) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	pv, uv, ok := s.store.stats(cid)
	if !ok {
		// Unknown CID = no pings yet. Surface zeroes so the app shows "0
		// views · 0 visitors" rather than "Analytics unavailable".
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"pageviews":      0,
			"uniqueVisitors": 0,
		})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]int64{
		"pageviews":      pv,
		"uniqueVisitors": uv,
	})
}

func (s *server) originAllowed(r *http.Request) bool {
	if len(s.cfg.allowedSuffixes) == 0 {
		return true
	}
	// Pick the first present header; allow either to satisfy. If neither is
	// present (e.g. a same-origin or no-referrer request), accept — we can't
	// distinguish a legit no-referrer ping from a forged one without it.
	candidates := []string{r.Header.Get("Origin"), r.Header.Get("Referer")}
	any := false
	for _, raw := range candidates {
		if raw == "" {
			continue
		}
		any = true
		host := raw
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			host = u.Host
		}
		for _, suf := range s.cfg.allowedSuffixes {
			if strings.HasSuffix(host, suf) {
				return true
			}
		}
	}
	return !any
}

func clientIP(r *http.Request) string {
	// Trust X-Forwarded-For only if the connection comes through a known
	// proxy; for simplicity here we take the first hop. In a real deploy
	// strip this to your trusted-proxy list.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.SplitN(xff, ",", 2)
		return strings.TrimSpace(parts[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	st, err := newStore(cfg.dataDir, cfg.dailySaltFile)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	srv := &server{
		cfg:     cfg,
		store:   st,
		limiter: newRateLimiter(cfg.rateLimitPerMin),
	}

	// Janitor: rotate daily salt at UTC midnight, prune old visitor sets, and
	// trim the rate-limiter map every cleanupInterval.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		lastDay := st.saltDate
		lastPrune := time.Now()
		for now := range t.C {
			today := now.UTC().Format("2006-01-02")
			if today != lastDay {
				if err := st.rotateSalt(today); err != nil {
					log.Printf("rotate salt: %v", err)
				} else {
					lastDay = today
				}
			}
			if now.Sub(lastPrune) >= cfg.cleanupInterval {
				st.pruneOldDays(cfg.uniqueVisitorTTL)
				srv.limiter.prune()
				if err := st.persist(); err != nil {
					log.Printf("janitor persist: %v", err)
				}
				lastPrune = now
			}
		}
	}()

	server := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("fxfiles-analytics listening on %s", cfg.listenAddr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func atoiOr(s string, def int) int {
	n, _ := strconv.Atoi(s)
	if n <= 0 {
		return def
	}
	return n
}
