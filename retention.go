package main

import (
	"context"
	"log/slog"
	"time"
)

// Retention policy (defaults; see TRANSCRIPT_RETENTION_DAYS and
// MINUTES_RETENTION_DAYS, 0 disables the time-based purge):
//   - deleting a room deletes its recordings, transcripts, minutes and
//     participant sessions immediately (DB.DeleteRoom);
//   - recording links are dropped when they expire (24 h, JaaS default);
//   - pre-authenticated transcript links are cleared once used or expired;
//   - transcripts are deleted after TRANSCRIPT_RETENTION_DAYS (90);
//   - minutes are deleted after MINUTES_RETENTION_DAYS (365);
//   - webhook idempotency keys are kept 30 days;
//   - expired admin and participant sessions are purged.
func (db *DB) PurgeExpired(transcriptDays, minutesDays int) (map[string]int64, error) {
	now := db.now()
	out := map[string]int64{}
	run := func(name, q string, args ...any) error {
		res, err := db.conn.Exec(q, args...)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		out[name] = n
		return nil
	}
	steps := []struct {
		name string
		q    string
		args []any
	}{
		{"recordings", "DELETE FROM recordings WHERE expires_at <= ?", []any{now.Unix()}},
		{"source_links", "UPDATE minutes SET source_url = NULL WHERE source_url IS NOT NULL AND source_expires_at <= ?", []any{now.Unix()}},
		{"webhook_events", "DELETE FROM webhook_events WHERE received_at < ?", []any{now.AddDate(0, 0, -30).Unix()}},
		{"admin_sessions", "DELETE FROM admin_sessions WHERE expires_at <= ? OR last_seen_at < ?", []any{now.Unix(), now.Add(-adminIdleTimeout).Unix()}},
		{"participant_sessions", "DELETE FROM participant_sessions WHERE expires_at <= ?", []any{now.Unix()}},
	}
	if transcriptDays > 0 {
		steps = append(steps, struct {
			name string
			q    string
			args []any
		}{"transcriptions", "DELETE FROM transcriptions WHERE created_at < ?", []any{now.AddDate(0, 0, -transcriptDays).Unix()}})
	}
	if minutesDays > 0 {
		steps = append(steps, struct {
			name string
			q    string
			args []any
		}{"minutes", "DELETE FROM minutes WHERE created_at < ? AND status IN ('ready','failed')", []any{now.AddDate(0, 0, -minutesDays).Unix()}})
	}
	for _, s := range steps {
		if err := run(s.name, s.q, s.args...); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (app *App) runRetention(ctx context.Context) {
	purge := func() {
		app.DB.DeactivateExpiredRooms(4 * time.Hour)
		res, err := app.DB.PurgeExpired(app.Config.TranscriptRetentionDays, app.Config.MinutesRetentionDays)
		if err != nil {
			slog.Error("retention: purge failed", "error", err.Error())
			return
		}
		for k, n := range res {
			if n > 0 {
				slog.Info("retention: purged", "what", k, "rows", n)
			}
		}
	}
	purge()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			purge()
		}
	}
}
