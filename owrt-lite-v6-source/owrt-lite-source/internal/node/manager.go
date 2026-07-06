package node

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"
)

type Node struct {
	ID           string
	Name         string
	URL          *url.URL
	Healthy      atomic.Bool
	Latency      atomic.Int64  // ms
	SuccessCount atomic.Uint64 // success connections
	FailCount    atomic.Uint64 // failed connections
}

type SavedChain struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	NodeIDs []string `json:"node_ids"`
}

type ExportNode struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Type     string `json:"type"`
	Server   string `json:"server"`
	Port     int    `json:"port"`
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Cipher   string `json:"cipher,omitempty"`
	UUID     string `json:"uuid,omitempty"`
	URL      string `json:"url,omitempty"`
}

type PersistState struct {
	Version     int          `json:"version"`
	SavedAt     string       `json:"saved_at"`
	Nodes       []ExportNode `json:"nodes"`
	SavedChains []SavedChain `json:"saved_chains"`
	ActiveID    string       `json:"active_id,omitempty"`
	ChainIDs    []string     `json:"chain_ids,omitempty"`
}

type Manager struct {
	mu     sync.RWMutex
	nodes  []*Node
	active atomic.Value // Stores *Node (single) or nil
	chain  []string     // Stores list of Node IDs for chain proxy

	savedChains []*SavedChain // Manually saved chains

	// Rotation controls
	rotateMu           sync.Mutex
	stopAutoRotate     chan struct{}
	stopChainRotate    chan struct{}
	autoRotateRunning  bool
	chainRotateRunning bool
	chainSeqIdx        int // Track sequential index

	logMu   sync.RWMutex
	sysLogs []string

	routeHookMu sync.RWMutex
	routeHook   func([]string)
}

func (m *Manager) AddLog(msg string) {
	m.logMu.Lock()
	defer m.logMu.Unlock()
	ts := time.Now().Format("15:04:05")
	logEntry := fmt.Sprintf("[%s] %s", ts, msg)
	m.sysLogs = append(m.sysLogs, logEntry)
	if len(m.sysLogs) > 50 {
		m.sysLogs = m.sysLogs[len(m.sysLogs)-50:]
	}
}

func (m *Manager) SetRouteChangeHook(fn func([]string)) {
	m.routeHookMu.Lock()
	m.routeHook = fn
	m.routeHookMu.Unlock()
}

func (m *Manager) notifyRouteChange(ids []string) {
	m.routeHookMu.RLock()
	fn := m.routeHook
	m.routeHookMu.RUnlock()
	if fn != nil {
		fn(append([]string(nil), ids...))
	}
}

func sameIDs(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (m *Manager) GetLogs() []string {
	m.logMu.RLock()
	defer m.logMu.RUnlock()
	// Return copy
	if len(m.sysLogs) == 0 {
		return []string{}
	}
	return append([]string(nil), m.sysLogs...)
}

func (m *Manager) Chain() []*Node {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.resolveChainLocked()
}

func (m *Manager) resolveChainLocked() []*Node {
	if len(m.chain) == 0 {
		return nil
	}
	var res []*Node
	for _, id := range m.chain {
		for _, n := range m.nodes {
			if n.ID == id {
				res = append(res, n)
				break
			}
		}
	}
	return res
}

func (m *Manager) setRouteIDs(ids []string) {
	if len(ids) == 0 {
		m.chain = nil
		m.active.Store((*Node)(nil))
		return
	}

	valid := make([]string, 0, len(ids))
	seen := map[string]struct{}{}
	var first *Node
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		for _, n := range m.nodes {
			if n.ID == id {
				seen[id] = struct{}{}
				valid = append(valid, id)
				if first == nil {
					first = n
				}
				break
			}
		}
	}

	if len(valid) == 0 {
		m.chain = nil
		m.active.Store((*Node)(nil))
		return
	}
	m.chain = valid
	m.active.Store(first)
}

func (m *Manager) SetChain(ids []string) {
	m.mu.Lock()
	old := append([]string(nil), m.chain...)
	m.setRouteIDs(ids)
	newIDs := append([]string(nil), m.chain...)
	m.mu.Unlock()
	if !sameIDs(old, newIDs) {
		m.AddLog(fmt.Sprintf("RouteChanged: %v", newIDs))
		m.notifyRouteChange(newIDs)
	}
}

func (m *Manager) GetChainIDs() []string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if len(m.chain) == 0 {
		return []string{}
	}
	return append([]string(nil), m.chain...)
}

func New(urls []*url.URL) *Manager {
	// 初始化随机数种子
	rand.Seed(time.Now().UnixNano())

	m := &Manager{}
	for i, u := range urls {
		n := &Node{ID: "n" + itoa(i), URL: u}
		n.Healthy.Store(true)
		m.nodes = append(m.nodes, n)
	}
	if len(m.nodes) > 0 {
		m.setRouteIDs([]string{m.nodes[0].ID})
	}
	return m
}

func (m *Manager) Active() *Node {
	v := m.active.Load()
	if v == nil {
		return nil
	}
	return v.(*Node)
}

