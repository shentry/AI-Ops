package tools

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

// Docker 只读工具名。受控重启是动作（action_restart.go），不在工具面出现。
const (
	ToolDockerInspect = "docker_inspect"
	ToolDockerLogs    = "docker_logs"
)

// containerNamePattern 防 URL 路径注入：容器名只允许字母数字和 _.-
var containerNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)

// DockerClient 通过 unix socket 调 Docker Engine API 的只读端点。
// 不引 Docker SDK：V1 只需要 inspect 和 logs 两个 GET，一个
// net/http over unix socket 的薄客户端更可控、可审计。
type DockerClient struct {
	socketPath string
	httpClient *http.Client
}

// NewDockerClient 校验 socket 存在。不存在不算进程级错误 —— 没装 Docker 的
// 环境里证据 collector 记录缺失即可（D07 容错原则）。
func NewDockerClient(socketPath string) (*DockerClient, error) {
	if strings.TrimSpace(socketPath) == "" {
		return nil, errors.New("tools: docker socket path is required")
	}
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("tools: docker socket: %w", err)
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
		Timeout: promToolTimeout + 5*time.Second,
	}
	return &DockerClient{socketPath: socketPath, httpClient: httpClient}, nil
}

// RegisterTools 登记 docker_inspect 和 docker_logs 两个只读工具。
// maxLogLines 是单次日志条数上限，由 evidence 配置统一下发。
func (c *DockerClient) RegisterTools(registry *Registry, maxLogLines int) error {
	if maxLogLines <= 0 {
		maxLogLines = 200
	}
	specs := []ToolSpec{
		{
			Name:        ToolDockerInspect,
			Description: "Inspect a container's state. Returns id/image/repo digests/status/restart policy/restarts/timestamps as JSON.",
			Timeout:     promToolTimeout,
			MaxOutput:   defaultMaxOutput,
			Params: []ParamSpec{
				{Name: "name", Description: "container name", Required: true},
			},
			Handler: c.inspect,
		},
		{
			Name:        ToolDockerLogs,
			Description: "Read bounded container logs aggregated by pattern: count, first/last Docker timestamp and latest sample, most recent first. Never follows.",
			Timeout:     promToolTimeout,
			MaxOutput:   defaultMaxOutput,
			Params: []ParamSpec{
				{Name: "name", Description: "container name", Required: true},
				{Name: "tail", Description: "max log lines, optional"},
				{Name: "since", Description: "RFC3339 window start, optional"},
			},
			Handler: c.logs(maxLogLines),
		},
	}
	for _, spec := range specs {
		if err := registry.Register(spec); err != nil {
			return err
		}
	}
	return nil
}

// get 调 Engine API。host 是占位（unix socket 没有主机概念），
// 错误文本不带 URL 细节，与 Prometheus 客户端同一纪律。
func (c *DockerClient) get(ctx context.Context, path string, query url.Values) ([]byte, error) {
	endpoint := &url.URL{Scheme: "http", Host: "docker", Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxResponseBytes)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker returned HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// ErrContainerNotFound means Docker answered that no such container exists,
// as opposed to Docker being unreachable.
var ErrContainerNotFound = errors.New("container not found")

type dockerContainerJSON struct {
	ID    string `json:"Id"`
	Name  string `json:"Name"`
	Image string `json:"Image"` // 实际运行的镜像 ID（sha256:…），标签会漂移，ID 不会
	State struct {
		Status     string    `json:"Status"`
		Running    bool      `json:"Running"`
		Restarting bool      `json:"Restarting"`
		OOMKilled  bool      `json:"OOMKilled"`
		ExitCode   int       `json:"ExitCode"`
		StartedAt  time.Time `json:"StartedAt"`
		FinishedAt time.Time `json:"FinishedAt"`
		Health     *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	HostConfig struct {
		RestartPolicy struct {
			Name string `json:"Name"`
		} `json:"RestartPolicy"`
	} `json:"HostConfig"`
	RestartCount int `json:"RestartCount"`
}

// ContainerInspect 是 docker_inspect 的输出契约。只含诊断和动作前提要用的字段，
// 不把整个 inspect（可能含环境变量里的密钥）外发。证据采集按这个结构解析。
type ContainerInspect struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Image         string    `json:"image"`
	ImageID       string    `json:"image_id"`
	Status        string    `json:"status"`
	Running       bool      `json:"running"`
	Restarting    bool      `json:"restarting"`
	OOMKilled     bool      `json:"oom_killed"`
	ExitCode      int       `json:"exit_code"`
	StartedAt     time.Time `json:"started_at"`
	FinishedAt    time.Time `json:"finished_at"`
	RestartCount  int       `json:"restart_count"`
	RestartPolicy string    `json:"restart_policy"`
	Health        string    `json:"health,omitempty"`
	// RepoDigests are the registry digests of the running image: the identity
	// release records and rollback verification compare, unlike drifting tags.
	RepoDigests []string `json:"repo_digests,omitempty"`
}

type dockerNameArgs struct {
	Name string `json:"name"`
}

func validContainerName(name string) error {
	if !containerNamePattern.MatchString(name) {
		return fmt.Errorf("invalid container name %q", name)
	}
	return nil
}

func (c *DockerClient) inspect(ctx context.Context, raw json.RawMessage) (string, error) {
	var args dockerNameArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", fmt.Errorf("invalid args: %w", err)
	}
	result, err := c.Inspect(ctx, args.Name)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(result)
	if err != nil {
		return "", fmt.Errorf("encode inspect: %w", err)
	}
	return string(out), nil
}

