package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"oncall-agent/internal/store"
)

type fakeApprovalService struct {
	decideErr      error
	decidedIDs     []uint64
	approver       bool
	operator       string
	decisionReason string
	decisionSource string
	listStatus     string
	getID          uint64
	getErr         error
	planHash       string
	row            store.Approval
}

func (f *fakeApprovalService) Decide(_ context.Context, id uint64, approve bool, expectedPlanHash, decidedBy, decisionReason, decisionSource string) (store.Approval, error) {
	f.decidedIDs = append(f.decidedIDs, id)
	f.approver = approve
	f.planHash = expectedPlanHash
	f.operator = decidedBy
	f.decisionReason = decisionReason
	f.decisionSource = decisionSource
	status := "denied"
	if approve {
		status = "approved"
	}
	row := f.row
	row.ID, row.Status = id, status
	return row, f.decideErr
}

func (f *fakeApprovalService) Get(_ context.Context, id uint64) (store.Approval, error) {
	f.getID = id
	if f.getErr != nil {
		return store.Approval{}, f.getErr
	}
	if f.row.ID != 0 {
		return f.row, nil
	}
	return store.Approval{ID: id, Status: "pending"}, nil
}

func (f *fakeApprovalService) List(_ context.Context, status string) ([]store.Approval, error) {
	f.listStatus = status
	return []store.Approval{{ID: 1, Status: "pending"}}, nil
}
func approvalRequest(t *testing.T, api *ApprovalAPI, method, path, token, operator string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(`{"plan_hash":"expected-snapshot","reason":""}`))
	if token != "" {
		req.Header.Set("Authorization", token)
	}
	if operator != "" {
		req.Header.Set("X-Operator", operator)
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, req)
	return response
}

func TestApprovalAPIAuthAndParams(t *testing.T) {
	svc := &fakeApprovalService{}
	api := NewApprovalAPI(svc, "secret", nil)

	// 无 token 401。
	if resp := approvalRequest(t, api, http.MethodPost, "/api/v1/approvals/1/approve", "", "ops"); resp.Code != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.Code)
	}
	// 缺 X-Operator 400。
	if resp := approvalRequest(t, api, http.MethodPost, "/api/v1/approvals/1/approve", "Bearer secret", ""); resp.Code != http.StatusBadRequest {
		t.Fatalf("no operator = %d, want 400", resp.Code)
	}
	// 非法 id 400。
	if resp := approvalRequest(t, api, http.MethodPost, "/api/v1/approvals/abc/approve", "Bearer secret", "ops"); resp.Code != http.StatusBadRequest {
		t.Fatalf("bad id = %d, want 400", resp.Code)
	}
	// GET approve 405。
	if resp := approvalRequest(t, api, http.MethodGet, "/api/v1/approvals/1/approve", "Bearer secret", "ops"); resp.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET approve = %d, want 405", resp.Code)
	}
}

func TestApprovalAPIDecide(t *testing.T) {
	svc := &fakeApprovalService{row: completeApproval(t)}
	api := NewApprovalAPI(svc, "secret", nil)
	resp := approvalRequest(t, api, http.MethodPost, "/api/v1/approvals/7/approve", "Bearer secret", "ops-li")
	if resp.Code != http.StatusOK {
		t.Fatalf("approve = %d, want 200", resp.Code)
	}
	if len(svc.decidedIDs) != 1 || svc.decidedIDs[0] != 7 || !svc.approver || svc.operator != "ops-li" || svc.decisionReason != "" || svc.decisionSource != "api" || svc.planHash != "expected-snapshot" {
		t.Fatalf("svc state = %+v", svc)
	}

	// deny 路径。
	svc2 := &fakeApprovalService{}
	api2 := NewApprovalAPI(svc2, "secret", nil)
	if resp := approvalRequest(t, api2, http.MethodPost, "/api/v1/approvals/8/deny", "Bearer secret", "ops-li"); resp.Code != http.StatusOK || svc2.approver {
		t.Fatalf("deny = %d, approver = %v", resp.Code, svc2.approver)
	}
}

