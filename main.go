package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/redis/go-redis/v9"
)

type Config struct {
	ListenAddr    string
	Zone          string
	ClusterList   []string
	Upstreams     []string
	RedisAddr     string
	RedisPassword string
	RedisDB       int

	CacheMaxTTL   time.Duration
	NegCacheTTL   time.Duration
	QueryTimeout  time.Duration
	AllowPassthru bool
	LogDecisions  bool

	TLSUpstream   bool
	TLSServerName string
}

type Server struct {
	cfg   Config
	rdb   *redis.Client
	uc    *dns.Client
	uctls *dns.Client
}

func envOr(key, def string) string {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	return v
}

func mustDuration(key string, def time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Fatalf("invalid duration %s=%q: %v", key, v, err)
	}
	return d
}

func mustBool(key string, def bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	v = strings.ToLower(v)
	return v == "1" || v == "true" || v == "yes" || v == "y"
}

func mustInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	var x int
	_, err := fmt.Sscanf(v, "%d", &x)
	if err != nil {
		log.Fatalf("invalid int %s=%q: %v", key, v, err)
	}
	return x
}

func parseCSVAddrs(s string) []string {
	parts := strings.Split(s, ",")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if !strings.Contains(p, ":") {
			p = p + ":53"
		}
		out = append(out, p)
	}
	return out
}

func parseClusterListEnv() []string {
	raw := strings.TrimSpace(os.Getenv("CLUSTER_LIST"))
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		if !seen[p] {
			out = append(out, p)
			seen[p] = true
		}
	}
	return out
}

func loadConfig() Config {
	zone := envOr("ZONE", "corp.com.")
	if !strings.HasSuffix(zone, ".") {
		zone += "."
	}

	return Config{
		ListenAddr:    envOr("LISTEN_ADDR", ":1053"),
		Zone:          zone,
		ClusterList:   parseClusterListEnv(),
		Upstreams:     parseCSVAddrs(envOr("UPSTREAMS", "10.0.0.10:53")),
		RedisAddr:     envOr("REDIS_ADDR", "redis-dns-cache:6379"),
		RedisPassword: envOr("REDIS_PASSWORD", ""),
		RedisDB:       mustInt("REDIS_DB", 0),

		CacheMaxTTL:   mustDuration("CACHE_MAX_TTL", 60*time.Second),
		NegCacheTTL:   mustDuration("NEG_CACHE_TTL", 15*time.Second),
		QueryTimeout:  mustDuration("QUERY_TIMEOUT", 900*time.Millisecond),
		AllowPassthru: mustBool("ALLOW_PASSTHRU", true),
		LogDecisions:  mustBool("LOG_DECISIONS", true),

		TLSUpstream:   mustBool("TLS_UPSTREAM", false),
		TLSServerName: envOr("TLS_SERVER_NAME", ""),
	}
}

func newServer(cfg Config) *Server {
	rdb := redis.NewClient(&redis.Options{
		Addr:     cfg.RedisAddr,
		Password: cfg.RedisPassword,
		DB:       cfg.RedisDB,
	})

	uc := &dns.Client{
		Net:          "udp",
		Timeout:      cfg.QueryTimeout,
		ReadTimeout:  cfg.QueryTimeout,
		WriteTimeout: cfg.QueryTimeout,
	}
	uctls := &dns.Client{
		Net:          "tcp-tls",
		Timeout:      cfg.QueryTimeout,
		ReadTimeout:  cfg.QueryTimeout,
		WriteTimeout: cfg.QueryTimeout,
		TLSConfig:    &tls.Config{ServerName: cfg.TLSServerName},
	}

	return &Server{cfg: cfg, rdb: rdb, uc: uc, uctls: uctls}
}

func (s *Server) cacheKey(q dns.Question) string {
	return fmt.Sprintf("dns:%s:%d:%d", strings.ToLower(q.Name), q.Qtype, q.Qclass)
}

func (s *Server) getCached(ctx context.Context, q dns.Question) (*dns.Msg, bool) {
	key := s.cacheKey(q)
	b, err := s.rdb.Get(ctx, key).Bytes()
	if err != nil {
		return nil, false
	}
	m := new(dns.Msg)
	if err := m.Unpack(b); err != nil {
		return nil, false
	}
	return m, true
}

