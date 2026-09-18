package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Android 上 CGO 关闭的 Go 会读 /etc/resolv.conf，里面经常是 nameserver ::1。
// netd 并不给这种静态二进制提供 [::1]:53，于是所有上游域名解析失败。
// 这里改用系统 getprop 的 DNS，并兜底公共解析器；不影响查询结果，只换解析通道。

var (
	dnsMu      sync.Mutex
	dnsCached  []string
	dnsCachedAt time.Time
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
			d := net.Dialer{Timeout: 3 * time.Second}
			var last error
			for _, srv := range loadDNSServers() {
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