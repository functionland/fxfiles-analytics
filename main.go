// fxfiles-analytics — minimal cookieless analytics for AI-generated static
// sites served from public IPFS gateways. See README.md for the design and
// API contract.
//
// Storage is PostgreSQL (pgx/pgxpool). The previous JSON-on-disk
// implementation has been retired — see `migrations/001_analytics.sql` for
// the schema. State that used to live in `tokens.json` and `.salt` now
// lives in `analytics_cids`, `analytics_visitors`, and `analytics_salt`.
package main

import (
	"context"
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
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	listenAddr       string
	pgDSN            string
	allowedSuffixes  []string
	rateLimitPerMin  int
	maxRequestBytes  int64
	uniqueVisitorTTL time.Duration
	cleanupInterval  time.Duration
	maxDistinctCIDs  int
	// CIDRs of upstream proxies we trust to set X-Forwarded-For / X-Real-IP.
	// For nginx-on-box the default `127.0.0.1/32, ::1/128` is correct; if
	// you front the service with a CDN, add its egress range here.
	trustedProxies []net.IPNet
}

func loadConfig() (*config, error) {
	c := &config{
		listenAddr:       getenv("LISTEN_ADDR", ":8080"),
		pgDSN:            os.Getenv("PG_DSN"),
		rateLimitPerMin:  atoiOr(getenv("RATE_LIMIT_PER_MIN", "60"), 60),
		maxRequestBytes:  4 * 1024,
		uniqueVisitorTTL: 30 * 24 * time.Hour, // keep 30 days of daily sets
		cleanupInterval:  6 * time.Hour,
		maxDistinctCIDs:  atoiOr(getenv("MAX_DISTINCT_CIDS", "100000"), 100000),
	}
	if c.pgDSN == "" {
		return nil, errors.New("PG_DSN is required (e.g. postgres://user:pass@127.0.0.1:5433/fxfiles_analytics?sslmode=disable)")
	}
	gateways := getenv("ALLOWED_GATEWAYS", ".ipfs.dweb.link,.ipfs.cloud.fx.land")
	for _, g := range strings.Split(gateways, ",") {
		g = strings.TrimSpace(g)
		if g != "" {
			c.allowedSuffixes = append(c.allowedSuffixes, g)
		}
	}
	proxiesEnv := getenv("TRUSTED_PROXIES", "127.0.0.1/32,::1/128")
	for _, p := range strings.Split(proxiesEnv, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		_, ipnet, err := net.ParseCIDR(p)
		if err != nil {
			return nil, fmt.Errorf("invalid TRUSTED_PROXIES entry %q: %w", p, err)
		}
		c.trustedProxies = append(c.trustedProxies, *ipnet)
	}
	return c, nil
}

// ---------------------------------------------------------------------------
// State: Postgres-backed counter store + per-day rotating salt
// ---------------------------------------------------------------------------

var errStoreFull = errors.New("store full: distinct CID cap exceeded")

// store wraps the connection pool and an in-memory copy of today's salt.
// The salt is cached because every pageview hashes against it; refetching
// from the DB on each call would be wasteful and would couple request
// latency to DB round-trip time.
type store struct {
	pool *pgxpool.Pool

	mu       sync.RWMutex
	salt     []byte
	saltDate string

	// Approximate count of distinct CIDs. Seeded from `SELECT COUNT(*)`
	// at startup and bumped after every new-CID insert. Used as a fast
	// pre-check before the per-CID existence query, so the cap check
	// doesn't have to do a full-table COUNT on every pageview.
	cidCount atomic.Int64
}