func (m *Manager) SetActive(id string) bool {
	return m.SetActiveSingle(id)
}

// SetActiveSingle selects a single node route and clears any multi-hop chain.
func (m *Manager) SetActiveSingle(id string) bool {
	var changed bool
	var newIDs []string
	var logMsg string

	m.mu.Lock()
	old := append([]string(nil), m.chain...)
	if id == "" {
		m.setRouteIDs(nil)
		newIDs = append([]string(nil), m.chain...)
		changed = !sameIDs(old, newIDs)
		logMsg = "SelectNode: cleared"
		m.mu.Unlock()
		m.AddLog(logMsg)
		if changed {
			m.notifyRouteChange(newIDs)
		}
		return true
	}
	for _, n := range m.nodes {
		if n.ID == id {
			m.setRouteIDs([]string{id})
			newIDs = append([]string(nil), m.chain...)
			changed = !sameIDs(old, newIDs)
			logMsg = fmt.Sprintf("SelectNode: %s (%s) -> %s", n.Name, n.ID, n.URL.Host)
			m.mu.Unlock()
			m.AddLog(logMsg)
			if changed {
				m.notifyRouteChange(newIDs)
			}
			return true
		}
	}
	m.mu.Unlock()
	return false
}

func (m *Manager) Next() *Node {
	m.mu.RLock()
	if len(m.nodes) == 0 {
		m.mu.RUnlock()
		m.SetActiveSingle("")
		return nil
	}
	cur := m.Active()
	idx := 0
	if cur != nil {
		for i, n := range m.nodes {
			if n.ID == cur.ID {
				idx = i
				break
			}
		}
	}
	var next *Node
	for i := 1; i <= len(m.nodes); i++ {
		n := m.nodes[(idx+i)%len(m.nodes)]
		if n.Healthy.Load() {
			next = n
			break
		}
	}
	if next == nil {
		next = m.nodes[0]
	}
	nextID := next.ID
	m.mu.RUnlock()
	m.SetActiveSingle(nextID)
	return next
}

type NodeJSON struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Type          string `json:"type"`
	Host          string `json:"host"`
	Port          string `json:"port"`
	Password      string `json:"password"`
	Cipher        string `json:"cipher"`
	Latency       int64  `json:"latency"`
	SuccessCount  uint64 `json:"success_count"`
	FailCount     uint64 `json:"fail_count"`
	Healthy       bool   `json:"healthy"`
	RuntimeStatus string `json:"runtime_status"`
	URL           string `json:"url"`
}

func (m *Manager) List() []NodeJSON {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]NodeJSON, 0, len(m.nodes))
	activeID := ""
	if a := m.Active(); a != nil {
		activeID = a.ID
	}
	chainPos := make(map[string]int, len(m.chain))
	for i, id := range m.chain {
		chainPos[id] = i + 1
	}
	for _, n := range m.nodes {
		host := n.URL.Hostname()
		port := n.URL.Port()
		pass, _ := "", false
		user := ""
		if n.URL.User != nil {
			pass, _ = n.URL.User.Password()
			user = n.URL.User.Username()
		}
		cipher := ""
		password := pass

		if n.URL.Scheme == "ss" {
			cipher = user
			if password == "" {
				if decodedCipher, decodedPassword, ok := decodeSSUserInfo(user); ok {
					cipher = decodedCipher
					password = decodedPassword
				}
			}
		} else if n.URL.Scheme == "trojan" {
			if password == "" {
				password = user
			}
		} else {
			// For others, password is usually in password field
			// If user field is used for something else, we might need to send it too
			// But for now, let's stick to what Update expects
		}

		runtimeStatus := "空闲"
		if !n.Healthy.Load() {
			runtimeStatus = "异常"
		}
		if pos, ok := chainPos[n.ID]; ok {
			if len(m.chain) == 1 && n.ID == activeID {
				runtimeStatus = "当前出口"
			} else {
				runtimeStatus = fmt.Sprintf("链路第%d跳", pos)
			}
		}

		out = append(out, NodeJSON{
			ID:            n.ID,
			Name:          n.Name,
			Type:          n.URL.Scheme,
			Host:          host,
			Port:          port,
			Password:      password,
			Cipher:        cipher,
			Latency:       n.Latency.Load(),
			SuccessCount:  n.SuccessCount.Load(),
			FailCount:     n.FailCount.Load(),
			Healthy:       n.Healthy.Load(),
			RuntimeStatus: runtimeStatus,
			URL:           n.URL.String(),
		})
	}
	return out
}

func (m *Manager) Rename(id, name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, n := range m.nodes {
		if n.ID == id {
			n.Name = name
			return true
		}
	}
	return false
}

func normalizeScheme(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "shadowsocks":
		return "ss"
	case "socks":
		return "socks5"
	default:
		return s
	}
}

func validScheme(s string) bool {
	switch normalizeScheme(s) {
	case "ss", "socks5", "http", "https", "vmess", "trojan":
		return true
	default:
		return false
	}
}