func minTTLFromMsg(m *dns.Msg, capTTL time.Duration) time.Duration {
	ttl := uint32(0)
	set := false
	for _, rr := range append(append([]dns.RR{}, m.Answer...), m.Ns...) {
		h := rr.Header()
		if h == nil {
			continue
		}
		if !set || h.Ttl < ttl {
			ttl = h.Ttl
			set = true
		}
	}
	if !set || ttl == 0 {
		return 0
	}
	d := time.Duration(ttl) * time.Second
	if capTTL > 0 && d > capTTL {
		return capTTL
	}
	return d
}

func (s *Server) setCached(ctx context.Context, q dns.Question, m *dns.Msg, ttl time.Duration) {
	if ttl <= 0 {
		return
	}
	key := s.cacheKey(q)
	wire, err := m.Pack()
	if err != nil {
		return
	}
	_ = s.rdb.Set(ctx, key, wire, ttl).Err()
}

func (s *Server) upstreamExchange(ctx context.Context, req *dns.Msg) (*dns.Msg, string, error) {
	var lastErr error
	for _, up := range s.cfg.Upstreams {
		var r *dns.Msg
		var err error

		if s.cfg.TLSUpstream {
			r, _, err = s.uctls.ExchangeContext(ctx, req, up)
		} else {
			r, _, err = s.uc.ExchangeContext(ctx, req, up)
			if err == nil && r != nil && r.Truncated {
				c := &dns.Client{Net: "tcp", Timeout: s.cfg.QueryTimeout}
				r, _, err = c.ExchangeContext(ctx, req, up)
			}
		}

		if err == nil && r != nil {
			return r, up, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no upstream response")
	}
	return nil, "", lastErr
}

func isInZone(qname, zone string) bool {
	qname = strings.ToLower(dns.Fqdn(qname))
	zone = strings.ToLower(dns.Fqdn(zone))
	return strings.HasSuffix(qname, zone)
}

func isAlreadyPrefixed(qname, zone string) bool {
	qname = dns.Fqdn(qname)
	labels := dns.SplitDomainName(qname)
	zlabels := dns.SplitDomainName(dns.Fqdn(zone))
	return len(labels) >= len(zlabels)+2
}

func addPrefix(prefix string, base string) string {
	base = dns.Fqdn(base)
	return dns.Fqdn(prefix + "." + strings.TrimSuffix(base, "."))
}

func (s *Server) firstAnswerForCandidates(ctx context.Context, q dns.Question, candidates []string) (*dns.Msg, string, string, error) {
	for _, name := range candidates {
		req := new(dns.Msg)
		req.SetQuestion(dns.Fqdn(name), q.Qtype)
		req.Question[0].Qclass = q.Qclass

		resp, up, err := s.upstreamExchange(ctx, req)
		if err != nil || resp == nil {
			continue
		}
		if resp.Rcode == dns.RcodeSuccess && len(resp.Answer) > 0 {
			return resp, up, name, nil
		}
		// NXDOMAIN / empty -> try next
	}
	return nil, "", "", fmt.Errorf("no candidates resolved")
}

func (s *Server) replySERVFAIL(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Rcode = dns.RcodeServerFailure
	_ = w.WriteMsg(m)
}

func (s *Server) handleDNS(w dns.ResponseWriter, r *dns.Msg) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*s.cfg.QueryTimeout)
	defer cancel()

	if len(r.Question) == 0 {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeFormatError
		_ = w.WriteMsg(m)
		return
	}

	q := r.Question[0]
	qname := dns.Fqdn(q.Name)

	// Cache by exact question
	if cached, ok := s.getCached(ctx, q); ok {
		cached.Id = r.Id
		_ = w.WriteMsg(cached)
		return
	}

	// Not our zone => passthru
	if !isInZone(qname, s.cfg.Zone) {
		resp, _, err := s.upstreamExchange(ctx, r)
		if err != nil || resp == nil {
			s.replySERVFAIL(w, r)
			return
		}
		resp.Id = r.Id
		ttl := minTTLFromMsg(resp, s.cfg.CacheMaxTTL)
		if ttl == 0 && (resp.Rcode != dns.RcodeSuccess || len(resp.Answer) == 0) {
			ttl = s.cfg.NegCacheTTL
		}
		s.setCached(ctx, q, resp, ttl)
		_ = w.WriteMsg(resp)
		return
	}

	// Already prefixed => passthru
	if isAlreadyPrefixed(qname, s.cfg.Zone) {
		resp, _, err := s.upstreamExchange(ctx, r)
		if err != nil || resp == nil {
			s.replySERVFAIL(w, r)
			return
		}
		resp.Id = r.Id
		ttl := minTTLFromMsg(resp, s.cfg.CacheMaxTTL)
		if ttl == 0 && (resp.Rcode != dns.RcodeSuccess || len(resp.Answer) == 0) {
			ttl = s.cfg.NegCacheTTL
		}
		s.setCached(ctx, q, resp, ttl)
		_ = w.WriteMsg(resp)
		return
	}

	// Canonical name inside zone => try prefixed candidates, THEN canonical as last fallback
	candidates := make([]string, 0, len(s.cfg.ClusterList)+1)
	for _, c := range s.cfg.ClusterList {
		candidates = append(candidates, addPrefix(c, qname))
	}
	// Legacy compatibility: ALWAYS try the unprefixed name last
	candidates = append(candidates, qname)

	resp, usedUp, chosen, err := s.firstAnswerForCandidates(ctx, q, candidates)
	if err != nil || resp == nil {
		if s.cfg.AllowPassthru {
			fb, _, uerr := s.upstreamExchange(ctx, r)
			if uerr == nil && fb != nil {
				fb.Id = r.Id
				ttl := minTTLFromMsg(fb, s.cfg.CacheMaxTTL)
				if ttl == 0 && (fb.Rcode != dns.RcodeSuccess || len(fb.Answer) == 0) {
					ttl = s.cfg.NegCacheTTL
				}
				s.setCached(ctx, q, fb, ttl)
				_ = w.WriteMsg(fb)
				return
			}
		}
		neg := new(dns.Msg)
		neg.SetReply(r)
		neg.Rcode = dns.RcodeNameError
		s.setCached(ctx, q, neg, s.cfg.NegCacheTTL)
		_ = w.WriteMsg(neg)
		return
	}

	if s.cfg.LogDecisions {
		log.Printf("policy %s %s -> chosen=%s order=%v upstream=%s",
			qname, dns.TypeToString[q.Qtype], chosen, candidates, usedUp)
	}

	resp.Id = r.Id
	ttl := minTTLFromMsg(resp, s.cfg.CacheMaxTTL)
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	s.setCached(ctx, q, resp, ttl)
	_ = w.WriteMsg(resp)
}

