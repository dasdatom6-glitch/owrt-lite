package server

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"owrt-lite/internal/node"
	"owrt-lite/internal/sysproxy"
	"owrt-lite/internal/ui"
	"owrt-lite/internal/util"
	"owrt-lite/internal/xproxy"
)

const (
	nodesStateFile = "nodes_state.json"
	settingsFile   = "settings.json"
)

type AppSettings struct {
	AutoSystemProxy  bool   `json:"auto_system_proxy"`
	StartupProxyMode string `json:"startup_proxy_mode"`
	AutoSaveNodes    bool   `json:"auto_save_nodes"`
	Password         string `json:"password,omitempty"`
}

func defaultSettings() AppSettings {
	return AppSettings{
		AutoSystemProxy:  true,
		StartupProxyMode: "rule",
		AutoSaveNodes:    false,
	}
}

func normalizeProxyMode(mode string) string {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "off", "global", "rule", "direct":
		return mode
	default:
		return "global"
	}
}

func loadSettings() AppSettings {
	st := defaultSettings()
	if data, err := os.ReadFile(settingsFile); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	st.StartupProxyMode = normalizeProxyMode(st.StartupProxyMode)
	if st.StartupProxyMode == "off" || st.StartupProxyMode == "direct" {
		st.AutoSystemProxy = false
	}
	return st
}

func saveSettings(st AppSettings) error {
	st.StartupProxyMode = normalizeProxyMode(st.StartupProxyMode)
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := settingsFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, settingsFile)
}

type Server struct {
	mux          *http.ServeMux
	inflight     chan struct{}
	reqs         atomic.Uint64
	pool         sync.Pool
	nm           *node.Manager
	proxy        *xproxy.Reverse
	password     string
	sessionToken string
	listenAddr   string
	onShutdown   func()

	// Proxy Mode: "off", "global", "rule"
	proxyMode string
	settings  AppSettings
	rules     struct {
		Direct []string `json:"direct"`
		Proxy  []string `json:"proxy"`
	}
	mu sync.RWMutex
}

func New(maxInflight int, nm *node.Manager, listenAddr string) *Server {
	settings := loadSettings()
	password := defaultPassword()
	if settings.Password != "" {
		password = settings.Password
	}
	initialMode := "off"
	if settings.AutoSystemProxy {
		initialMode = normalizeProxyMode(settings.StartupProxyMode)
		if initialMode == "off" || initialMode == "direct" {
			initialMode = "global"
		}
	}
	s := &Server{
		mux:          http.NewServeMux(),
		nm:           nm,
		password:     password,
		sessionToken: generateRandomPassword(),
		proxyMode:    initialMode,
		settings:     settings,
		listenAddr:   listenAddr,
		pool: sync.Pool{
			New: func() any { return make([]byte, 32*1024) },
		},
	}
	// Load rules
	if data, err := os.ReadFile("rules.json"); err == nil {
		json.Unmarshal(data, &s.rules)
	}
	// Provide sane defaults if no rules loaded
	if len(s.rules.Direct) == 0 && len(s.rules.Proxy) == 0 {
		s.rules.Direct = []string{
			".cn", ".com.cn", ".net.cn", ".org.cn", ".gov.cn",
			"localhost", "lan", ".local",
			".baidu.com", ".qq.com", ".bilibili.com", ".jd.com", ".taobao.com", ".tmall.com",
		}
		s.rules.Proxy = []string{
			".google.com", ".gstatic.com", ".googleapis.com", ".youtube.com", ".ytimg.com",
			".facebook.com", ".fbcdn.net", ".instagram.com", ".whatsapp.com",
			".twitter.com", ".t.co", ".x.com", ".twimg.com",
			".wikipedia.org", ".github.com", ".githubusercontent.com", ".gitlab.com",
			".spotify.com", ".netflix.com", ".openai.com", ".telegram.org", ".t.me",
		}
	}

	if maxInflight > 0 {
		s.inflight = make(chan struct{}, maxInflight)
	}
	s.proxy = xproxy.New(nm)
	if nm != nil {
		nm.SetRouteChangeHook(func(ids []string) {
			closed := s.proxy.CloseActiveConnections()
			if closed > 0 {
				nm.AddLog(fmt.Sprintf("RouteChanged: closed %d old tunnel(s); new route=%v", closed, ids))
			}
		})
	}

	// Like Clash: optionally enable OS system proxy automatically on startup.
	go func() {
		if s.settings.AutoSystemProxy {
			if err := sysproxy.SetProxy(s.proxyListenAddr()); err != nil {
				if s.nm != nil {
					s.nm.AddLog(fmt.Sprintf("Auto system proxy failed: %v", err))
				}
			} else if s.nm != nil {
				s.nm.AddLog(fmt.Sprintf("Auto system proxy enabled: %s (%s)", s.proxyListenAddr(), initialMode))
			}
		} else {
			_ = sysproxy.ClearProxy()
		}
	}()

	s.routes()
	return s
}