func decodeSSUserInfo(raw string) (string, string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", false
	}
	if decoded, err := base64.RawURLEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	} else if decoded, err := base64.URLEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	} else if decoded, err := base64.RawStdEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	} else if decoded, err := base64.StdEncoding.DecodeString(raw); err == nil {
		raw = string(decoded)
	}
	if !strings.Contains(raw, ":") {
		return "", "", false
	}
	parts := strings.SplitN(raw, ":", 2)
	if parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func normalizeURLCredentials(u *url.URL) {
	if u == nil || u.User == nil {
		return
	}
	switch normalizeScheme(u.Scheme) {
	case "ss":
		user := u.User.Username()
		pass, hasPass := u.User.Password()
		if (!hasPass || pass == "") && user != "" {
			if method, password, ok := decodeSSUserInfo(user); ok {
				u.User = url.UserPassword(method, password)
			}
		}
	case "trojan":
		user := u.User.Username()
		pass, hasPass := u.User.Password()
		if hasPass && pass != "" && user == "" {
			u.User = url.User(pass)
		}
	}
}

func (m *Manager) nextNodeIDLocked() string {
	maxID := -1
	used := make(map[string]struct{}, len(m.nodes))
	for _, n := range m.nodes {
		used[n.ID] = struct{}{}
		if strings.HasPrefix(n.ID, "n") {
			if v, err := strconv.Atoi(strings.TrimPrefix(n.ID, "n")); err == nil && v > maxID {
				maxID = v
			}
		}
	}
	for i := maxID + 1; ; i++ {
		id := "n" + itoa(i)
		if _, ok := used[id]; !ok {
			return id
		}
	}
}

func joinHostPort(host string, port int) string {
	return net.JoinHostPort(strings.TrimSpace(host), strconv.Itoa(port))
}

func nodeURLFromExport(en ExportNode) (*url.URL, bool) {
	if strings.TrimSpace(en.URL) != "" {
		if u, err := url.Parse(strings.TrimSpace(en.URL)); err == nil && u.Host != "" && validScheme(u.Scheme) {
			return u, true
		}
	}
	scheme := normalizeScheme(en.Type)
	if en.Server == "" || en.Port < 1 || en.Port > 65535 || !validScheme(scheme) {
		return nil, false
	}
	u := &url.URL{Scheme: scheme, Host: joinHostPort(en.Server, en.Port)}
	if scheme == "ss" {
		cipher := en.Cipher
		if cipher == "" {
			cipher = en.Username
		}
		if cipher == "" {
			cipher = "none"
		}
		u.User = url.UserPassword(cipher, en.Password)
	} else if scheme == "trojan" {
		password := en.Password
		if password == "" {
			password = en.Username
		}
		u.User = url.User(password)
	} else if scheme == "vmess" {
		if en.UUID != "" {
			u.User = url.User(en.UUID)
		}
	} else if en.Username != "" || en.Password != "" {
		u.User = url.UserPassword(en.Username, en.Password)
	}
	return u, true
}

func (m *Manager) Update(id, name, scheme, host, port, password, cipher string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	var target *Node
	for _, n := range m.nodes {
		if n.ID == id {
			target = n
			break
		}
	}
	if target == nil {
		return false
	}
	scheme = normalizeScheme(scheme)
	host = strings.TrimSpace(host)
	if host == "" || !validScheme(scheme) {
		return false
	}

	// 验证端口合法性
	portNum, err := strconv.Atoi(port)
	if err != nil || portNum < 1 || portNum > 65535 {
		return false
	}

	// Build new URL
	u := &url.URL{
		Scheme: scheme,
		Host:   joinHostPort(host, portNum),
	}

	if scheme == "ss" {
		if cipher != "" {
			u.User = url.UserPassword(cipher, password)
		} else {
			u.User = url.UserPassword("none", password)
		}
	} else if scheme == "trojan" {
		if password != "" {
			u.User = url.User(password)
		}
	} else if scheme == "vmess" {
		// VMess support is limited
	} else {
		if password != "" {
			u.User = url.UserPassword("", password)
		}
	}

	// Update fields
	target.Name = name
	target.URL = u
	return true
}

func (m *Manager) addProxyNode(name, scheme, host string, port any, password, cipher, user, uuid string) bool {
	scheme = normalizeScheme(scheme)
	host = strings.TrimSpace(host)
	if host == "" || !validScheme(scheme) {
		return false
	}
	// 验证端口合法性
	var portNum int
	var err error
	switch p := port.(type) {
	case int:
		portNum = p
	case string:
		portNum, err = strconv.Atoi(p)
		if err != nil {
			return false
		}
	case float64:
		portNum = int(p)
	default:
		return false
	}
	if portNum < 1 || portNum > 65535 {
		return false
	}

	u := &url.URL{
		Scheme: scheme,
		Host:   joinHostPort(host, portNum),
	}

	if scheme == "ss" {
		if cipher != "" {
			u.User = url.UserPassword(cipher, password)
		} else {
			u.User = url.UserPassword("none", password)
		}
	} else if scheme == "trojan" {
		if password == "" {
			password = user
		}
		if password != "" {
			u.User = url.User(password)
		}
	} else if scheme == "vmess" {
		// VMess support is limited, store as is
		if uuid != "" {
			u.User = url.User(uuid)
		}
	} else {
		if user != "" || password != "" {
			u.User = url.UserPassword(user, password)
		}
	}

	// Check dup
	for _, n := range m.nodes {
		if n.URL.String() == u.String() {
			return false
		}
	}

	n := &Node{ID: m.nextNodeIDLocked(), Name: name, URL: u}
	n.Healthy.Store(true)
	m.nodes = append(m.nodes, n)
	return true
}

