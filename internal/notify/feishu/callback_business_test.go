package feishu

import (
	"context"
	"strings"
	"testing"
	"time"

	larkevent "github.com/larksuite/oapi-sdk-go/v3/event"
	"github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"oncall-agent/internal/notify"
	"oncall-agent/internal/store"
)

// fakeCallbackStore implements the whole CallbackStore contract. Receipt claims
// are the only behaviour these tests exercise; the remaining methods record
// nothing and must stay present so the fake compiles against the real contract.
type fakeCallbackStore struct {
	claimed map[string]bool
	events  []store.IncidentEvent
}

func (f *fakeCallbackStore) ClaimIntegrationEventReceipt(_ context.Context, receipt store.IntegrationEventReceipt) (bool, error) {
	if f.claimed == nil {
		f.claimed = map[string]bool{}
	}
	if f.claimed[receipt.EventID] {
		return false, nil
	}
	f.claimed[receipt.EventID] = true
	return true, nil
}

func (f *fakeCallbackStore) CompleteIntegrationEventReceipt(context.Context, string, string, time.Time) error {
	return nil
}

func (f *fakeCallbackStore) DeleteIntegrationEventReceipt(_ context.Context, eventID string) error {
	delete(f.claimed, eventID)
	return nil
}

func (f *fakeCallbackStore) CreateIMBinding(_ context.Context, binding store.IMBinding) (store.IMBinding, error) {
	return binding, nil
}

func (f *fakeCallbackStore) FindIMBinding(context.Context, string, string, string, string, string) (store.IMBinding, error) {
	return store.IMBinding{}, ErrCallbackBinding
}

func (f *fakeCallbackStore) AppendIncidentEvent(_ context.Context, event store.IncidentEvent) (store.IncidentEvent, error) {
	f.events = append(f.events, event)
	return event, nil
}

type fakeApproval struct {
	row            store.Approval
	decided        int
	decideErr      error
	expectedHash   string
	operator       string
	source         string
	mutateAfterGet bool
}

func (f *fakeApproval) Get(context.Context, uint64) (store.Approval, error) {
	row := f.row
	if f.mutateAfterGet {
		f.row.PlanHash = "changed-after-read"
	}
	return row, nil
}

func (f *fakeApproval) Decide(_ context.Context, _ uint64, _ bool, expectedHash, operator, _ string, source string) (store.Approval, error) {
	f.decided++
	f.expectedHash = expectedHash
	f.operator = operator
	f.source = source
	if expectedHash == "" || expectedHash != f.row.PlanHash {
		return store.Approval{}, store.ErrApprovalConflict
	}
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
	approval := &fakeApproval{row: completeCardApproval(t)}
	business := NewCallbackBusiness(BusinessDependencies{
		Store:             &fakeCallbackStore{},
		Approval:          approval,
		OperatorAllowlist: []string{"ou_ops"},
	})
	resp, err := business.CardAction(context.Background(), cardEvent("evt-1", "ou_ops", "approve", 7, approval.row.PlanHash))
	if err != nil {
		t.Fatalf("CardAction: %v", err)
	}
	if approval.decided != 1 || approval.expectedHash != approval.row.PlanHash || approval.operator != "feishu:ou_ops" || approval.source != "feishu" {
		t.Fatalf("Decide = %+v", approval)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Content != "审批已批准" {
		t.Fatalf("toast = %+v", resp)
	}
}

func TestCardActionRejectsStaleOrMissingHash(t *testing.T) {
	for _, hash := range []string{"", "stale"} {
		approval := &fakeApproval{row: completeCardApproval(t)}
		business := NewCallbackBusiness(BusinessDependencies{Store: &fakeCallbackStore{}, Approval: approval, OperatorAllowlist: []string{"ou_ops"}})
		resp, err := business.CardAction(context.Background(), cardEvent("stale-"+hash, "ou_ops", "approve", 7, hash))
		if err != nil || approval.decided != 0 || resp == nil || resp.Toast.Type != "error" {
			t.Fatalf("stale hash: response=%+v err=%v calls=%d", resp, err, approval.decided)
		}
	}
}

func TestCardActionServiceConflictNeverReportsApproval(t *testing.T) {
	for _, name := range []string{"changed_after_get", "legacy", "expired", "already_decided"} {
		t.Run(name, func(t *testing.T) {
			approval := &fakeApproval{row: completeCardApproval(t), decideErr: store.ErrApprovalConflict}
			if name == "changed_after_get" {
				approval.decideErr = nil
				approval.mutateAfterGet = true
			}
			if name == "legacy" {
				approval.row.ExecutionContext = nil
			}
			hash := approval.row.PlanHash
			business := NewCallbackBusiness(BusinessDependencies{Store: &fakeCallbackStore{}, Approval: approval, OperatorAllowlist: []string{"ou_ops"}})
			resp, err := business.CardAction(context.Background(), cardEvent("conflict-"+name, "ou_ops", "approve", 7, hash))
			if err != nil || approval.decided != 1 || approval.expectedHash != hash || resp == nil || resp.Toast.Type != "error" {
				t.Fatalf("conflict: response=%+v err=%v approval=%+v", resp, err, approval)
			}
		})
	}
}

type cardPatchClient struct{ patched chan string }

func (c cardPatchClient) Reply(context.Context, string, string) (notify.Delivery, error) {
	return notify.Delivery{}, nil
}
func (c cardPatchClient) Patch(_ context.Context, _ string, content string) error {
	c.patched <- content
	return nil
}

func TestDecisionPatchRetainsApprovedSnapshotWithoutButtons(t *testing.T) {
	approval := &fakeApproval{row: completeCardApproval(t)}
	client := cardPatchClient{patched: make(chan string, 1)}
	business := NewCallbackBusiness(BusinessDependencies{Store: &fakeCallbackStore{}, Approval: approval, Client: client, OperatorAllowlist: []string{"ou_ops"}})
	if _, err := business.CardAction(context.Background(), cardEvent("patch", "ou_ops", "approve", 7, approval.row.PlanHash)); err != nil {
		t.Fatal(err)
	}
	select {
	case content := <-client.patched:
		for _, expected := range []string{"container/sub2api", "single_container", "L2", "**Dry run:** false"} {
			if !strings.Contains(content, expected) {
				t.Fatalf("patch missing %s: %s", expected, content)
			}
		}
		for _, forbidden := range []string{`"type":"callback"`, "private-target.invalid", "target_kind", "execution_context"} {
			if strings.Contains(content, forbidden) {
				t.Fatalf("patch leaked %s: %s", forbidden, content)
			}
		}
	case <-time.After(time.Second):
		t.Fatal("decision card was not patched")
	}
}

func TestCardActionDuplicateReceiptIsIdempotent(t *testing.T) {
	approval := &fakeApproval{row: store.Approval{ID: 7, Status: "pending", PlanHash: "abc"}}
	receipts := &fakeCallbackStore{}
	business := NewCallbackBusiness(BusinessDependencies{
		Store:             receipts,
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
		Store:             &fakeCallbackStore{},
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
		Store:             &fakeCallbackStore{},
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
