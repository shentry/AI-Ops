package api

import (
	"net/http"

	"github.com/gogf/gf/v2/net/ghttp"

	"oncall-agent/internal/metrics"
)

// MetricsHandler 暴露 Prometheus 文本格式的进程指标。
// 无敏感内容，不带鉴权 —— 供内网 Prometheus 抓取。
func MetricsHandler(r *ghttp.Request) {
	r.Response.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metrics.Write(r.Response.BufferWriter)
}

// MetricsHTTP 是标准库形态，供测试直接调用。
func MetricsHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	metrics.Write(w)
}