func (m *Manager) ImportYAML(r io.Reader) int {
	data, err := io.ReadAll(r)
	if err != nil {
		return 0
	}

	var config struct {
		Proxies []struct {
			Name     string      `yaml:"name"`
			Type     string      `yaml:"type"`
			Server   string      `yaml:"server"`
			Port     interface{} `yaml:"port"`
			Password string      `yaml:"password"`
			Cipher   string      `yaml:"cipher"`
			User     string      `yaml:"username"`
			UUID     string      `yaml:"uuid"`
		} `yaml:"proxies"`
	}

	if yaml.Unmarshal(data, &config) == nil && len(config.Proxies) > 0 {
		m.mu.Lock()
		defer m.mu.Unlock()
		added := 0
		for _, p := range config.Proxies {
			if m.addProxyNode(p.Name, p.Type, p.Server, p.Port, p.Password, p.Cipher, p.User, p.UUID) {
				added++
			}
		}
		return added
	}
	return 0
}

func (m *Manager) AddManual(name, scheme, host, port, password, cipher string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.addProxyNode(name, scheme, host, port, password, cipher, "", "")
}

func (m *Manager) ImportText(r io.Reader) int {
	data, _ := io.ReadAll(r)
	s := strings.TrimSpace(string(data))

	// Try JSON first if it looks like JSON
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") {
		if added := m.ImportJSON(strings.NewReader(s)); added > 0 {
			return added
		}
	}

	// Try YAML first if it looks like YAML
	if strings.Contains(s, "proxies:") || strings.HasPrefix(s, "- {") {
		if added := m.ImportYAML(strings.NewReader(s)); added > 0 {
			return added
		}
	}

	// Try Base64 decode
	if decoded, err := base64.StdEncoding.DecodeString(s); err == nil {
		s = string(decoded)
	} else if decoded, err := base64.URLEncoding.DecodeString(s); err == nil {
		s = string(decoded)
	}

	lines := strings.Split(s, "\n")

	// Optimization: Parse candidates outside lock to minimize lock contention
	type candidate struct {
		UseProxyNode                 bool
		Name, Scheme, Host           string
		Port                         any
		Password, Cipher, User, UUID string
		URL                          *url.URL
	}
	var candidates []candidate

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		// Try VMess
		if strings.HasPrefix(line, "vmess://") {
			b64 := strings.TrimPrefix(line, "vmess://")
			if decoded, err := base64.StdEncoding.DecodeString(b64); err == nil {
				var v struct {
					Ps   string `json:"ps"`
					Add  string `json:"add"`
					Port any    `json:"port"`
					Id   string `json:"id"`
					Net  string `json:"net"`
					Type string `json:"type"`
					Tls  string `json:"tls"`
				}
				if json.Unmarshal(decoded, &v) == nil {
					candidates = append(candidates, candidate{
						UseProxyNode: true,
						Name:         v.Ps, Scheme: "vmess", Host: v.Add, Port: v.Port, UUID: v.Id,
					})
					continue
				}
			}
		}

		// Try SS
		if strings.HasPrefix(line, "ss://") {
			// Basic SS parsing
			part := strings.TrimPrefix(line, "ss://")
			remark := ""
			if idx := strings.Index(part, "#"); idx != -1 {
				if r, err := url.QueryUnescape(part[idx+1:]); err == nil {
					remark = r
				} else {
					remark = part[idx+1:]
				}
				part = part[:idx]
			}

			// Try decoding
			var userinfo, hostport string
			if decoded, err := base64.RawURLEncoding.DecodeString(part); err == nil {
				part = string(decoded)
			} else if decoded, err := base64.URLEncoding.DecodeString(part); err == nil {
				part = string(decoded)
			} else if decoded, err := base64.StdEncoding.DecodeString(part); err == nil {
				part = string(decoded)
			}

			if strings.Contains(part, "@") {
				parts := strings.SplitN(part, "@", 2)
				userinfo = parts[0]
				hostport = parts[1]
			}

			if hostport != "" {
				host, port, _ := net.SplitHostPort(hostport)
				if cipher, password, ok := decodeSSUserInfo(userinfo); ok {
					candidates = append(candidates, candidate{
						UseProxyNode: true,
						Name:         remark, Scheme: "ss", Host: host, Port: port,
						Password: password, Cipher: cipher,
					})
					continue
				}
			}
		}

		if u, err := url.Parse(line); err == nil && u.Host != "" {
			normalizeURLCredentials(u)
			candidates = append(candidates, candidate{
				UseProxyNode: false,
				URL:          u,
			})
		}
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	added := 0

	for _, c := range candidates {
		if c.UseProxyNode {
			if m.addProxyNode(c.Name, c.Scheme, c.Host, c.Port, c.Password, c.Cipher, c.User, c.UUID) {
				added++
			}
		} else {
			// Check dup
			dup := false
			for _, n := range m.nodes {
				if n.URL.String() == c.URL.String() {
					dup = true
					break
				}
			}
			if !dup {
				n := &Node{ID: m.nextNodeIDLocked(), Name: c.URL.Fragment, URL: c.URL} // Use fragment as name if present
				n.Healthy.Store(true)
				m.nodes = append(m.nodes, n)
				added++
			}
		}
	}
	return added
}

