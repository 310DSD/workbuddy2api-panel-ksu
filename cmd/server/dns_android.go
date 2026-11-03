package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Android 上 CGO 关闭的 Go 会读 /etc/resolv.conf，里面经常是 nameserver ::1。
// netd 并不给这种静态二进制提供 [::1]:53，于是所有上游域名解析失败。
// 这里改用系统 getprop 的 DNS，并兜底公共解析器；不影响查询结果，只换解析通道。

var (
	dnsMu       sync.Mutex
	dnsCached   []string
	dnsCachedAt time.Time
	// dnsRR 轮转起点：同一个「首选 DNS 不可达」的账号不该每次都撞同一台服务器。
	// Go 的解析器在查询超时后会再次调用 Dial，这里每次从下一台开始，最坏情况
	// （首选服务器黑洞）也能在第二次尝试换到下一台，而不是永远卡在第一台。
	dnsRR atomic.Uint64
)

func init() {
	installAndroidResolver()
}

func installAndroidResolver() {
	servers := loadDNSServers()
	if len(servers) == 0 {
		return
	}
	net.DefaultResolver = &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			// 解析器给的 address 若是**真正可用**的 DNS（非环回），优先按它的顺序拨：
			// 多 DNS 配置时由 Go 自己决定何时换服务器。Android 上是 [::1]:53，
			// 走不了，落到下面的 getprop + 公共兜底列表。
			if host, _, err := net.SplitHostPort(address); err == nil {
				if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
					if c, derr := d.DialContext(ctx, network, address); derr == nil {
						return c, nil
					}
				}
			}
			servers := loadDNSServers()
			if len(servers) == 0 {
				return nil, &net.OpError{Op: "dial", Net: network, Err: os.ErrNotExist}
			}
			// 轮转起点：每次 Dial 往后挪一格。UDP 拨号几乎总是立即成功（无握手），
			// 服务器「不可达」只能在查询超时里暴露；轮转保证连续两次尝试不会
			// 都压在同一个黑洞 DNS 上（手机切网后 net.dns1 常常是旧网络的地址）。
			start := int(dnsRR.Add(1)) % len(servers)
			var last error
			for i := 0; i < len(servers); i++ {
				srv := servers[(start+i)%len(servers)]
				// 先 UDP；连接失败再试 TCP（少数网络封 UDP 53）。
				for _, nw := range []string{"udp", "tcp"} {
					c, err := d.DialContext(ctx, nw, srv)
					if err == nil {
						return c, nil
					}
					last = err
				}
			}
			if last == nil {
				last = &net.OpError{Op: "dial", Net: network, Err: os.ErrNotExist}
			}
			return nil, last
		},
	}
	log.Printf("dns: using nameservers %s (bypass loopback resolv.conf)", strings.Join(servers, ", "))
}

func loadDNSServers() []string {
	dnsMu.Lock()
	defer dnsMu.Unlock()
	if time.Since(dnsCachedAt) < 30*time.Second && len(dnsCached) > 0 {
		return dnsCached
	}
	seen := map[string]bool{}
	var out []string
	add := func(host string) {
		host = strings.TrimSpace(host)
		if host == "" {
			return
		}
		if i := strings.IndexByte(host, '%'); i >= 0 {
			host = host[:i]
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return
		}
		if ip.IsLoopback() || ip.IsUnspecified() {
			return
		}
		addr := net.JoinHostPort(ip.String(), "53")
		if seen[addr] {
			return
		}
		seen[addr] = true
		out = append(out, addr)
	}

	if v := os.Getenv("WB2A_DNS"); v != "" {
		for _, p := range strings.Split(v, ",") {
			add(p)
		}
	}
	for _, key := range []string{"net.dns1", "net.dns2", "net.dns3", "net.dns4"} {
		add(getprop(key))
	}
	// 国内可达的公共 DNS 兜底（系统 DNS 为空或仍是环回时）
	for _, fb := range []string{"223.5.5.5", "223.6.6.6", "119.29.29.29", "8.8.8.8", "1.1.1.1"} {
		add(fb)
	}
	dnsCached = out
	dnsCachedAt = time.Now()
	return out
}

func getprop(key string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	cmd := exec.CommandContext(ctx, "getprop", key)
	b, err := cmd.Output()
	if err != nil {
		cmd = exec.CommandContext(ctx, "/system/bin/getprop", key)
		b, err = cmd.Output()
		if err != nil {
			return ""
		}
	}
	return strings.TrimSpace(string(b))
}
