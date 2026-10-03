-- Nullable: old zero durations remain unknown; new measured zero survives log pruning.
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS latency_known BOOLEAN;