func (m *Manager) ImportJSON(r io.Reader) int {
	// Try parsing as complex structure first
	data, err := io.ReadAll(r)
	if err != nil {
		return 0
	}

	// 1. Try Clash-like format: { "proxies": [...] }
	var clash struct {
		Proxies []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Server   string `json:"server"`
			Port     any    `json:"port"` // can be string or int
			Password string `json:"password"`
			Cipher   string `json:"cipher"` // Add Cipher
			User     string `json:"username"`
			UUID     string `json:"uuid"`
		} `json:"proxies"`
	}
	if json.Unmarshal(data, &clash) == nil && len(clash.Proxies) > 0 {
		m.mu.Lock()
		defer m.mu.Unlock()
		added := 0
		for _, p := range clash.Proxies {
			if m.addProxyNode(p.Name, p.Type, p.Server, p.Port, p.Password, p.Cipher, p.User, p.UUID) {
				added++
			}
		}
		return added
	}

	// 2. Try simple list format
	var simple struct {
		Nodes []string `json:"nodes"`
	}
	if json.Unmarshal(data, &simple) == nil && len(simple.Nodes) > 0 {
		added := 0
		for _, s := range simple.Nodes {
			u, err := url.Parse(strings.TrimSpace(s))
			if err != nil || u.Host == "" {
				continue
			}
			m.addURL(u)
			added++
		}
		return added
	}

	// 3. Try generic list of objects
	var generic []map[string]any
	if json.Unmarshal(data, &generic) == nil {
		added := 0
		for _, obj := range generic {
			name, _ := obj["name"].(string)

			// Try to find server/host/ip and port
			host, _ := obj["server"].(string)
			if host == "" {
				host, _ = obj["host"].(string)
			}
			if host == "" {
				host, _ = obj["ip"].(string)
			}

			// Port can be string or int in JSON
			var port string
			if p, ok := obj["port"]; ok {
				port = fmt.Sprint(p)
			}

			scheme, _ := obj["type"].(string)
			if scheme == "" {
				scheme, _ = obj["scheme"].(string)
			}
			if scheme == "" {
				scheme, _ = obj["protocol"].(string)
			}

			password, _ := obj["password"].(string)
			cipher, _ := obj["cipher"].(string)
			user, _ := obj["username"].(string)
			uuid, _ := obj["uuid"].(string)

			// Special handling for SS password field being "cipher:password"
			if (scheme == "ss" || scheme == "shadowsocks") && cipher == "" && strings.Contains(password, ":") {
				parts := strings.SplitN(password, ":", 2)
				cipher = parts[0]
				password = parts[1]
			}

			if host != "" && port != "" && scheme != "" {
				if m.addProxyNode(name, scheme, host, port, password, cipher, user, uuid) {
					added++
				}
			}
		}
		return added
	}

	return 0
}

func (m *Manager) addURL(u *url.URL) {
	normalizeURLCredentials(u)
	m.mu.Lock()
	defer m.mu.Unlock()
	n := &Node{ID: m.nextNodeIDLocked(), URL: u}
	n.Healthy.Store(true)
	m.nodes = append(m.nodes, n)
	if len(m.chain) == 0 && m.Active() == nil {
		m.setRouteIDs([]string{n.ID})
	}
}

func (m *Manager) AddURLString(s string) bool {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Host == "" {
		return false
	}
	normalizeURLCredentials(u)
	m.addURL(u)
	return true
}

func (m *Manager) exportNodesLocked() []ExportNode {
	list := make([]ExportNode, 0, len(m.nodes))
	for _, n := range m.nodes {
		host := n.URL.Hostname()
		portStr := n.URL.Port()
		port, _ := strconv.Atoi(portStr)

		pass := ""
		user := ""
		if n.URL.User != nil {
			pass, _ = n.URL.User.Password()
			user = n.URL.User.Username()
		}

		en := ExportNode{
			ID:     n.ID,
			Name:   n.Name,
			Type:   n.URL.Scheme,
			Server: host,
			Port:   port,
			URL:    n.URL.String(),
		}

		if n.URL.Scheme == "ss" {
			en.Cipher = user
			en.Password = pass
		} else if n.URL.Scheme == "trojan" {
			if pass != "" {
				en.Password = pass
			} else {
				en.Password = user
			}
		} else if n.URL.Scheme == "vmess" {
			en.UUID = user
		} else {
			en.Username = user
			en.Password = pass
		}
		list = append(list, en)
	}
	if list == nil {
		return []ExportNode{}
	}
	return list
}

