package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	incidentrule "oncall-agent/internal/incident"
)

// agent_run_step 逐步审计，以及 CompleteRun ——
// run 终态 + 最后一批 Step/Event/Problem 的单事务收尾。

// RunStepRecord 是一步审计、事实事件和问题变更的一次性批次。
// AppendRunStepRecord 会在同一短事务中写入全部内容。
type RunStepRecord struct {
	Step     AgentRunStep
	Events   []IncidentEvent
	Problems []ProblemMutation
}

// RunCompletion 原子写入一次 run 的终态和最后一批审计记录。
// Steps、Events、Problems 会与 agent_run 终态在同一短事务提交。
type RunCompletion struct {
	RunID      uint64
	RCA        string
	PlanJSON   []byte
	TokensIn   int
	TokensOut  int
	Status     string
	FinishedAt time.Time
	Steps      []AgentRunStep
	Events     []IncidentEvent
	Problems   []ProblemMutation
	Approval   *Approval // Published in the same transaction; ID is filled on success.
}

// AppendRunStepRecord 在同一短事务中写入 step、事件和有限问题变更。
// 任一写入失败都会回滚整批；流水线只使用这一条审计写入路径。
func (db *DB) AppendRunStepRecord(ctx context.Context, record RunStepRecord) error {
	step := record.Step
	if step.RunID == 0 || step.Name == "" || step.Kind == "" {
		return errors.New("store: run step run_id, kind and name are required")
	}
	if step.StartedAt.IsZero() {
		return errors.New("store: run step start time is required")
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.WithContext(ctx).Create(&step).Error; err != nil {
			return fmt.Errorf("store: append run step: %w", err)
		}
		for _, event := range record.Events {
			if _, err := appendIncidentEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		for _, mutation := range record.Problems {
			if err := applyProblemMutation(ctx, tx, mutation); err != nil {
				return err
			}
		}
		return nil
	})
}

