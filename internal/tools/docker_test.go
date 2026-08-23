package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDemuxDockerLog(t *testing.T) {
	frame := func(stream byte, payload string) []byte {
		header := []byte{stream, 0, 0, 0, 0, 0, 0, 0}
		binary.BigEndian.PutUint32(header[4:], uint32(len(payload)))
		return append(header, payload...)
	}
	muxed := append(frame(1, "out line\n"), frame(2, "err line\n")...)
	got := string(demuxDockerLog(muxed))
	if got != "out line\nerr line\n" {
		t.Fatalf("demuxDockerLog() = %q", got)
	}

	// TTY 容器是纯文本、没有帧头：原样返回，不强行解析。
	plain := "tty raw log\nsecond line\n"
	if got := string(demuxDockerLog([]byte(plain))); got != plain {
		t.Fatalf("demuxDockerLog(plain) = %q", got)
	}

	// 空输入。
	if got := demuxDockerLog(nil); len(got) != 0 {
		t.Fatalf("demuxDockerLog(empty) = %q", got)
	}
}

func TestValidContainerName(t *testing.T) {
	for _, ok := range []string{"sub2api", "oncallagent-mysql-1", "a.b_c-2"} {
		if err := validContainerName(ok); err != nil {
			t.Fatalf("validContainerName(%q) error = %v", ok, err)
		}
	}
	for _, bad := range []string{"", "../etc", "a/b", "a b", "-leading", strings.Repeat("x", 200)} {
		if err := validContainerName(bad); err == nil {
			t.Fatalf("validContainerName(%q) error = nil, want rejection", bad)
		}
	}
}

func TestRegisterRestartToolAllowlist(t *testing.T) {
	client, err := NewDockerClient("/var/run/docker.sock")
	if err != nil {
		t.Skip("no docker socket")
	}
	limits := RestartLimits{MinInterval: time.Minute, MaxPerHour: 3}
	// 空白名单 → 不注册。
	empty := NewRegistry()
	if err := client.RegisterRestartTool(empty, nil, limits); err != nil {
		t.Fatal(err)
	}
	if _, ok := empty.Get(ToolDockerRestart); ok {
		t.Fatal("restart tool registered with empty allowlist")
	}

	registry := NewRegistry()
	if err := client.RegisterRestartTool(registry, []string{"sub2api"}, limits); err != nil {
		t.Fatal(err)
	}
	spec, ok := registry.Get(ToolDockerRestart)
	if !ok || spec.Level != L2LowRisk {
		t.Fatalf("spec = %+v, %v", spec, ok)
	}
	// L2 工具绝不出现在 LLM 工具面。
	for _, exposed := range registry.ForLLM() {
		if exposed.Name == ToolDockerRestart {
			t.Fatal("docker_restart leaked into ForLLM")
		}
	}
	// 白名单外的容器被拒，不触达 Docker。
	if _, err := registry.Execute(context.Background(), ToolDockerRestart, json.RawMessage(`{"name":"evil"}`)); err == nil {
		t.Fatal("non-allowlisted container was not rejected")
	}
}

func TestRestartLimiterMinInterval(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	limiter := newRestartLimiter(RestartLimits{MinInterval: 5 * time.Minute, MaxPerHour: 10})
	limiter.now = func() time.Time { return now }
	if err := limiter.allow("sub2api"); err != nil {
		t.Fatalf("first restart rejected: %v", err)
	}
	// 间隔不够 → 拒绝。
	now = now.Add(time.Minute)
	if err := limiter.allow("sub2api"); err == nil {
		t.Fatal("restart within min interval was allowed")
	}
	// 别的容器不受影响：限频是按 target 记的。
	if err := limiter.allow("other"); err != nil {
		t.Fatalf("unrelated container rejected: %v", err)
	}
	// 间隔够了 → 放行。
	now = now.Add(5 * time.Minute)
	if err := limiter.allow("sub2api"); err != nil {
		t.Fatalf("restart after min interval rejected: %v", err)
	}
}

func TestRestartLimiterHourlyCap(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	limiter := newRestartLimiter(RestartLimits{MaxPerHour: 2})
	limiter.now = func() time.Time { return now }
	for i := 0; i < 2; i++ {
		if err := limiter.allow("sub2api"); err != nil {
			t.Fatalf("restart %d rejected: %v", i, err)
		}
		now = now.Add(10 * time.Minute)
	}
	if err := limiter.allow("sub2api"); err == nil {
		t.Fatal("third restart within the hour was allowed")
	}
	// 窗口滚出后重新可用。
	now = now.Add(time.Hour)
	if err := limiter.allow("sub2api"); err != nil {
		t.Fatalf("restart after window rejected: %v", err)
	}
}

// 限频必须在发请求之前拦下：socket 指向不存在的路径时，被拒的调用
// 返回限频错误而不是连接错误 —— 证明它没触达 Docker。
func TestRestartRateLimitBlocksBeforeRequest(t *testing.T) {
	client := &DockerClient{socketPath: "/nonexistent/docker.sock", httpClient: http.DefaultClient}
	limiter := newRestartLimiter(RestartLimits{MinInterval: time.Hour, MaxPerHour: 5})
	handler := client.restart(map[string]bool{"sub2api": true}, limiter)
	// 第一次会尝试连接并失败（socket 不存在），但已经记账。
	if _, err := handler(context.Background(), json.RawMessage(`{"name":"sub2api"}`)); err == nil {
		t.Fatal("restart against missing socket succeeded")
	}
	_, err := handler(context.Background(), json.RawMessage(`{"name":"sub2api"}`))
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("second restart error = %v, want rate limited", err)
	}
}

func TestRegisterRestartToolSkipsWithoutSocket(t *testing.T) {
	if _, err := NewDockerClient("/nonexistent/docker.sock"); err == nil {
		t.Fatal("missing socket accepted")
	}
}