// SetShutdownCallback registers a function to run when the server receives a shutdown request.
func (s *Server) SetShutdownCallback(fn func()) {
	s.onShutdown = fn
}

func (s *Server) proxyListenAddr() string {
	addr := s.listenAddr
	if addr == "" {
		return "127.0.0.1:8080"
	}
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return addr
}

func (s *Server) Handler() http.Handler {
	h := s.mux
	if s.inflight != nil {
		h = http.NewServeMux()
		h.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
			s.inflight <- struct{}{}
			defer func() { <-s.inflight }()
			s.mux.ServeHTTP(w, r)
		})
	}

	// Add recovery middleware
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC recovered: %v\n", err)
				w.WriteHeader(http.StatusInternalServerError)
			}
		}()

		// Check for Proxy Request (CONNECT or Absolute URL)
		if r.Method == http.MethodConnect || r.URL.IsAbs() {
			s.reqs.Add(1)
			s.mu.RLock()
			mode := s.proxyMode
			s.mu.RUnlock()

			// Get target host
			host := r.URL.Hostname()
			if host == "" {
				host = r.Host
				if h, _, err := net.SplitHostPort(host); err == nil {
					host = h
				}
			}

			// Log the proxy request attempt
			if s.nm != nil {
				nodes := s.nm.Chain()
				routeDesc := "none"
				if len(nodes) > 0 {
					routeDesc = fmt.Sprintf("%s@%s", nodes[0].ID, nodes[0].URL.Host)
					if len(nodes) > 1 {
						routeDesc += fmt.Sprintf(" (+%d hops)", len(nodes)-1)
					}
				}
				fmt.Printf("[%s] ProxyReq: %s %s [Route: %s]\n", mode, r.Method, host, routeDesc)
				s.nm.AddLog(fmt.Sprintf("ProxyReq: %s [%s] -> %s (Route: %s)", r.Method, mode, host, routeDesc))
			}

			// Direct Mode (New)
			if mode == "direct" {
				if r.Method == http.MethodConnect {
					s.proxy.ServeDirect(w, r)
				} else {
					s.proxy.ServeProxy(w, r, nil)
				}
				return
			}

			// Rule Mode Logic: Bypass LAN and CN domains (with forced-proxy override)
			if mode == "rule" {
				// Bypass logic
				isLocal := util.IsPrivateIP(host) || host == "localhost" || host == "127.0.0.1"

				// Match built-in rules
				matchDirect := false
				s.mu.RLock()
				for _, d := range s.rules.Direct {
					if strings.HasSuffix(host, d) {
						matchDirect = true
						break
					}
				}
				s.mu.RUnlock()

				// Optional: Force proxy for specific domains even if they look like CN
				matchProxy := false
				s.mu.RLock()
				for _, p := range s.rules.Proxy {
					if strings.HasSuffix(host, p) {
						matchProxy = true
						break
					}
				}
				s.mu.RUnlock()
				if matchProxy && s.nm != nil {
					s.nm.AddLog(fmt.Sprintf("Rule Proxy (Forced): %s", host))
				}

				// Basic CN domain suffixes
				cnSuffixes := []string{".cn", ".com.cn", ".net.cn", ".org.cn", ".gov.cn"}
				isCN := false
				for _, s := range cnSuffixes {
					if strings.HasSuffix(host, s) {
						isCN = true
						break
					}
				}

				// Direct only when not forced to proxy
				if !matchProxy && (isLocal || isCN || matchDirect) {
					// Direct connection
					if s.nm != nil {
						s.nm.AddLog(fmt.Sprintf("Rule Direct: %s", host))
					}
					if r.Method == http.MethodConnect {
						s.proxy.ServeDirect(w, r)
					} else {
						s.proxy.ServeProxy(w, r, nil)
					}
					return
				}
			}

			if s.nm != nil {
				// Get chain URLs
				var chain []*url.URL
				nodes := s.nm.Chain()
				for _, n := range nodes {
					chain = append(chain, n.URL)
				}
				if len(chain) > 0 {
					s.proxy.ServeProxy(w, r, chain)
					return
				}
			}

			// No chain available:
			// - In GLOBAL mode, do NOT fallback to direct. Return 502 to enforce proxy semantics.
			// - In RULE mode, fallback to direct to avoid connectivity loss for unmatched cases.
			// - In DIRECT mode, this path won't be reached (handled earlier).
			if mode == "global" {
				if s.nm != nil {
					s.nm.AddLog(fmt.Sprintf("GlobalFallback: No Active Node -> %s", host))
				}
				http.Error(w, "Proxy unavailable: no active node selected", http.StatusBadGateway)
				return
			}
			// RULE or other modes: fallback to direct
			if s.nm != nil {
				s.nm.AddLog(fmt.Sprintf("ProxyFallback: Direct (No Active Node) -> %s", host))
			}
			if r.Method == http.MethodConnect {
				s.proxy.ServeDirect(w, r)
			} else {
				s.proxy.ServeProxy(w, r, nil)
			}
			return
		}

		h.ServeHTTP(w, r)
	})
}