func (m *Manager) ExportJSON() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	data, _ := json.MarshalIndent(m.exportNodesLocked(), "", "  ")
	return data
}

func (m *Manager) SaveState(path string) error {
	m.mu.RLock()
	activeID := ""
	if a := m.Active(); a != nil {
		activeID = a.ID
	}
	chains := make([]SavedChain, len(m.savedChains))
	for i, sc := range m.savedChains {
		chains[i] = *sc
	}
	st := PersistState{
		Version:     1,
		SavedAt:     time.Now().Format(time.RFC3339),
		Nodes:       m.exportNodesLocked(),
		SavedChains: chains,
		ActiveID:    activeID,
		ChainIDs:    append([]string(nil), m.chain...),
	}
	m.mu.RUnlock()

	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	m.AddLog(fmt.Sprintf("Saved nodes to %s", path))
	return nil
}

func (m *Manager) LoadState(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st PersistState
	if err := json.Unmarshal(data, &st); err != nil || len(st.Nodes) == 0 {
		var nodes []ExportNode
		if err2 := json.Unmarshal(data, &nodes); err2 != nil {
			if err != nil {
				return err
			}
			return err2
		}
		st.Nodes = nodes
	}

	m.StopAutoRotate()
	m.StopChainRotate()

	m.mu.Lock()
	old := append([]string(nil), m.chain...)
	m.nodes = nil
	m.savedChains = nil
	seenIDs := map[string]struct{}{}
	for _, en := range st.Nodes {
		u, ok := nodeURLFromExport(en)
		if !ok {
			continue
		}
		id := strings.TrimSpace(en.ID)
		if id == "" {
			id = m.nextNodeIDLocked()
		}
		if _, exists := seenIDs[id]; exists {
			id = m.nextNodeIDLocked()
		}
		seenIDs[id] = struct{}{}
		n := &Node{ID: id, Name: en.Name, URL: u}
		n.Healthy.Store(true)
		m.nodes = append(m.nodes, n)
	}
	for _, sc := range st.SavedChains {
		if sc.ID == "" || len(sc.NodeIDs) == 0 {
			continue
		}
		copyIDs := append([]string(nil), sc.NodeIDs...)
		m.savedChains = append(m.savedChains, &SavedChain{ID: sc.ID, Name: sc.Name, NodeIDs: copyIDs})
	}
	if len(st.ChainIDs) > 0 {
		m.setRouteIDs(st.ChainIDs)
	} else if st.ActiveID != "" {
		m.setRouteIDs([]string{st.ActiveID})
	} else if len(m.nodes) > 0 {
		m.setRouteIDs([]string{m.nodes[0].ID})
	} else {
		m.setRouteIDs(nil)
	}
	newIDs := append([]string(nil), m.chain...)
	loaded := len(m.nodes)
	m.mu.Unlock()

	m.AddLog(fmt.Sprintf("Loaded %d nodes from %s", loaded, path))
	if !sameIDs(old, newIDs) {
		m.notifyRouteChange(newIDs)
	}
	return nil
}

func (m *Manager) Delete(id string) bool {
	m.mu.Lock()
	idx := -1
	for i, n := range m.nodes {
		if n.ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		m.mu.Unlock()
		return false
	}
	old := append([]string(nil), m.chain...)
	cur := m.Active()
	del := m.nodes[idx]
	m.nodes = append(m.nodes[:idx], m.nodes[idx+1:]...)
	if cur != nil && cur.ID == del.ID {
		if len(m.nodes) > 0 {
			m.setRouteIDs([]string{m.nodes[0].ID})
		} else {
			m.setRouteIDs(nil)
		}
	} else if len(m.chain) > 0 {
		// Remove deleted node from current route if present
		var kept []string
		for _, id := range m.chain {
			if id != del.ID {
				kept = append(kept, id)
			}
		}
		m.setRouteIDs(kept)
	}
	newIDs := append([]string(nil), m.chain...)
	m.mu.Unlock()
	if !sameIDs(old, newIDs) {
		m.notifyRouteChange(newIDs)
	}
	return true
}

func (m *Manager) Clear() {
	m.StopAutoRotate()
	m.StopChainRotate()
	m.mu.Lock()
	old := append([]string(nil), m.chain...)
	m.nodes = []*Node{}
	m.setRouteIDs(nil)
	m.chainSeqIdx = 0
	newIDs := append([]string(nil), m.chain...)
	m.mu.Unlock()
	if !sameIDs(old, newIDs) {
		m.notifyRouteChange(newIDs)
	}
}
func itoa(i int) string {
	return strconv.Itoa(i)
}

