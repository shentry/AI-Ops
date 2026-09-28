-- Access paths used by the execution queue and per-Incident admission transaction.
ALTER TABLE approval
  ADD KEY idx_approval_ready (status, expires_at, id),
  ADD KEY idx_approval_incident_status (incident_id, status);
ALTER TABLE agent_run ADD KEY idx_run_incident_status (incident_id, status, id);
ALTER TABLE incident_event ADD KEY idx_event_incident_type (incident_id, event_type, id);