// Inspect reads the container and its image digests. Actions use this typed
// form; the model and collectors see the same content through docker_inspect.
func (c *DockerClient) Inspect(ctx context.Context, name string) (ContainerInspect, error) {
	if err := validContainerName(name); err != nil {
		return ContainerInspect{}, err
	}
	body, err := c.get(ctx, "/containers/"+name+"/json", nil)
	if err != nil {
		return ContainerInspect{}, err
	}
	var full dockerContainerJSON
	if err := json.Unmarshal(body, &full); err != nil {
		return ContainerInspect{}, fmt.Errorf("decode inspect: %w", err)
	}
	result := ContainerInspect{
		ID: full.ID, Name: strings.TrimPrefix(full.Name, "/"), Image: full.Config.Image, ImageID: full.Image,
		Status: full.State.Status, Running: full.State.Running, Restarting: full.State.Restarting,
		OOMKilled: full.State.OOMKilled, ExitCode: full.State.ExitCode,
		StartedAt: full.State.StartedAt, FinishedAt: full.State.FinishedAt,
		RestartCount: full.RestartCount, RestartPolicy: full.HostConfig.RestartPolicy.Name,
	}
	if full.State.Health != nil {
		result.Health = full.State.Health.Status
	}
	if full.Image != "" {
		image, err := c.get(ctx, "/images/"+url.PathEscape(full.Image)+"/json", nil)
		if err != nil {
			return ContainerInspect{}, fmt.Errorf("inspect image: %w", err)
		}
		var meta struct {
			RepoDigests []string `json:"RepoDigests"`
		}
		if err := json.Unmarshal(image, &meta); err != nil {
			return ContainerInspect{}, fmt.Errorf("decode image: %w", err)
		}
		result.RepoDigests = meta.RepoDigests
	}
	return result, nil
}

type dockerLogsArgs struct {
	Name  string          `json:"name"`
	Tail  json.RawMessage `json:"tail"`
	Since string          `json:"since"` // RFC3339
}

// logs 读受限日志：tail 封顶、since 限窗口、绝不 follow，按模式聚合后返回。
// 非 TTY 容器的日志流是多路复用格式（8 字节帧头），需要解帧成纯文本。
func (c *DockerClient) logs(maxLogLines int) Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var args dockerLogsArgs
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		if err := validContainerName(args.Name); err != nil {
			return "", err
		}
		tail, err := IntArg(args.Tail)
		if err != nil {
			return "", fmt.Errorf("tail must be an integer: %w", err)
		}
		if tail <= 0 || tail > maxLogLines {
			tail = maxLogLines
		}
		// timestamps=1：每行带 Docker 记录的 RFC3339Nano 时间，
		// 聚合日志模式时才有可信的首次/末次出现时间。
		query := url.Values{
			"stdout":     {"1"},
			"stderr":     {"1"},
			"timestamps": {"1"},
			"tail":       {fmt.Sprintf("%d", tail)},
		}
		if strings.TrimSpace(args.Since) != "" {
			since, err := time.Parse(time.RFC3339, args.Since)
			if err != nil {
				return "", fmt.Errorf("since must be RFC3339: %w", err)
			}
			query.Set("since", fmt.Sprintf("%d", since.Unix()))
		}
		body, err := c.get(ctx, "/containers/"+args.Name+"/logs", query)
		if err != nil {
			return "", err
		}
		return aggregateLogLines(string(demuxDockerLog(body))), nil
	}
}

// demuxDockerLog 解 Docker 的多路复用日志帧：[stream,0,0,0,size(4B BE)]payload。
// 遇到不像帧头的内容（TTY 容器是纯文本）就按原文返回，不强行解析。
func demuxDockerLog(raw []byte) []byte {
	var out strings.Builder
	reader := bufio.NewReader(strings.NewReader(string(raw)))
	for {
		header := make([]byte, 8)
		if _, err := io.ReadFull(reader, header); err != nil {
			break
		}
		size := binary.BigEndian.Uint32(header[4:])
		// 帧头合理性校验：stream 类型只能是 0/1/2，size 不能越过剩余长度。
		if header[0] > 2 || header[1] != 0 || header[2] != 0 || header[3] != 0 || size > uint32(len(raw)) {
			return raw
		}
		payload := make([]byte, size)
		if _, err := io.ReadFull(reader, payload); err != nil {
			return raw
		}
		out.Write(payload)
	}
	if out.Len() == 0 && len(raw) > 0 {
		return raw
	}
	return []byte(out.String())
}

// Restart asks Docker to restart the container once, giving the process ten
// seconds to exit gracefully. Only the restart action calls it.
func (c *DockerClient) Restart(ctx context.Context, name string) error {
	if err := validContainerName(name); err != nil {
		return err
	}
	endpoint := &url.URL{Scheme: "http", Host: "docker", Path: "/containers/" + name + "/restart", RawQuery: url.Values{"t": {"10"}}.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBytes))
	if resp.StatusCode == http.StatusNotFound {
		return ErrContainerNotFound
	}
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		return fmt.Errorf("docker returned HTTP %d", resp.StatusCode)
	}
	return nil
}
