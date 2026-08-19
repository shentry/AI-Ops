//go:build tools

// 钉住 github.com/gogf/gf/v2，避免 go mod tidy 丢掉 D03 的 HTTP 依赖。
// tools 这个 build tag 让它不进 D01 运行路径。
package tools

import _ "github.com/gogf/gf/v2/frame/g"