func TestApprovalAPIConflictAndNotFound(t *testing.T) {
	// 重复决策 409。
	svc := &fakeApprovalService{decideErr: store.ErrApprovalConflict}
	api := NewApprovalAPI(svc, "secret", nil)
	if resp := approvalRequest(t, api, http.MethodPost, "/api/v1/approvals/7/approve", "Bearer secret", "ops"); resp.Code != http.StatusConflict {
		t.Fatalf("conflict = %d, want 409", resp.Code)
	}
	// 不存在 404。
	svc.decideErr = store.ErrApprovalNotFound
	if resp := approvalRequest(t, api, http.MethodPost, "/api/v1/approvals/7/approve", "Bearer secret", "ops"); resp.Code != http.StatusNotFound {
		t.Fatalf("not found = %d, want 404", resp.Code)
	}
	// 存储故障 503。
	svc.decideErr = errors.New("db down")
	if resp := approvalRequest(t, api, http.MethodPost, "/api/v1/approvals/7/approve", "Bearer secret", "ops"); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("db error = %d, want 503", resp.Code)
	}
}

func TestApprovalAPIList(t *testing.T) {
	svc := &fakeApprovalService{}
	api := NewApprovalAPI(svc, "secret", nil)
	resp := approvalRequest(t, api, http.MethodGet, "/api/v1/approvals?status=pending", "Bearer secret", "")
	if resp.Code != http.StatusOK || svc.listStatus != "pending" {
		t.Fatalf("list = %d, status = %q", resp.Code, svc.listStatus)
	}
}

func TestApprovalAPIAllowsAnonymousConsoleDecision(t *testing.T) {
	svc := &fakeApprovalService{}
	api := NewApprovalAPI(svc, "secret", NewConsole())
	req := httptest.NewRequest(http.MethodPost, "/api/v1/approvals/11/approve", strings.NewReader(`{"plan_hash":"expected-snapshot","reason":"public console"}`))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	api.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("anonymous approve = %d body=%s", resp.Code, resp.Body.String())
	}
	if svc.operator != "anonymous" || svc.decisionSource != "web" || svc.decisionReason != "public console" || svc.planHash != "expected-snapshot" {
		t.Fatalf("anonymous decision = %+v", svc)
	}
}

func TestApprovalDecisionRequiresHashAndReasonBody(t *testing.T) {
	for _, body := range []string{"", `null`, `{}`, `{"reason":"approve"}`, `{"plan_hash":"" ,"reason":""}`, `{"plan_hash":"   ","reason":""}`, `{"plan_hash":"abc"}`, `{"plan_hash":"abc","reason":null}`, `{"plan_hash":42,"reason":""}`, `{"plan_hash":"abc","reason":""} {}`, strings.Repeat(" ", 64<<10) + `{}`} {
		for _, action := range []string{"approve", "deny"} {
			svc := &fakeApprovalService{}
			api := NewApprovalAPI(svc, "secret", NewConsole())
			resp := httptest.NewRecorder()
			api.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/v1/approvals/7/"+action, strings.NewReader(body)))
			if resp.Code != http.StatusBadRequest || len(svc.decidedIDs) != 0 {
				t.Fatalf("%s body %q: code=%d calls=%v", action, body, resp.Code, svc.decidedIDs)
			}
		}
	}
}

func TestApprovalAPIPropagatesSnapshotConflicts(t *testing.T) {
	for _, kind := range []string{"legacy", "changed_snapshot", "expired", "already_decided"} {
		t.Run(kind, func(t *testing.T) {
			row := completeApproval(t)
			if kind == "legacy" {
				row.ExecutionContext = nil
			}
			svc := &fakeApprovalService{row: row, decideErr: store.ErrApprovalConflict}
			resp := approvalRequest(t, NewApprovalAPI(svc, "secret", NewConsole()), http.MethodPost, "/api/v1/approvals/7/approve", "", "")
			if resp.Code != http.StatusConflict || svc.planHash != "expected-snapshot" || svc.getID != 0 {
				t.Fatalf("conflict must be validated by Decide: %d service=%+v", resp.Code, svc)
			}
		})
	}
}

func TestApprovalGetReturnsVerification(t *testing.T) {
	row := completeApproval(t)
	row.Status = "executed"
	row.Verification = &store.VerifyTask{ApprovalID: row.ID, Status: "passed"}
	svc := &fakeApprovalService{row: row}
	resp := approvalRequest(t, NewApprovalAPI(svc, "secret", NewConsole()), http.MethodGet, "/api/v1/approvals/7", "", "")
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"verification":{"status":"passed"`) {
		t.Fatalf("GET: %d %s", resp.Code, resp.Body.String())
	}
	for _, forbidden := range []string{"base_url", "private-target.invalid", "args_json", "execution_context"} {
		if strings.Contains(resp.Body.String(), forbidden) {
			t.Fatalf("leaked %s", forbidden)
		}
	}
}
