package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestBusinessTrafficRequiresFreshCompleteMetrics(t *testing.T) {
	for _, scenario := range []string{"healthy", "zero traffic", "missing", "failed exporter", "stale", "NaN", "inconsistent", "multiple targets"} {
		t.Run(scenario, func(t *testing.T) {
			registry := NewRegistry()
			err := registry.Register(ToolSpec{Name: ToolPromInstantQuery, Description: "metrics", Timeout: time.Second, Handler: func(_ context.Context, raw json.RawMessage) (string, error) {
				var args map[string]string
				_ = json.Unmarshal(raw, &args)
				if !strings.Contains(args["query"], "timestamp(") || args["time"] == "" {
					t.Fatal("query does not constrain underlying scrape age")
				}
				args["query"] = strings.SplitN(strings.TrimPrefix(args["query"], "("), ") and", 2)[0]
				value := "1"
				switch args["query"] {
				case `sub2api_requests_5m{class="sla"}`:
					value = "100"
				case `sub2api_errors_5m{class="sla"}`:
					value = "0"
				}
				at := time.Now().Unix()
				switch scenario {
				case "zero traffic":
					if args["query"] != `sub2api_ops_up{endpoint="overview"}` {
						value = "0"
					}
				case "missing":
					return `{"resultType":"vector","result":[]}`, nil
				case "failed exporter":
					if args["query"] == `sub2api_ops_up{endpoint="overview"}` {
						value = "0"
					}
				case "stale":
					at -= 600
				case "NaN":
					value = "NaN"
				case "inconsistent":
					if args["query"] == `sub2api_errors_5m{class="sla"}` {
						value = "101"
					}
				case "multiple targets":
					return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,"10"]},{"value":[%d,"10"]}]}`, at, at), nil
				}
				return fmt.Sprintf(`{"resultType":"vector","result":[{"value":[%d,%q]}]}`, at, value), nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			traffic, err := ReadBusinessTraffic(context.Background(), registry)
			want := scenario == "healthy" || scenario == "zero traffic"
			if (err == nil) != want {
				t.Fatalf("traffic=%+v err=%v", traffic, err)
			}
		})
	}
}
