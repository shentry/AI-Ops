package tools

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
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

// The schema declares tail a string; a number, a numeric string and an
// oversized value are all read, and the cap still applies.
func TestDockerLogsTailAcceptsNumberOrString(t *testing.T) {
	// Unix socket paths are limited to ~104 bytes; t.TempDir embeds the test name.
	dir, err := os.MkdirTemp("", "ds")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "d.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	tails := make(chan string, 4)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tails <- r.URL.Query().Get("tail")
		fmt.Fprint(w, "2026-09-28T00:00:00Z line\n")
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close() })
	client, err := NewDockerClient(socket)
	if err != nil {
		t.Fatal(err)
	}
	logs := client.logs(200)
	for args, want := range map[string]string{`{"name":"sub2api","tail":50}`: "50", `{"name":"sub2api","tail":"50"}`: "50", `{"name":"sub2api","tail":"5000"}`: "200", `{"name":"sub2api"}`: "200"} {
		if _, err := logs(context.Background(), json.RawMessage(args)); err != nil {
			t.Fatalf("%s: %v", args, err)
		}
		if got := <-tails; got != want {
			t.Fatalf("%s: tail = %s, want %s", args, got, want)
		}
	}
	if _, err := logs(context.Background(), json.RawMessage(`{"name":"sub2api","tail":"many"}`)); err == nil {
		t.Fatal("non-numeric tail accepted")
	}
}
