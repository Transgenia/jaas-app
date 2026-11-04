package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
)

func isEvidenceReview(err error) bool {
	var review *evidenceReviewError
	return errors.As(err, &review)
}

// A failed candidate is private review material. Failed status keeps the
// existing download, delivery and Odoo gates closed; no notification is queued.
func (db *DB) SaveMinutesReview(id int64, raw, model, reason string) error {
	res, err := db.conn.Exec(`UPDATE minutes SET status='failed',result_json=?,model=?,error=?,
	 locked_until=NULL,updated_at=? WHERE id=? AND status='processing' AND cancelled_at IS NULL`,
		raw, model, reason, db.unix(), id)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		return errAnalysisCancelled
	}
	return err
}

type minutesReview struct {
	Result *MinutesResult
	Issues []evidenceIssue
	Digest string
}

func reviewDigest(draft, transcript string) string {
	sum := sha256.Sum256([]byte(draft + "\x00" + transcript))
	return hex.EncodeToString(sum[:])
}

func (app *App) loadMinutesReview(m *Minutes, t *Transcription) (*minutesReview, error) {
	if m.Status != "failed" || m.ResultJSON == "" || !m.CancelledAt.IsZero() || t == nil {
		return nil, ErrNotFound
	}
	// Validate the provider fields again; the persisted metrics are recomputed.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(m.ResultJSON), &fields); err != nil {
		return nil, err
	}
	delete(fields, "metrics")
	if string(fields["commercial_signals"]) == "null" {
		delete(fields, "commercial_signals")
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	r, err := validateMinutes(raw)
	if err != nil {
		return nil, err
	}
	if err = transcriptQuality(t.Data); err != nil {
		return nil, err
	}
	metrics := computeMetrics(parseUtterances(t.Data))
	if unmeasuredTranscript(t.Data) {
		metrics = nil
	}
	prepareMinutesResult(r, t.Data, metrics)
	issues, err := evidenceIssues(r, t.Data)
	if err != nil {
		return nil, err
	}
	return &minutesReview{r, issues, reviewDigest(m.ResultJSON, t.Data)}, nil
}

func omitUnverified(r *MinutesResult, issues []evidenceIssue) {
	bad := map[string]map[int]bool{}
	for _, issue := range issues {
		if bad[issue.Section] == nil {
			bad[issue.Section] = map[int]bool{}
		}
		bad[issue.Section][issue.Index] = true
	}
	r.ActionItems = keepVerified(r.ActionItems, bad["action_items"])
	r.Sentiment.BySpeaker = keepVerified(r.Sentiment.BySpeaker, bad["by_speaker"])
	r.Sentiment.Moments = keepVerified(r.Sentiment.Moments, bad["moments"])
	if r.CommercialSignals != nil {
		r.CommercialSignals = keepVerified(r.CommercialSignals, bad["commercial_signals"])
	}
	if len(issues) > 0 {
		r.ReviewWarnings = []string{fmt.Sprintf("Durante la revisión se excluyeron %d elementos cuya evidencia no pudo verificarse. La lista de tareas puede estar incompleta; contrástala con la transcripción antes de compartirla. El resumen sigue siendo un borrador generado por IA.", len(issues))}
	}
}

func keepVerified[T any](items []T, bad map[int]bool) []T {
	kept := make([]T, 0, len(items))
	for i, item := range items {
		if !bad[i] {
			kept = append(kept, item)
		}
	}
	return kept
}

// Review discards only the explicitly listed unsupported elements. It never
// accepts an unsupported assertion, invokes AI, or approves participant email.
func (app *App) handleResolveMinutes(w http.ResponseWriter, r *http.Request) {
	m, _, ok := app.loadMinutes(w, r)
	if !ok {
		return
	}
	app.effectsMu.Lock()
	defer app.effectsMu.Unlock()
	t, err := app.DB.GetTranscriptionBySession(m.RoomSlug, m.SessionID)
	if err != nil {
		http.Error(w, "La transcripción no está disponible para verificar el borrador.", 409)
		return
	}
	review, err := app.loadMinutesReview(m, t)
	if err != nil || !tokensEqual(r.PostFormValue("draft_digest"), reviewDigest(m.ResultJSON, t.Data)) {
		http.Error(w, "El borrador cambió o no se puede revisar. Recarga la página.", 409)
		return
	}
	omitUnverified(review.Result, review.Issues)
	if err = validateEvidence(review.Result, t.Data); err != nil {
		http.Error(w, "El borrador todavía contiene evidencia no verificable.", 409)
		return
	}
	raw, err := json.Marshal(review.Result)
	if err != nil {
		app.serverError(w, "review encode", err)
		return
	}
	if err = app.DB.completeMinutes(m.ID, string(raw), m.Model, app.minutesOwners(m.RoomSlug), m.ResultJSON); err != nil {
		http.Error(w, "El borrador cambió o fue cancelado. Recarga la página.", 409)
		return
	}
	app.wakeWorker()
	http.Redirect(w, r, fmt.Sprintf("/admin/minutes/%d", m.ID), http.StatusSeeOther)
}
