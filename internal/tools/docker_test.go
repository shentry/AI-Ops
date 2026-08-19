package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"strings"
	"testing"
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
	// 空白名单 → 不注册。
	empty := NewRegistry()
	if err := client.RegisterRestartTool(empty, nil); err != nil {
		t.Fatal(err)
	}
	if _, ok := empty.Get(ToolDockerRestart); ok {
		t.Fatal("restart tool registered with empty allowlist")
	}

	registry := NewRegistry()
	if err := client.RegisterRestartTool(registry, []string{"sub2api"}); err != nil {
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

func TestRegisterRestartToolSkipsWithoutSocket(t *testing.T) {
	if _, err := NewDockerClient("/nonexistent/docker.sock"); err == nil {
		t.Fatal("missing socket accepted")
	}
}
