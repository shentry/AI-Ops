package feishu

import (
	"context"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"oncall-agent/internal/store"
)

type fakeReceipts struct {
	claimed map[string]bool
}

func (f *fakeReceipts) ClaimIntegrationEventReceipt(_ context.Context, receipt store.IntegrationEventReceipt) (bool, error) {
	if f.claimed == nil {
		f.claimed = map[string]bool{}
	}
	if f.claimed[receipt.EventID] {
		return false, nil
	}
	f.claimed[receipt.EventID] = true
	return true, nil
}
func (f *fakeReceipts) CompleteIntegrationEventReceipt(context.Context, string, string, time.Time) error {
	return nil
}

type fakeApproval struct {
	row       store.Approval
	decided   int
	decideErr error
}

func (f *fakeApproval) Get(context.Context, uint64) (store.Approval, error) {
	return f.row, nil
}

func (f *fakeApproval) Decide(context.Context, uint64, bool, string, string, string) (store.Approval, error) {
	f.decided++
	if f.decideErr != nil {
		return store.Approval{}, f.decideErr
	}
	row := f.row
	row.Status = "approved"
	return row, nil
}

func cardEvent(eventID, openID, action string, approvalID uint64, planHash string) *callback.CardActionTriggerEvent {
	return &callback.CardActionTriggerEvent{
		EventV2Base: &larkevent.EventV2Base{Header: &larkevent.EventHeader{EventID: eventID}},
		Event: &callback.CardActionTriggerRequest{
			Operator: &callback.Operator{OpenID: openID},
			Action: &callback.CallBackAction{Value: map[string]any{
				"action": action, "approval_id": approvalID, "plan_hash": planHash,
			}},
			Context: &callback.Context{OpenMessageID: "om_card"},
		},
	}
}

func TestCardActionApproveUsesSharedDecide(t *testing.T) {
	approval := &fakeApproval{row: store.Approval{ID: 7, Status: "pending", PlanHash: "abc"}}
	business := NewCallbackBusiness(BusinessDependencies{
		Receipts:          &fakeReceipts{},
		Approval:          approval,
		OperatorAllowlist: []string{"ou_ops"},
	})
	resp, err := business.CardAction(context.Background(), cardEvent("evt-1", "ou_ops", "approve", 7, "abc"))
	if err != nil {
		t.Fatalf("CardAction: %v", err)
	}
	if approval.decided != 1 {
		t.Fatalf("Decide calls = %d", approval.decided)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "审批已批准" {
		t.Fatalf("toast = %+v", resp)
	}
}

func TestCardActionDuplicateReceiptIsIdempotent(t *testing.T) {
	approval := &fakeApproval{row: store.Approval{ID: 7, Status: "pending", PlanHash: "abc"}}
	receipts := &fakeReceipts{}
	business := NewCallbackBusiness(BusinessDependencies{
		Receipts:          receipts,
		Approval:          approval,
		OperatorAllowlist: []string{"ou_ops"},
	})
	event := cardEvent("evt-dup", "ou_ops", "approve", 7, "abc")
	if _, err := business.CardAction(context.Background(), event); err != nil {
		t.Fatalf("first CardAction: %v", err)
	}
	resp, err := business.CardAction(context.Background(), event)
	if err != nil {
		t.Fatalf("second CardAction: %v", err)
	}
	if approval.decided != 1 {
		t.Fatalf("Decide calls = %d", approval.decided)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "该操作已处理" {
		t.Fatalf("toast = %+v", resp)
	}
}

func TestCardActionRequestEvidenceIsRejectedWithoutFalseSuccess(t *testing.T) {
	approval := &fakeApproval{row: store.Approval{ID: 7, Status: "pending", PlanHash: "abc"}}
	business := NewCallbackBusiness(BusinessDependencies{
		Receipts:          &fakeReceipts{},
		Approval:          approval,
		OperatorAllowlist: []string{"ou_ops"},
	})
	resp, err := business.CardAction(context.Background(), cardEvent("evt-2", "ou_ops", approvalActionRequestProof, 7, "abc"))
	if err != nil {
		t.Fatalf("CardAction: %v", err)
	}
	if approval.decided != 0 {
		t.Fatalf("Decide should not run, got %d", approval.decided)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "error" || resp.Toast.Content != "请在 Web 控制室请求补充证据" {
		t.Fatalf("toast = %+v", resp)
	}
}

func TestCardActionRejectsUnknownOperator(t *testing.T) {
	approval := &fakeApproval{row: store.Approval{ID: 7, Status: "pending", PlanHash: "abc"}}
	business := NewCallbackBusiness(BusinessDependencies{
		Receipts:          &fakeReceipts{},
		Approval:          approval,
		OperatorAllowlist: []string{"ou_ops"},
	})
	resp, err := business.CardAction(context.Background(), cardEvent("evt-3", "ou_other", "approve", 7, "abc"))
	if err != nil {
		t.Fatalf("CardAction: %v", err)
	}
	if approval.decided != 0 {
		t.Fatalf("Decide calls = %d", approval.decided)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "error" {
		t.Fatalf("toast = %+v", resp)
	}
}
