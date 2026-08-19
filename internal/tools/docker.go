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

// Docker 只读工具名。D11 的受控重启是独立的 L2 工具，不在这里出现。
const (
	ToolDockerInspect = "docker_inspect"
	ToolDockerLogs    = "docker_logs"
	// ToolDockerRestart 是 D11 的 L2 变更动作：受控重启，目标受白名单约束。
	ToolDockerRestart = "docker_restart"
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

// RegisterRestartTool 登记 docker_restart（L2）。target 必须命中白名单
// （GC-11：只对配置允许的 Sub2API 运行对象执行），白名单为空则不注册 ——
// 没有可重启目标的部署形态下，这个动作在系统里不存在。
func (c *DockerClient) RegisterRestartTool(registry *Registry, allowedContainers []string) error {
	if len(allowedContainers) == 0 {
		return nil
	}
	allowed := make(map[string]bool, len(allowedContainers))
	for _, name := range allowedContainers {
		allowed[name] = true
	}
	return registry.Register(ToolSpec{
		Name:        ToolDockerRestart,
		Description: "Restart a container (L2, requires approval unless guardrails allow auto). Args: {name} or plan target_name.",
		Level:       L2LowRisk,
		Timeout:     30 * time.Second,
		MaxOutput:   1024,
		Params: []ParamSpec{
			{Name: "name", Description: "container name from the allowlist", Required: true},
		},
		Handler: c.restart(allowed),
	})
}

// RegisterTools 登记 docker_inspect 和 docker_logs 两个 L1 只读工具。
// maxLogLines 是单次日志条数上限，由 evidence 配置统一下发。
func (c *DockerClient) RegisterTools(registry *Registry, maxLogLines int) error {
	if maxLogLines <= 0 {
		maxLogLines = 200
	}
	specs := []ToolSpec{
		{
			Name:        ToolDockerInspect,
			Description: "Inspect a container's state. Returns status/restarts/timestamps as JSON.",
			Level:       L1ReadOnly,
			Timeout:     promToolTimeout,
			MaxOutput:   defaultMaxOutput,
			Params: []ParamSpec{
				{Name: "name", Description: "container name", Required: true},
			},
			Handler: c.inspect,
		},
		{
			Name:        ToolDockerLogs,
			Description: "Read bounded container logs. Never follows.",
			Level:       L1ReadOnly,
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
		return nil, errContainerNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("docker returned HTTP %d", resp.StatusCode)
	}
	return body, nil
}

var errContainerNotFound = errors.New("container not found")

type dockerContainerJSON struct {
	Name  string `json:"Name"`
	State struct {
		Status       string    `json:"Status"`
		Running      bool      `json:"Running"`
		OOMKilled    bool      `json:"OOMKilled"`
		ExitCode     int       `json:"ExitCode"`
		StartedAt    time.Time `json:"StartedAt"`
		FinishedAt   time.Time `json:"FinishedAt"`
		RestartCount int       `json:"RestartCount"`
	} `json:"State"`
	Config struct {
		Image string `json:"Image"`
	} `json:"Config"`
	RestartCount int `json:"RestartCount"`
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
	if err := validContainerName(args.Name); err != nil {
		return "", err
	}
	body, err := c.get(ctx, "/containers/"+args.Name+"/json", nil)
	if err != nil {
		return "", err
	}
	var full dockerContainerJSON
	if err := json.Unmarshal(body, &full); err != nil {
		return "", fmt.Errorf("decode inspect: %w", err)
	}
	// 只透出诊断要用的状态字段，不把整个 inspect（可能含环境变量里的密钥）外发。
	out, err := json.Marshal(map[string]any{
		"name":          strings.TrimPrefix(full.Name, "/"),
		"image":         full.Config.Image,
		"status":        full.State.Status,
		"running":       full.State.Running,
		"oom_killed":    full.State.OOMKilled,
		"exit_code":     full.State.ExitCode,
		"started_at":    full.State.StartedAt,
		"finished_at":   full.State.FinishedAt,
		"restart_count": full.RestartCount,
	})
	if err != nil {
		return "", fmt.Errorf("encode inspect: %w", err)
	}
	return string(out), nil
}

type dockerLogsArgs struct {
	Name  string `json:"name"`
	Tail  int    `json:"tail"`
	Since string `json:"since"` // RFC3339
}

// logs 读受限日志：tail 封顶、since 限窗口、绝不 follow。
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
		if args.Tail <= 0 || args.Tail > maxLogLines {
			args.Tail = maxLogLines
		}
		query := url.Values{
			"stdout": {"1"},
			"stderr": {"1"},
			"tail":   {fmt.Sprintf("%d", args.Tail)},
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
		return string(demuxDockerLog(body)), nil
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

// post 调 Engine API 的写端点（目前仅 restart）。与 get 同一纪律：
// 限量读、错误不带 URL。
func (c *DockerClient) post(ctx context.Context, path string, query url.Values) ([]byte, error) {
	endpoint := &url.URL{Scheme: "http", Host: "docker", Path: path, RawQuery: query.Encode()}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
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
	if resp.StatusCode == http.StatusNotFound {
		return nil, errContainerNotFound
	}
	// 重启成功返回 204。
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return nil, fmt.Errorf("docker returned HTTP %d", resp.StatusCode)
	}
	return body, nil
}

// restart 是受控重启 handler：白名单 + 名字形态双重校验，
// 幂等（重启一个运行中的容器结果是确定的）。t=10 给进程 10 秒优雅退出。
func (c *DockerClient) restart(allowed map[string]bool) Handler {
	return func(ctx context.Context, raw json.RawMessage) (string, error) {
		var args struct {
			Name       string `json:"name"`
			TargetName string `json:"target_name"`
		}
		if err := json.Unmarshal(raw, &args); err != nil {
			return "", fmt.Errorf("invalid args: %w", err)
		}
		name := args.Name
		if name == "" {
			name = args.TargetName
		}
		if err := validContainerName(name); err != nil {
			return "", err
		}
		// 白名单是硬约束：配置之外的容器名一律拒绝（GC-11）。
		if !allowed[name] {
			return "", fmt.Errorf("container %q is not in the restart allowlist", name)
		}
		if _, err := c.post(ctx, "/containers/"+name+"/restart", url.Values{"t": {"10"}}); err != nil {
			return "", err
		}
		out, err := json.Marshal(map[string]any{"restarted": name})
		if err != nil {
			return "", fmt.Errorf("encode restart result: %w", err)
		}
		return string(out), nil
	}
}
