package tools

import (
	"encoding/binary"
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

func TestNewDockerClientRequiresSocket(t *testing.T) {
	if _, err := NewDockerClient("/nonexistent/docker.sock"); err == nil {
		t.Fatal("missing socket accepted")
	}
}