func (s *Server) setSystemProxyMode(mode string) error {
	mode = normalizeProxyMode(mode)
	s.mu.Lock()
	s.proxyMode = mode
	s.settings.StartupProxyMode = mode
	s.settings.AutoSystemProxy = mode != "off" && mode != "direct"
	settings := s.settings
	s.mu.Unlock()

	var err error
	if mode == "off" || mode == "direct" {
		err = sysproxy.ClearProxy()
	} else {
		err = sysproxy.SetProxy(s.proxyListenAddr())
	}
	if err != nil {
		return err
	}
	return saveSettings(settings)
}

func (s *Server) autoSaveNodes() {
	s.mu.RLock()
	auto := s.settings.AutoSaveNodes
	s.mu.RUnlock()
	if auto && s.nm != nil {
		go func() { _ = s.nm.SaveState(nodesStateFile) }()
	}
}

func (s *Server) routes() {
	// Auth middleware
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			c, err := r.Cookie("auth")
			s.mu.RLock()
			token := s.sessionToken
			s.mu.RUnlock()
			if err != nil || c.Value == "" || c.Value != token {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			next(w, r)
		}
	}

	s.mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var cred struct {
			Password string `json:"password"`
		}
		if json.NewDecoder(r.Body).Decode(&cred) == nil {
			s.mu.RLock()
			match := cred.Password == s.password
			s.mu.RUnlock()
			if match {
				s.mu.RLock()
				token := s.sessionToken
				s.mu.RUnlock()
				http.SetCookie(w, &http.Cookie{Name: "auth", Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusUnauthorized)
	})

	s.mux.HandleFunc("/logout", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "auth", Value: "", Path: "/", MaxAge: -1, Expires: time.Unix(0, 0), HttpOnly: true, SameSite: http.SameSiteLaxMode})
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/api/password", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Password string `json:"password"`
		}
		if json.NewDecoder(r.Body).Decode(&req) == nil && req.Password != "" {
			s.mu.Lock()
			s.password = req.Password
			s.settings.Password = req.Password
			settings := s.settings
			s.mu.Unlock()
			if err := saveSettings(settings); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusBadRequest)
		}
	}))

	s.mux.HandleFunc("/api/settings", auth(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			s.mu.RLock()
			settings := s.settings
			mode := s.proxyMode
			s.mu.RUnlock()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(struct {
				AppSettings
				ProxyMode string `json:"proxy_mode"`
			}{AppSettings: settings, ProxyMode: mode})
		case http.MethodPost:
			var req AppSettings
			if json.NewDecoder(r.Body).Decode(&req) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			req.StartupProxyMode = normalizeProxyMode(req.StartupProxyMode)
			s.mu.Lock()
			if req.Password == "" {
				req.Password = s.settings.Password
			}
			s.settings.AutoSaveNodes = req.AutoSaveNodes
			s.settings.AutoSystemProxy = req.AutoSystemProxy
			s.settings.StartupProxyMode = req.StartupProxyMode
			s.settings.Password = req.Password
			settings := s.settings
			s.mu.Unlock()
			if err := saveSettings(settings); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	s.mux.HandleFunc("/node/rename", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.NewDecoder(r.Body).Decode(&req) == nil && req.ID != "" {
			if s.nm.Rename(req.ID, req.Name) {
				s.autoSaveNodes()
				w.WriteHeader(http.StatusOK)
				return
			}
		}
		w.WriteHeader(http.StatusBadRequest)
	}))

	s.mux.Handle("/ui/", http.StripPrefix("/ui/", ui.Handler()))
	s.mux.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusMovedPermanently)
	})
	s.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/ui/", http.StatusFound)
	})
	s.mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	s.mux.HandleFunc("/metrics", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("requests=" + strconv.FormatUint(s.reqs.Load(), 10)))
	}))
	s.mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		s.reqs.Add(1)
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.Body == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		buf := s.pool.Get().([]byte)
		defer s.pool.Put(buf)
		_, _ = io.CopyBuffer(w, r.Body, buf)
	})

	s.mux.HandleFunc("/proxy", func(w http.ResponseWriter, r *http.Request) {
		// Use Chain instead of Active
		chain := []*url.URL{}
		if s.nm != nil {
			nodes := s.nm.Chain()
			for _, n := range nodes {
				chain = append(chain, n.URL)
			}
		}

		if len(chain) == 0 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		s.proxy.ServeChain(w, r, chain)
	})

	s.mux.HandleFunc("/node/list", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		stats := s.nm.GetGlobalStats()
		stats["total_requests"] = int64(s.reqs.Load())

		s.mu.RLock()
		mode := s.proxyMode
		settings := s.settings
		s.mu.RUnlock()

		_ = json.NewEncoder(w).Encode(struct {
			Active    string           `json:"active"`
			Nodes     []node.NodeJSON  `json:"nodes"`
			Status    map[string]any   `json:"status"`
			Stats     map[string]int64 `json:"stats"`
			Logs      []string         `json:"logs"`
			ProxyMode string           `json:"proxy_mode"`
			Settings  AppSettings      `json:"settings"`
		}{
			Active: func() string {
				if a := s.nm.Active(); a != nil {
					return a.ID
				}
				return ""
			}(),
			Nodes:     s.nm.List(),
			Status:    s.nm.GetRotationStatus(),
			Stats:     stats,
			Logs:      s.nm.GetLogs(),
			ProxyMode: mode,
			Settings:  settings,
		})
	}))

	s.mux.HandleFunc("/system/proxy", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Mode string `json:"mode"` // "off", "global", "rule"
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		mode := normalizeProxyMode(req.Mode)
		if err := s.setSystemProxyMode(mode); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/system/proxy/reapply", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.mu.RLock()
		mode := s.proxyMode
		s.mu.RUnlock()
		if mode == "off" || mode == "direct" {
			mode = "rule"
		}
		if err := s.setSystemProxyMode(mode); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Diagnose: request external IP via this proxy (server-side through local forward proxy)
	s.mux.HandleFunc("/diagnose/ip", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		type result struct {
			IP      string `json:"ip"`
			Service string `json:"service"`
		}
		// Use our own proxy to ensure path goes through Handler and chain
		purl, _ := url.Parse("http://" + s.proxyListenAddr())
		tr := &http.Transport{
			Proxy:                 http.ProxyURL(purl),
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: 8 * time.Second,
			ForceAttemptHTTP2:     true,
			DisableKeepAlives:     true,
		}
		client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

		try := func(u string, key string) (string, bool) {
			resp, err := client.Get(u)
			if err != nil {
				return "", false
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return "", false
			}
			var buf struct {
				IP string `json:"ip"`
			}
			data, _ := io.ReadAll(resp.Body)
			// some services return plain text IP if no format param; handle both
			if json.Unmarshal(data, &buf) == nil && buf.IP != "" {
				return buf.IP, true
			}
			// plain text fallback
			return strings.TrimSpace(string(data)), true
		}

		if ip, ok := try("https://api.ipify.org?format=json", "ipify"); ok && ip != "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result{IP: ip, Service: "ipify"})
			return
		}
		if ip, ok := try("https://ipinfo.io/ip", "ipinfo"); ok && ip != "" {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(result{IP: ip, Service: "ipinfo"})
			return
		}
		http.Error(w, "cannot fetch ip via proxy", http.StatusBadGateway)
	}))

	s.mux.HandleFunc("/diagnose/direct-ip", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		client := &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 10 * time.Second}
		resp, err := client.Get("https://api.ipify.org?format=json")
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.Copy(w, resp.Body)
	}))

	s.mux.HandleFunc("/node/active", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost {
			id := ""
			if values, ok := r.URL.Query()["id"]; ok {
				id = values[0]
			} else {
				var req struct {
					ID string `json:"id"`
				}
				if json.NewDecoder(r.Body).Decode(&req) == nil {
					id = req.ID
				}
			}
			if !s.nm.SetActiveSingle(id) && id != "" {
				http.Error(w, "node not found", http.StatusNotFound)
				return
			}
		}
		// Return current active and chain info
		active := s.nm.Active()
		chain := s.nm.GetChainIDs()

		res := struct {
			ActiveID string   `json:"active_id"`
			ChainIDs []string `json:"chain_ids"`
		}{
			ChainIDs: chain,
		}
		if active != nil {
			res.ActiveID = active.ID
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(res)
	}))

	s.mux.HandleFunc("/node/chain", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Method == http.MethodPost {
			var ids []string
			if err := json.NewDecoder(r.Body).Decode(&ids); err == nil {
				s.nm.SetChain(ids)
				s.autoSaveNodes()
			}
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusMethodNotAllowed)
	}))

	s.mux.HandleFunc("/node/chain/saved", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		switch r.Method {
		case http.MethodGet:
			chains := s.nm.GetSavedChains()
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(chains)
		case http.MethodPost:
			var req struct {
				Name    string   `json:"name"`
				NodeIDs []string `json:"node_ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			id := s.nm.AddSavedChain(req.Name, req.NodeIDs)
			s.autoSaveNodes()
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(id))
		case http.MethodDelete:
			id := r.URL.Query().Get("id")
			if id == "" {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if s.nm.DeleteSavedChain(id) {
				s.autoSaveNodes()
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	s.mux.HandleFunc("/node/next", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		n := s.nm.Next()
		if n == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(n.ID))
	}))

	s.mux.HandleFunc("/node/import", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		ct := r.Header.Get("Content-Type")
		added := 0
		if ct == "application/json" {
			added = s.nm.ImportJSON(r.Body)
		} else {
			// ImportText now supports YAML detection
			added = s.nm.ImportText(r.Body)
		}
		if added > 0 {
			s.autoSaveNodes()
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strconv.Itoa(added)))
	}))

	s.mux.HandleFunc("/node/export", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		data := s.nm.ExportJSON()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", "attachment; filename=nodes.json")
		w.Write(data)
	}))

	s.mux.HandleFunc("/node/save", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch r.Method {
		case http.MethodPost:
			if err := s.nm.SaveState(nodesStateFile); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		case http.MethodDelete:
			if err := os.Remove(nodesStateFile); err != nil && !os.IsNotExist(err) {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			if s.nm != nil {
				s.nm.AddLog("Removed saved node state")
			}
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))

	s.mux.HandleFunc("/node/load", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		if err := s.nm.LoadState(nodesStateFile); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/node/update", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var p struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Scheme   string `json:"scheme"`
			Protocol string `json:"protocol"`
			Host     string `json:"host"`
			Port     string `json:"port"`
			Password string `json:"password"`
			Cipher   string `json:"cipher"`
		}
		if json.NewDecoder(r.Body).Decode(&p) != nil || p.ID == "" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if p.Scheme == "" {
			p.Scheme = p.Protocol
		}
		if s.nm.Update(p.ID, p.Name, p.Scheme, p.Host, p.Port, p.Password, p.Cipher) {
			s.autoSaveNodes()
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusNotFound)
		}
	}))

	s.mux.HandleFunc("/node/add/manual", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var p struct {
			Name     string `json:"name"`
			Scheme   string `json:"scheme"`
			Protocol string `json:"protocol"`
			Host     string `json:"host"`
			Port     string `json:"port"`
			Password string `json:"password"`
			Cipher   string `json:"cipher"`
		}
		if json.NewDecoder(r.Body).Decode(&p) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if p.Scheme == "" {
			p.Scheme = p.Protocol
		}
		if s.nm.AddManual(p.Name, p.Scheme, p.Host, p.Port, p.Password, p.Cipher) {
			s.autoSaveNodes()
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusConflict)
		}
	}))

	s.mux.HandleFunc("/node/add", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		u := r.URL.Query().Get("url")
		if u == "" {
			u = string(util.ReadAllSmall(r.Body))
		}
		if s.nm.AddURLString(u) {
			s.autoSaveNodes()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))

	s.mux.HandleFunc("/node/delete", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "" {
			id = string(util.ReadAllSmall(r.Body))
		}
		if s.nm.Delete(id) {
			s.autoSaveNodes()
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))

	s.mux.HandleFunc("/node/delete/batch", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			IDs []string `json:"ids"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil || len(req.IDs) == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		deleted := 0
		for _, id := range req.IDs {
			if s.nm.Delete(id) {
				deleted++
			}
		}
		if deleted > 0 {
			s.autoSaveNodes()
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(strconv.Itoa(deleted)))
	}))

	s.mux.HandleFunc("/node/clear", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		s.nm.Clear()
		s.autoSaveNodes()
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/node/speedtest", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		id := r.URL.Query().Get("id")
		if id == "all" {
			go s.nm.TestAll()
			w.WriteHeader(http.StatusOK)
			w.Write([]byte("0"))
			return
		}
		if id != "" {
			ms, err := s.nm.TestNode(id)
			if err != nil {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("-1"))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(strconv.FormatInt(ms, 10)))
			return
		}
		w.WriteHeader(http.StatusBadRequest)
	}))

	s.mux.HandleFunc("/node/rotate/auto", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			Enable   bool     `json:"enable"`
			Interval string   `json:"interval"` // e.g., "10s", "1m"
			Pool     []string `json:"pool"`     // Optional: list of node IDs to rotate among
			Loop     bool     `json:"loop"`     // Optional: if true, sequential loop; if false, random (or single pass?) -> Let's map Loop=true to Sequential, Loop=false to Random for AutoRotate?
			// Wait, previous logic: "If loop is true, sequential... else random".
			// But for ChainRotate, we have "mode".
			// For AutoRotate, let's treat "Loop" as "Sequential".
			// If Loop is false, it uses Random.
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if req.Enable {
			dur, err := time.ParseDuration(req.Interval)
			if err != nil || dur < time.Second {
				dur = 10 * time.Second
			}
			s.nm.StartAutoRotate(dur, req.Pool, req.Loop)
		} else {
			s.nm.StopAutoRotate()
		}
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/node/rotate/chain", auth(func(w http.ResponseWriter, r *http.Request) {
		if s.nm == nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var req struct {
			Enable   bool     `json:"enable"`
			Interval string   `json:"interval"`
			Mode     string   `json:"mode"` // "random" or "seq"
			Pool     []string `json:"pool"`
		}
		if json.NewDecoder(r.Body).Decode(&req) != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}

		if req.Enable {
			dur, err := time.ParseDuration(req.Interval)
			if err != nil || dur < time.Second {
				dur = 10 * time.Second
			}
			mode := req.Mode
			if mode == "" {
				mode = "random"
			}
			s.nm.StartChainRotate(dur, mode, req.Pool)
		} else {
			s.nm.StopChainRotate()
		}
		w.WriteHeader(http.StatusOK)
	}))

	s.mux.HandleFunc("/system/shutdown", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		var req struct {
			SaveNodes  bool `json:"save_nodes"`
			ClearSaved bool `json:"clear_saved"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.SaveNodes && s.nm != nil {
			_ = s.nm.SaveState(nodesStateFile)
		}
		if req.ClearSaved {
			_ = os.Remove(nodesStateFile)
		}

		s.mu.Lock()
		s.proxyMode = "off"
		s.mu.Unlock()

		_ = sysproxy.ClearProxy()
		if s.nm != nil {
			s.nm.StopAutoRotate()
			s.nm.StopChainRotate()
		}

		w.WriteHeader(http.StatusOK)
		if s.onShutdown != nil {
			go s.onShutdown()
		}
	}))

	s.mux.HandleFunc("/api/memo", auth(func(w http.ResponseWriter, r *http.Request) {
		const memoFile = "memo.txt"
		if r.Method == "GET" {
			data, err := os.ReadFile(memoFile)
			if err != nil && !os.IsNotExist(err) {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			w.Write(data)
		} else if r.Method == "POST" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if err := os.WriteFile(memoFile, body, 0644); err != nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusOK)
		} else {
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
}

// GetPassword 返回当前服务器密码
func (s *Server) GetPassword() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.password
}

// defaultPassword returns the initial admin password.
// Keep README/tests behavior predictable (admin), while allowing operators to
// override it safely with ADMIN_PASSWORD in production.
func defaultPassword() string {
	if v := os.Getenv("ADMIN_PASSWORD"); v != "" {
		return v
	}
	return "admin"
}

// generateRandomPassword 生成一个安全的随机密码，保留给未来需要随机初始化密码时使用。
func generateRandomPassword() string {
	const chars = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*"
	password := make([]byte, 12)
	for i := 0; i < 12; i++ {
		num, err := rand.Int(rand.Reader, big.NewInt(int64(len(chars))))
		if err != nil {
			// 如果失败，使用时间戳作为备用方案
			return fmt.Sprintf("pass%d", time.Now().Unix()%100000)
		}
		password[i] = chars[num.Int64()]
	}
	return string(password)
}