func newStore(ctx context.Context, dsn string) (*store, error) {
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse PG_DSN: %w", err)
	}
	// Conservative pool sizing — analytics is write-heavy with short txns.
	poolCfg.MaxConns = 16
	poolCfg.MinConns = 2
	poolCfg.MaxConnLifetime = 30 * time.Minute
	poolCfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("pgxpool new: %w", err)
	}

	// Retry the first ping for up to 10s so we tolerate slow Postgres
	// container starts when systemd brings us up alongside Docker. Kept
	// short (vs. 30s) so the systemd `StartLimitBurst` actually triggers
	// within `StartLimitIntervalSec` if the DB is genuinely down — fast
	// feedback beats a long quiet stall.
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := pool.Ping(ctx)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			pool.Close()
			return nil, fmt.Errorf("postgres unreachable after 30s: %w", err)
		}
		log.Printf("waiting for postgres: %v", err)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(time.Second):
		}
	}

	s := &store{pool: pool}
	if err := s.loadOrRotateSalt(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("load salt: %w", err)
	}

	var count int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM analytics_cids`).Scan(&count); err != nil {
		pool.Close()
		return nil, fmt.Errorf("seed cid count: %w", err)
	}
	s.cidCount.Store(count)
	log.Printf("store ready: %d distinct CIDs, salt date %s", count, s.saltDate)
	return s, nil
}

func (s *store) close() {
	s.pool.Close()
}

// loadOrRotateSalt reads today's salt from the DB, generating it if no
// row exists for today. Concurrent processes calling this at exactly
// midnight UTC are safe — `ON CONFLICT DO NOTHING` + re-read picks up
// whichever one won the race.
func (s *store) loadOrRotateSalt(ctx context.Context) error {
	today := time.Now().UTC().Format("2006-01-02")
	var salt []byte
	err := s.pool.QueryRow(ctx, `SELECT salt FROM analytics_salt WHERE day = $1`, today).Scan(&salt)
	if err == nil {
		s.mu.Lock()
		s.salt = salt
		s.saltDate = today
		s.mu.Unlock()
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	return s.rotateSalt(ctx, today)
}

func (s *store) rotateSalt(ctx context.Context, day string) error {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`INSERT INTO analytics_salt(day, salt) VALUES ($1, $2) ON CONFLICT (day) DO NOTHING`,
		day, buf); err != nil {
		return err
	}
	// Re-read so concurrent processes converge on the same canonical salt
	// (whichever insert won the ON CONFLICT race).
	var canonical []byte
	if err := s.pool.QueryRow(ctx,
		`SELECT salt FROM analytics_salt WHERE day = $1`, day).Scan(&canonical); err != nil {
		return err
	}
	s.mu.Lock()
	s.salt = canonical
	s.saltDate = day
	s.mu.Unlock()
	return nil
}

func (s *store) stats(ctx context.Context, cid string) (pv, uv int64, ok bool, err error) {
	err = s.pool.QueryRow(ctx,
		`SELECT pageviews FROM analytics_cids WHERE cid = $1`, cid).Scan(&pv)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	err = s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM analytics_visitors WHERE cid = $1`, cid).Scan(&uv)
	if err != nil {
		return 0, 0, false, err
	}
	return pv, uv, true, nil
}

// recordPageview increments the counter for `cid` and adds `visitorHash`
// to the per-day unique-visitor set. Returns `errStoreFull` when the
// per-instance CID cap is exceeded and the CID is new, so callers can
// return 503/429 instead of silently dropping. Existing records are
// always updated regardless of the cap.
func (s *store) recordPageview(ctx context.Context, cid, visitorHash, day string, capCIDs int) error {
	// Cheap pre-check: if our cached count is already at/above cap, do
	// the existence query first to decide whether to short-circuit.
	if capCIDs > 0 && s.cidCount.Load() >= int64(capCIDs) {
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM analytics_cids WHERE cid = $1)`, cid).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return errStoreFull
		}
	}

	// Upsert. `xmax = 0` discriminates a fresh insert from an
	// ON-CONFLICT update so we can bump the cached CID count.
	var inserted bool
	if err := s.pool.QueryRow(ctx, `
		INSERT INTO analytics_cids(cid, pageviews) VALUES ($1, 1)
		ON CONFLICT (cid) DO UPDATE
		   SET pageviews = analytics_cids.pageviews + 1,
		       updated_at = NOW()
		RETURNING (xmax = 0) AS inserted
	`, cid).Scan(&inserted); err != nil {
		return err
	}
	if inserted {
		s.cidCount.Add(1)
	}

	if _, err := s.pool.Exec(ctx, `
		INSERT INTO analytics_visitors(cid, day, visitor_hash) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING
	`, cid, day, visitorHash); err != nil {
		return err
	}
	return nil
}

// visitorHash returns sha256(salt || ip || ua) truncated to 16 hex chars
// alongside the salt date the hash is keyed against. Truncation is
// intentional — uniqueness within a single day is enough.
func (s *store) visitorHash(ip, ua string) (hash, day string) {
	s.mu.RLock()
	salt := s.salt
	d := s.saltDate
	s.mu.RUnlock()
	h := sha256.New()
	h.Write(salt)
	h.Write([]byte(ip))
	h.Write([]byte{0})
	h.Write([]byte(ua))
	return hex.EncodeToString(h.Sum(nil)[:8]), d
}

// pruneOldDays drops visitor rows older than `ttl` and old salt rows.
// Idempotent; safe to call on a schedule.
func (s *store) pruneOldDays(ctx context.Context, ttl time.Duration) error {
	cutoff := time.Now().UTC().Add(-ttl).Format("2006-01-02")
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM analytics_visitors WHERE day < $1`, cutoff); err != nil {
		return err
	}
	// Keep one extra week of salts for forensic ability to verify older
	// hashes if needed.
	saltCutoff := time.Now().UTC().Add(-ttl - 7*24*time.Hour).Format("2006-01-02")
	if _, err := s.pool.Exec(ctx,
		`DELETE FROM analytics_salt WHERE day < $1`, saltCutoff); err != nil {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Rate limiting: per (cid, ip) sliding 1-minute window — in-memory.
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
	cidPattern = regexp.MustCompile(`^(Qm[1-9A-HJ-NP-Za-km-z]{44}|baf[ykz][a-z0-9]{40,80})$`)
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
	mux.HandleFunc("GET /healthz", s.handleHealth)
	return mux
}

