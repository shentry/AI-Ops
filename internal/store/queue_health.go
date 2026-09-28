package store

import (
	"context"
	"time"
)

// QueueAges measures waiting work, independently of queue length. The queries use
// existing status/time indexes and never wait longer than the scrape budget.
func (db *DB) QueueAges(ctx context.Context) (map[string]int64, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	var rows []struct {
		Name string
		Age  int64
	}
	err := db.WithContext(ctx).Raw(`
SELECT 'raw_event' AS name, COALESCE(TIMESTAMPDIFF(SECOND, MIN(created_at), UTC_TIMESTAMP(3)),0) AS age FROM raw_event WHERE status='pending'
UNION ALL
SELECT 'diagnosis', COALESCE(TIMESTAMPDIFF(SECOND, MIN(started_at), UTC_TIMESTAMP(3)),0) FROM agent_run WHERE status='pending'
UNION ALL
SELECT 'verification', COALESCE(TIMESTAMPDIFF(SECOND, MIN(next_check_at), UTC_TIMESTAMP(3)),0) FROM verify_task WHERE status='pending'
UNION ALL
SELECT 'notification', COALESCE(TIMESTAMPDIFF(SECOND, MIN(next_attempt_at), UTC_TIMESTAMP(3)),0) FROM notification_task WHERE delivered_at IS NULL`).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	ages := make(map[string]int64, len(rows))
	for _, row := range rows {
		ages[row.Name] = max(row.Age, 0)
	}
	return ages, nil
}
