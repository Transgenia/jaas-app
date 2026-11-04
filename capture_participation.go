package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

var errCaptureNoAudio = errors.New("no se recibió audio para transcribir")

func captureParticipantKey(p *ParticipantSession, connection string) string {
	return hashToken(p.CSRFToken + "\x00" + connection)
}

type captureReport struct {
	ConnectionID string `json:"connection_id"`
	RunID        int64  `json:"run_id"`
	Choice       string `json:"choice"`
	Event        string `json:"event"`
}

func (db *DB) recordCaptureParticipation(p *ParticipantSession, report captureReport) (int64, error) {
	if p == nil || p.CSRFToken == "" || report.RunID < 0 || !streamIDPattern.MatchString(report.ConnectionID) {
		return 0, errChunkForbidden
	}
	if report.Choice != "enabled" && report.Choice != "disabled" && report.Choice != "unsupported" {
		return 0, errChunkForbidden
	}
	switch report.Event {
	case "joined", "heartbeat", "ended", "permission_denied", "device_error", "upload_failed":
	default:
		return 0, errChunkForbidden
	}
	if report.RunID == 0 && report.Event != "joined" {
		return 0, errChunkForbidden
	}
	if report.Choice != "enabled" && (report.Event == "permission_denied" || report.Event == "device_error" || report.Event == "upload_failed") {
		return 0, errChunkForbidden
	}
	db.captureMu.Lock()
	defer db.captureMu.Unlock()
	tx, err := db.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	id := report.RunID
	if id == 0 {
		id, err = openRun(tx, p.RoomSlug, db.now())
		if err != nil {
			return 0, err
		}
	}
	var open int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM capture_runs WHERE id=? AND room_slug=?
	 AND status='open' AND processing_approved_at IS NULL AND cancelled_at IS NULL`, id, p.RoomSlug).Scan(&open); err != nil {
		return 0, err
	}
	if open != 1 {
		return 0, errChunkClosed
	}
	key := captureParticipantKey(p, report.ConnectionID)
	if report.RunID == 0 {
		_, err = tx.Exec(`INSERT INTO capture_participation(run_id,participant_key,display_name,choice,updated_at)
		 VALUES(?,?,?,?,?) ON CONFLICT(run_id,participant_key) DO NOTHING`, id, key, p.DisplayName, report.Choice, db.unix())
		if err != nil {
			return 0, err
		}
	}
	var choice string
	var ended sql.NullInt64
	if err = tx.QueryRow("SELECT choice,ended_at FROM capture_participation WHERE run_id=? AND participant_key=?", id, key).Scan(&choice, &ended); err != nil || choice != report.Choice {
		return 0, errChunkForbidden
	}
	if ended.Valid {
		if report.Event == "ended" {
			return id, nil
		}
		return 0, errChunkClosed
	}
	issue := ""
	switch report.Event {
	case "permission_denied", "device_error", "upload_failed":
		issue = report.Event
	}
	_, err = tx.Exec(`UPDATE capture_participation SET updated_at=?,
	 issue=CASE WHEN ?<>'' THEN ? ELSE issue END,
	 ended_at=CASE WHEN ?='ended' THEN ? ELSE ended_at END
	 WHERE run_id=? AND participant_key=?`, db.unix(), issue, issue, report.Event, db.unix(), id, key)
	if err != nil {
		return 0, err
	}
	_, err = tx.Exec("UPDATE capture_runs SET last_activity_at=? WHERE id=?", db.unix(), id)
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

func (app *App) handleCaptureParticipation(w http.ResponseWriter, r *http.Request) {
	p, err := app.DB.GetParticipantSession(app.cookieValue(r, "tgm_part"))
	if err != nil || p.RoomSlug != r.PathValue("slug") || !tokensEqual(r.Header.Get("X-CSRF-Token"), p.CSRFToken) {
		http.Error(w, "Forbidden", 403)
		return
	}
	room, err := app.DB.GetRoom(p.RoomSlug)
	if err != nil || !room.Transcription || app.Transcriber == nil {
		http.Error(w, "Captura no disponible", 409)
		return
	}
	var report captureReport
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 2048))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&report) != nil {
		http.Error(w, "Bad Request", 400)
		return
	}
	id, err := app.DB.recordCaptureParticipation(p, report)
	if err != nil {
		http.Error(w, "No se pudo registrar el estado de captura.", 409)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int64{"run_id": id})
}

type captureParticipationView struct {
	Name    string
	Choice  string
	Issue   string
	Streams int
}
type captureCoverage struct {
	Reports        []captureParticipationView
	UnknownStreams int
	TotalStreams   int
}

func (db *DB) captureCoverage(id int64) (*captureCoverage, error) {
	coverage := &captureCoverage{}
	rows, err := db.conn.Query(`SELECT p.display_name,p.choice,p.issue,
	 (SELECT COUNT(*) FROM audio_streams a WHERE a.run_id=p.run_id AND a.participant_key=p.participant_key)
	 FROM capture_participation p WHERE run_id=? ORDER BY p.updated_at,p.participant_key`, id)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var row captureParticipationView
		if err = rows.Scan(&row.Name, &row.Choice, &row.Issue, &row.Streams); err != nil {
			rows.Close()
			return nil, err
		}
		coverage.Reports = append(coverage.Reports, row)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	err = db.conn.QueryRow(`SELECT COUNT(*),COALESCE(SUM(NOT EXISTS(
	 SELECT 1 FROM capture_participation p WHERE p.run_id=a.run_id AND p.participant_key=a.participant_key)),0)
	 FROM audio_streams a WHERE run_id=?`, id).Scan(&coverage.TotalStreams, &coverage.UnknownStreams)
	return coverage, err
}

func (app *App) minutesCaptureCoverage(m *Minutes) *captureCoverage {
	tail, ok := strings.CutPrefix(m.SessionID, "captura-")
	if !ok {
		return nil
	}
	id, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return nil
	}
	run, err := app.DB.GetCaptureRun(id)
	if err != nil || run.RoomSlug != m.RoomSlug {
		return nil
	}
	coverage, _ := app.DB.captureCoverage(id)
	return coverage
}

// Called while captureMu is held, in the same transaction as the upload.
func checkCaptureParticipation(tx *sql.Tx, p *ParticipantSession, id int64, connection string, requireReport bool) error {
	var choice string
	err := tx.QueryRow("SELECT choice FROM capture_participation WHERE run_id=? AND participant_key=?", id, captureParticipantKey(p, connection)).Scan(&choice)
	if errors.Is(err, sql.ErrNoRows) && !requireReport {
		return nil
	} // old clients: unknown
	if err != nil || choice != "enabled" {
		return errChunkForbidden
	}
	return nil
}

const participationActiveWindow = 2 * time.Minute