func (m *Manager) TestNode(id string) (int64, error) {
	m.mu.RLock()
	var target *Node
	for _, n := range m.nodes {
		if n.ID == id {
			target = n
			break
		}
	}
	m.mu.RUnlock()

	if target == nil {
		return 0, fmt.Errorf("node not found")
	}

	start := time.Now()
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := dialer.Dial("tcp", target.URL.Host)
	if err == nil && strings.ToLower(target.URL.Scheme) == "trojan" {
		serverName := target.URL.Query().Get("sni")
		if serverName == "" {
			serverName = target.URL.Query().Get("peer")
		}
		if serverName == "" {
			serverName = target.URL.Hostname()
		}
		tlsConn := tls.Client(conn, &tls.Config{ServerName: serverName})
		err = tlsConn.Handshake()
		conn = tlsConn
	}
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		target.FailCount.Add(1)
		target.Healthy.Store(false)
		target.Latency.Store(-1)
		return -1, err
	}
	conn.Close()

	if !validScheme(target.URL.Scheme) || strings.ToLower(target.URL.Scheme) == "vmess" {
		target.FailCount.Add(1)
		target.Healthy.Store(false)
		target.Latency.Store(-1)
		return -1, fmt.Errorf("unsupported scheme: %s", target.URL.Scheme)
	}

	latency := time.Since(start).Milliseconds()
	target.SuccessCount.Add(1)
	target.Healthy.Store(true)
	target.Latency.Store(latency)
	return latency, nil
}

func (m *Manager) TestAll() {
	m.mu.RLock()
	nodes := m.nodes
	m.mu.RUnlock()

	var wg sync.WaitGroup
	sem := make(chan struct{}, 10) // Limit concurrency

	for _, n := range nodes {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			m.TestNode(id)
		}(n.ID)
	}
	wg.Wait()
}

func (m *Manager) StartAutoRotate(interval time.Duration, pool []string, loop bool) {
	m.rotateMu.Lock()
	defer m.rotateMu.Unlock()

	if m.autoRotateRunning {
		if m.stopAutoRotate != nil {
			close(m.stopAutoRotate)
		}
	}

	m.stopAutoRotate = make(chan struct{})
	m.autoRotateRunning = true
	ch := m.stopAutoRotate

	m.AddLog(fmt.Sprintf("Start AutoRotate: interval=%v, pool_size=%d, loop=%v", interval, len(pool), loop))

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		idx := -1 // For sequential; rotate immediately on start
		idx = m.rotateNode(pool, loop, idx)

		for {
			select {
			case <-ticker.C:
				idx = m.rotateNode(pool, loop, idx)
			case <-ch:
				m.AddLog("Stop AutoRotate")
				return
			}
		}
	}()
}

func (m *Manager) StopAutoRotate() {
	m.rotateMu.Lock()
	defer m.rotateMu.Unlock()
	if m.autoRotateRunning {
		if m.stopAutoRotate != nil {
			close(m.stopAutoRotate)
			m.stopAutoRotate = nil
		}
		m.autoRotateRunning = false
		m.AddLog("Stop AutoRotate")
	}
}

func (m *Manager) rotateNode(pool []string, loop bool, lastIdx int) int {
	m.mu.RLock()
	nodes := m.nodes
	m.mu.RUnlock()

	var candidates []*Node

	// Filter by pool if provided
	if len(pool) > 0 {
		for _, id := range pool {
			for _, n := range nodes {
				if n.ID == id {
					if n.Healthy.Load() {
						candidates = append(candidates, n)
					}
					break
				}
			}
		}
	} else {
		// Use all healthy nodes
		for _, n := range nodes {
			if n.Healthy.Load() {
				candidates = append(candidates, n)
			}
		}
	}

	if len(candidates) == 0 {
		m.AddLog("AutoRotate: No healthy candidates available")
		return lastIdx
	}

	var next *Node
	var nextIdx int

	if loop {
		// Sequential (Loop)
		nextIdx = (lastIdx + 1) % len(candidates)
		next = candidates[nextIdx]
	} else {
		// Random
		nextIdx = rand.Intn(len(candidates))
		next = candidates[nextIdx]
	}

	m.SetActiveSingle(next.ID)
	m.AddLog(fmt.Sprintf("AutoRotate: Switched to %s (%s)", next.Name, next.ID))
	return nextIdx
}

func (m *Manager) StartChainRotate(interval time.Duration, mode string, pool []string) {
	m.rotateMu.Lock()
	defer m.rotateMu.Unlock()

	if m.chainRotateRunning {
		if m.stopChainRotate != nil {
			close(m.stopChainRotate)
		}
	}

	m.stopChainRotate = make(chan struct{})
	m.chainRotateRunning = true
	ch := m.stopChainRotate

	m.AddLog(fmt.Sprintf("Start ChainRotate: interval=%v, mode=%s, pool_size=%d", interval, mode, len(pool)))

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		m.rotateChain(mode, pool)

		for {
			select {
			case <-ticker.C:
				m.rotateChain(mode, pool)
			case <-ch:
				m.AddLog("Stop ChainRotate")
				return
			}
		}
	}()
}