func (s *Server) Run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("redis ping failed: %w", err)
	}

	mux := dns.NewServeMux()
	mux.HandleFunc(".", s.handleDNS)

	udp := &dns.Server{Addr: s.cfg.ListenAddr, Net: "udp", Handler: mux}
	tcp := &dns.Server{Addr: s.cfg.ListenAddr, Net: "tcp", Handler: mux}

	errCh := make(chan error, 2)
	go func() { errCh <- udp.ListenAndServe() }()
	go func() { errCh <- tcp.ListenAndServe() }()

	log.Printf("dns-policy-router listening=%s zone=%s clusters=%v upstreams=%v redis=%s",
		s.cfg.ListenAddr, s.cfg.Zone, s.cfg.ClusterList, s.cfg.Upstreams, s.cfg.RedisAddr)

	err := <-errCh
	_ = udp.Shutdown()
	_ = tcp.Shutdown()
	return err
}

func main() {
	cfg := loadConfig()

	if len(cfg.Upstreams) == 0 {
		log.Fatal("UPSTREAMS must not be empty")
	}
	if _, _, err := net.SplitHostPort(cfg.ListenAddr); err != nil {
		log.Fatalf("invalid LISTEN_ADDR %q: %v", cfg.ListenAddr, err)
	}

	if len(cfg.ClusterList) == 0 {
		log.Printf("warning: CLUSTER_LIST is empty; will still try canonical fallback for names in %s", cfg.Zone)
	}

	s := newServer(cfg)
	if err := s.Run(); err != nil {
		log.Fatalf("server error: %v", err)
	}
}
