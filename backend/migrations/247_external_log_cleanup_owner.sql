-- Explicit default survives process restarts: a missing/unreadable setting must
-- never hand retention back to native workers after external management began.
INSERT INTO settings (key, value)
VALUES ('external_log_cleanup_enabled', 'false')
ON CONFLICT (key) DO NOTHING;
