// ring.go 固定容量的行级日志环形缓冲（并发安全，实现 io.Writer）。
// main 把 log 包输出与 chat 表格日志经 MultiWriter 镜像进来，面板
// /panel/api/logs 读取快照；超出容量的旧行按 FIFO 淘汰。
package panel

import (
	"strings"
	"sync"
)

// Ring 日志环形缓冲。
type Ring struct {
	mu    sync.Mutex
	lines []string
	cap   int
}

// NewRing 构建容量为 capacity 的日志环（非正值回退 500）。
func NewRing(capacity int) *Ring {
	if capacity <= 0 {
		capacity = 500
	}
	return &Ring{cap: capacity}
}

// Write 按 \n 切分入环（实现 io.Writer）。空行丢弃；超容量淘汰最旧行。
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, line := range strings.Split(strings.TrimRight(string(p), "\r\n"), "\n") {
		if line == "" {
			continue
		}
		r.lines = append(r.lines, line)
		if overflow := len(r.lines) - r.cap; overflow > 0 {
			r.lines = r.lines[overflow:]
		}
	}
	return len(p), nil
}

// Snapshot 按写入顺序返回缓冲内全部行（拷贝，调用方可安全持有）。
func (r *Ring) Snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.lines))
	copy(out, r.lines)
	return out
}