// CompleteRun 原子更新 run 终态，并写入最后一批 Step/Event/Problem。
// 事务先锁 Incident 再锁 Run；READ COMMITTED 防止锁前身份查询固定旧成员快照。
func (db *DB) CompleteRun(ctx context.Context, completion RunCompletion) error {
	if completion.RunID == 0 {
		return errors.New("store: run completion run_id is required")
	}
	if completion.Status != "succeeded" && completion.Status != "failed" {
		return errors.New("store: run completion status must be succeeded or failed")
	}
	if completion.FinishedAt.IsZero() {
		return errors.New("store: run completion finished_at is required")
	}
	if len(completion.PlanJSON) > 0 && !json.Valid(completion.PlanJSON) {
		return errors.New("store: run completion plan must be valid JSON")
	}
	completion.FinishedAt = completion.FinishedAt.UTC()
	var published Approval
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var run AgentRun
		if err := tx.WithContext(ctx).First(&run, completion.RunID).Error; err != nil {
			return err
		}
		parent, err := lockIncident(ctx, tx, run.IncidentID)
		if err != nil {
			return err
		}
		query := tx.WithContext(ctx).Clauses(clause.Locking{Strength: "UPDATE"}).First(&run, completion.RunID)
		if errors.Is(query.Error, gorm.ErrRecordNotFound) {
			return ErrAgentRunNotFound
		}
		if query.Error != nil {
			return fmt.Errorf("lock agent run for completion: %w", query.Error)
		}
		if run.Status != "pending" && run.Status != "running" {
			return fmt.Errorf("store: agent run %d is not pending/running", completion.RunID)
		}

		updates := map[string]any{
			"status":      completion.Status,
			"tokens_in":   completion.TokensIn,
			"tokens_out":  completion.TokensOut,
			"finished_at": completion.FinishedAt,
		}
		if completion.RCA != "" {
			updates["rca_text"] = completion.RCA
		}
		if len(completion.PlanJSON) > 0 {
			updates["plan_json"] = datatypes.JSON(append([]byte(nil), completion.PlanJSON...))
		}
		result := tx.WithContext(ctx).Model(&AgentRun{}).
			Where("id = ? AND status IN ?", completion.RunID, []string{"pending", "running"}).
			Updates(updates)
		if result.Error != nil {
			return fmt.Errorf("complete agent run: %w", result.Error)
		}
		if result.RowsAffected == 0 {
			return fmt.Errorf("store: agent run %d is not pending/running", completion.RunID)
		}

		for _, step := range completion.Steps {
			if step.RunID == 0 {
				step.RunID = run.ID
			}
			if step.RunID != run.ID || step.Name == "" || step.Kind == "" {
				return errors.New("store: run completion step has invalid run_id, kind or name")
			}
			if step.StartedAt.IsZero() {
				return errors.New("store: run completion step start time is required")
			}
			if err := tx.WithContext(ctx).Create(&step).Error; err != nil {
				return fmt.Errorf("append completion run step: %w", err)
			}
		}
		for _, event := range completion.Events {
			if event.IncidentID == 0 {
				event.IncidentID = run.IncidentID
			}
			if event.IncidentID != run.IncidentID {
				return errors.New("store: run completion event incident does not match run")
			}
			if event.RunID == nil {
				runID := run.ID
				event.RunID = &runID
			} else if *event.RunID != run.ID {
				return errors.New("store: run completion event run does not match run")
			}
			if _, err := appendIncidentEvent(ctx, tx, event); err != nil {
				return err
			}
		}
		for _, mutation := range completion.Problems {
			if mutation.Kind == ProblemOpen {
				if mutation.Problem.IncidentID == 0 {
					mutation.Problem.IncidentID = run.IncidentID
				}
				if mutation.Problem.IncidentID != run.IncidentID {
					return errors.New("store: run completion problem incident does not match run")
				}
			} else if mutation.Kind == ProblemResolve {
				if mutation.IncidentID == 0 {
					mutation.IncidentID = run.IncidentID
				}
				if mutation.IncidentID != run.IncidentID {
					return errors.New("store: run completion problem incident does not match run")
				}
			}
			if err := applyProblemMutation(ctx, tx, mutation); err != nil {
				return err
			}
		}
		if completion.Approval != nil {
			draft := *completion.Approval
			if completion.Status != "succeeded" || parent.Status != incidentrule.StatusFiring || draft.IncidentID != run.IncidentID || draft.RunID != run.ID {
				return errors.New("store: approval cannot be published for this run")
			}
			snapshot, err := incidentrule.ParseExecutionContext(draft.ExecutionContext)
			if err != nil {
				return err
			}
			members, err := listIncidentExecutionMembers(ctx, tx, parent.ID)
			if err != nil {
				return err
			}
			if err := snapshot.ValidateMembers(members, true); err != nil {
				return err
			}
			published, err = insertApproval(ctx, tx, draft)
			if err != nil {
				return err
			}
		}
		return nil
	}, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return fmt.Errorf("store: complete run: %w", err)
	}
	if completion.Approval != nil {
		*completion.Approval = published
	}
	return nil
}

// ListRunSteps 按 seq 升序取 run 的全部审计步（重诊注入上一轮 verify 结论用）。
func (db *DB) ListRunSteps(ctx context.Context, runID uint64) ([]AgentRunStep, error) {
	steps := make([]AgentRunStep, 0)
	err := db.WithContext(ctx).Where("run_id = ?", runID).Order("seq ASC, id ASC").Find(&steps).Error
	if err != nil {
		return nil, fmt.Errorf("store: list run steps: %w", err)
	}
	return steps, nil
}

// ListIncidentRunSteps 读一次 run 的审计步，并校验该 run 确实属于这个
// incident —— 否则带着别人的 run_id 就能越权读到任意 Incident 的步骤。
func (db *DB) ListIncidentRunSteps(ctx context.Context, incidentID, runID, afterID uint64, limit int) ([]AgentRunStep, error) {
	if incidentID == 0 || runID == 0 {
		return nil, ErrAgentRunNotFound
	}
	run, err := db.GetAgentRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if run.IncidentID != incidentID {
		return nil, ErrAgentRunNotFound
	}
	steps := make([]AgentRunStep, 0)
	err = db.WithContext(ctx).Where("run_id = ? AND id > ?", runID, afterID).Order("id ASC").Limit(normalizePageLimit(limit)).Find(&steps).Error
	if err != nil {
		return nil, fmt.Errorf("store: list incident run steps: %w", err)
	}
	return steps, nil
}