func (m *Manager) StopChainRotate() {
	m.rotateMu.Lock()
	defer m.rotateMu.Unlock()
	if m.chainRotateRunning {
		if m.stopChainRotate != nil {
			close(m.stopChainRotate)
			m.stopChainRotate = nil
		}
		m.chainRotateRunning = false
		m.AddLog("Stop ChainRotate")
	}
}

func (m *Manager) rotateChain(mode string, pool []string) {
	m.mu.RLock()
	// Check if we are rotating SAVED chains or NODES
	// If pool contains "c..." IDs, assume saved chains.
	useSaved := false
	if len(pool) > 0 {
		for _, id := range pool {
			if strings.HasPrefix(id, "c") {
				useSaved = true
				break
			}
		}
	}
	m.mu.RUnlock()

	if useSaved {
		m.rotateSavedChain(mode, pool)
		return
	}

	// Existing logic for node-based chain rotation (default to single-node chain for compatibility)
	m.mu.RLock()
	nodes := m.nodes
	m.mu.RUnlock()

	var candidates []*Node
	if len(pool) > 0 {
		for _, id := range pool {
			for _, n := range nodes {
				if n.ID == id {
					if n.Healthy.Load() {
						candidates = append(candidates, n)
					}
					break
				}
			}
		}
	} else {
		for _, n := range nodes {
			if n.Healthy.Load() {
				candidates = append(candidates, n)
			}
		}
	}

	if len(candidates) == 0 {
		m.AddLog("ChainRotate: No healthy candidates")
		return
	}

	var ids []string
	if mode == "seq" {
		idx := m.chainSeqIdx % len(candidates)
		ids = []string{candidates[idx].ID}
		m.chainSeqIdx++
	} else {
		// random
		ids = []string{candidates[rand.Intn(len(candidates))].ID}
	}

	if len(ids) > 0 {
		m.SetChain(ids)
		m.AddLog(fmt.Sprintf("ChainRotate: Built chain %v (single-node)", ids))
	}
}

func (m *Manager) rotateSavedChain(mode string, pool []string) {
	m.mu.RLock()
	saved := m.savedChains
	m.mu.RUnlock()

	var candidates []*SavedChain
	// Filter pool
	if len(pool) > 0 {
		// Optimization: Create map for O(1) lookup if pool is large,
		// but for small N (typical for saved chains), nested loop is fine.
		// However, let's do it efficiently.
		savedMap := make(map[string]*SavedChain, len(saved))
		for _, sc := range saved {
			savedMap[sc.ID] = sc
		}

		for _, id := range pool {
			if sc, ok := savedMap[id]; ok {
				candidates = append(candidates, sc)
			}
		}
	} else {
		candidates = saved
	}

	if len(candidates) == 0 {
		m.AddLog("ChainRotate: No saved chains available")
		return
	}

	var next *SavedChain

	if mode == "random" {
		next = candidates[rand.Intn(len(candidates))]
	} else {
		idx := m.chainSeqIdx % len(candidates)
		next = candidates[idx]
		m.chainSeqIdx++
	}

	m.SetChain(next.NodeIDs)
	m.AddLog(fmt.Sprintf("ChainRotate: Switched to saved chain %s (%s)", next.Name, next.ID))
}

func (m *Manager) GetGlobalStats() map[string]int64 {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var success, fail, totalLat int64
	var healthyCnt int64
	var count int64

	for _, n := range m.nodes {
		s := int64(n.SuccessCount.Load())
		f := int64(n.FailCount.Load())
		l := n.Latency.Load()
		success += s
		fail += f
		if n.Healthy.Load() {
			healthyCnt++
		}
		if l > 0 {
			totalLat += l
			count++
		}
	}

	avgLat := int64(0)
	if count > 0 {
		avgLat = totalLat / count
	}

	return map[string]int64{
		"total_nodes":   int64(len(m.nodes)),
		"healthy_nodes": healthyCnt,
		"total_success": success,
		"total_fail":    fail,
		"avg_latency":   avgLat,
	}
}

func (m *Manager) GetRotationStatus() map[string]any {
	m.rotateMu.Lock()
	defer m.rotateMu.Unlock()
	return map[string]any{
		"auto_rotate":  m.autoRotateRunning,
		"chain_rotate": m.chainRotateRunning,
	}
}

// Saved Chain CRUD

func (m *Manager) AddSavedChain(name string, ids []string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 使用时间戳生成唯一 ID
	id := fmt.Sprintf("c%d", time.Now().UnixNano())

	m.savedChains = append(m.savedChains, &SavedChain{
		ID:      id,
		Name:    name,
		NodeIDs: ids,
	})
	return id
}

func (m *Manager) GetSavedChains() []SavedChain {
	m.mu.RLock()
	defer m.mu.RUnlock()
	res := make([]SavedChain, len(m.savedChains))
	for i, sc := range m.savedChains {
		res[i] = *sc
	}
	return res
}

func (m *Manager) DeleteSavedChain(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, sc := range m.savedChains {
		if sc.ID == id {
			m.savedChains = append(m.savedChains[:i], m.savedChains[i+1:]...)
			return true
		}
	}
	return false
}