type trackPayload struct {
	CID   string `json:"cid"`
	Event string `json:"event"`
	Ref   string `json:"ref"`
}

func (s *server) handleTrack(w http.ResponseWriter, r *http.Request) {
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
	if !s.originAllowed(r) {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	ua := r.Header.Get("User-Agent")
	if ua == "" || botPattern.MatchString(ua) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	ip := s.clientIP(r)
	if !s.limiter.allow(p.CID, ip) {
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}

	hash, day := s.store.visitorHash(ip, ua)

	// Per-request DB timeout so a slow/stuck Postgres doesn't pile up
	// goroutines under load.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	if err := s.store.recordPageview(ctx, p.CID, hash, day, s.cfg.maxDistinctCIDs); err != nil {
		if errors.Is(err, errStoreFull) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		log.Printf("recordPageview error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) handleStats(w http.ResponseWriter, r *http.Request) {
	cid := r.PathValue("cid")
	if !cidPattern.MatchString(cid) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	pv, uv, ok, err := s.store.stats(ctx, cid)
	if err != nil {
		log.Printf("stats error: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if !ok {
		// Surface zeroes so the app shows "0 views · 0 visitors" rather
		// than "Analytics unavailable" for sites that haven't been
		// visited yet.
		_ = json.NewEncoder(w).Encode(map[string]int64{
			"pageviews":      0,
			"uniqueVisitors": 0,
		})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]int64{
		"pageviews":      pv,
		"uniqueVisitors": uv,
	})
}

func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	if err := s.store.pool.Ping(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, "db unavailable")
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

func (s *server) originAllowed(r *http.Request) bool {
	if len(s.cfg.allowedSuffixes) == 0 {
		return true
	}
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

// clientIP returns the originating client IP, honouring X-Forwarded-For
// ONLY when the immediate peer (r.RemoteAddr) is in the trusted-proxies
// CIDR list. This is the production-safe pattern: without it, any
// attacker can spoof their rate-limit identity by setting XFF themselves.
func (s *server) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remoteIP := net.ParseIP(host)
	if remoteIP == nil {
		return host
	}
	trusted := false
	for _, cidr := range s.cfg.trustedProxies {
		if cidr.Contains(remoteIP) {
			trusted = true
			break
		}
	}
	if !trusted {
		return host
	}
	// Trusted peer — accept their XFF / X-Real-IP. We take the first
	// (leftmost) entry, which by convention is the original client IP
	// closest to the user. nginx (as configured by our installer) only
	// emits one value here.
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		for _, p := range strings.Split(xff, ",") {
			ip := strings.TrimSpace(p)
			if ip != "" {
				return ip
			}
		}
	}
	if xri := r.Header.Get("X-Real-IP"); xri != "" {
		return strings.TrimSpace(xri)
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

	rootCtx, cancelRoot := signal.NotifyContext(context.Background(),
		syscall.SIGINT, syscall.SIGTERM)
	defer cancelRoot()

	st, err := newStore(rootCtx, cfg.pgDSN)
	if err != nil {
		log.Fatalf("store: %v", err)
	}
	defer st.close()

	srv := &server{
		cfg:     cfg,
		store:   st,
		limiter: newRateLimiter(cfg.rateLimitPerMin),
	}

	// Janitor: rotate daily salt at UTC midnight, prune old visitor rows,
	// and trim the in-memory rate-limiter map every cleanupInterval.
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		lastDay := st.saltDate
		lastPrune := time.Now()
		for {
			select {
			case <-rootCtx.Done():
				return
			case now := <-t.C:
				today := now.UTC().Format("2006-01-02")
				if today != lastDay {
					if err := st.rotateSalt(rootCtx, today); err != nil {
						log.Printf("rotate salt: %v", err)
					} else {
						lastDay = today
					}
				}
				if now.Sub(lastPrune) >= cfg.cleanupInterval {
					if err := st.pruneOldDays(rootCtx, cfg.uniqueVisitorTTL); err != nil {
						log.Printf("prune: %v", err)
					}
					srv.limiter.prune()
					lastPrune = now
				}
			}
		}
	}()

	httpSrv := &http.Server{
		Addr:              cfg.listenAddr,
		Handler:           srv.routes(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       30 * time.Second,
	}

	// Graceful shutdown: stop accepting new connections, wait up to 10s
	// for in-flight to finish.
	go func() {
		<-rootCtx.Done()
		log.Printf("shutdown signal received")
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := httpSrv.Shutdown(shCtx); err != nil {
			log.Printf("graceful shutdown: %v", err)
		}
	}()

	log.Printf("fxfiles-analytics listening on %s", cfg.listenAddr)
	if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	log.Printf("server stopped cleanly")
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
