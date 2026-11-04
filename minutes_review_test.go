package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func failedReview(t *testing.T) (*testEnv, *browser, string, *Minutes) {
	t.Helper()
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "sala-review")
	e.app.DB.conn.Exec("UPDATE rooms SET owner_email='host@example.test' WHERE slug='sala-review'")
	e.llm.out = []byte(strings.Replace(validMinutesJSON, "Ana: Pablo envía la propuesta el viernes\", \"confidence\"", "Ana: PRIVATE_UNSUPPORTED_QUOTE\", \"confidence\"", 1))
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "review-key", "s1", "sala-review", testLink))
	if !e.app.processOne(context.Background()) {
		t.Fatal("no job")
	}
	ms, err := e.app.DB.ListMinutes("sala-review")
	if err != nil || len(ms) != 1 {
		t.Fatal("missing minutes", err)
	}
	return e, b, csrf, &ms[0]
}

func TestEvidenceReviewRetainsDraftAndBlocksSideEffects(t *testing.T) {
	e, b, _, m := failedReview(t)
	if m.Status != "failed" || !strings.Contains(m.ResultJSON, "PRIVATE_UNSUPPORTED_QUOTE") || m.Model != "test-model" {
		t.Fatalf("not retained: %+v", m)
	}
	for _, table := range []string{"host_notifications", "minutes_deliveries", "odoo_sync_outbox"} {
		var count int
		if err := e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("%s: %d %v", table, count, err)
		}
	}
	if strings.Contains(e.logs.String(), "PRIVATE_UNSUPPORTED_QUOTE") {
		t.Fatal("quote leaked to logs")
	}
	if ok, err := e.app.DB.RequeueMinutes(m.ID); err != nil || ok {
		t.Fatal("saved candidate regenerated", err)
	}
	if e.app.processOne(context.Background()) || e.llm.calls != 1 {
		t.Fatal("automatic regeneration")
	}
	page := b.do("GET", "/admin/minutes/"+itoa(m.ID), nil)
	for _, text := range []string{"borrador requiere revisión", "Tarea 1", "PRIVATE_UNSUPPORTED_QUOTE", "Conservar borrador", "Consultar la transcripción"} {
		if page.Code != 200 || !strings.Contains(page.Body.String(), text) {
			t.Fatalf("missing %q in %d: %s", text, page.Code, page.Body.String())
		}
	}
	if strings.Contains(page.Body.String(), "Enviar transcripción y minuta") || strings.Contains(page.Body.String(), ">Reintentar<") {
		t.Fatal("unsafe action shown")
	}
	if rr := b.do("GET", "/admin/minutes/"+itoa(m.ID)+"/download", nil); rr.Code != 409 {
		t.Fatal("candidate downloadable", rr.Code)
	}
	if _, err := e.app.DB.QueueDeliveries(m.ID, []Recipient{{Email: "guest@example.test"}}, "admin"); err == nil {
		t.Fatal("candidate queued for mail")
	}
}

func TestResolveEvidenceReviewWithoutAIThenNotifyOnce(t *testing.T) {
	e, b, csrf, m := failedReview(t)
	page := b.do("GET", "/admin/minutes/"+itoa(m.ID), nil)
	form := url.Values{"csrf_token": {csrf}, "draft_digest": {hidden(t, page.Body.String(), "draft_digest")}}
	path := "/admin/minutes/" + itoa(m.ID) + "/resolve"
	if rr := b.do("POST", path, form); rr.Code != 303 {
		t.Fatalf("resolve %d: %s", rr.Code, rr.Body.String())
	}
	ready, _ := e.app.DB.GetMinutes(m.ID)
	var result MinutesResult
	if err := json.Unmarshal([]byte(ready.ResultJSON), &result); err != nil {
		t.Fatal(err)
	}
	if ready.Status != "ready" || len(result.ActionItems) != 1 || len(result.ReviewWarnings) != 1 || e.llm.calls != 1 {
		t.Fatalf("unexpected result: %+v", ready)
	}
	if strings.Contains(ready.ResultJSON, "PRIVATE_UNSUPPORTED_QUOTE") {
		t.Fatal("unsupported item published")
	}
	if err := validateEvidence(&result, testTranscriptJSON); err != nil {
		t.Fatal(err)
	}
	if rr := b.do("POST", path, form); rr.Code != 409 {
		t.Fatal("duplicate resolve accepted")
	}
	var notices, deliveries, events int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM host_notifications").Scan(&notices)
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM minutes_deliveries").Scan(&deliveries)
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM odoo_sync_outbox").Scan(&events)
	if notices != 1 || deliveries != 0 || events != 1 {
		t.Fatalf("effects %d/%d/%d", notices, deliveries, events)
	}
	md := b.do("GET", "/admin/minutes/"+itoa(m.ID)+"/download", nil)
	if !strings.Contains(md.Body.String(), "lista de tareas puede estar incompleta") {
		t.Fatal("review warning missing from export")
	}
}

func TestReviewRejectsStaleCSRFAndCancelled(t *testing.T) {
	e, b, csrf, m := failedReview(t)
	path := "/admin/minutes/" + itoa(m.ID) + "/resolve"
	page := b.do("GET", "/admin/minutes/"+itoa(m.ID), nil)
	digest := hidden(t, page.Body.String(), "draft_digest")
	if rr := b.do("POST", path, url.Values{"draft_digest": {digest}}); rr.Code != http.StatusForbidden {
		t.Fatal("CSRF not enforced")
	}
	if rr := b.do("POST", path, url.Values{"csrf_token": {csrf}, "draft_digest": {"stale"}}); rr.Code != 409 {
		t.Fatal("stale review accepted")
	}
	e.app.DB.CancelMinutes(m.ID)
	if rr := b.do("POST", path, url.Values{"csrf_token": {csrf}, "draft_digest": {digest}}); rr.Code != 409 {
		t.Fatal("cancelled review accepted")
	}
}

func TestReviewIsPrivateToRoomOwner(t *testing.T) {
	a := newAccessEnv(t)
	owner, csrf := a.user("owner@transgenia.org")
	a.createRoom(owner, csrf, "private-review")
	a.llm.out = []byte(strings.Replace(validMinutesJSON, "Ana: alguien revise inventario", "Ana: PRIVATE_UNSUPPORTED_QUOTE", 1))
	a.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "private-review", "s1", "private-review", testLink))
	a.app.processOne(context.Background())
	ms, _ := a.app.DB.ListMinutes("private-review")
	path := "/admin/minutes/" + itoa(ms[0].ID)
	other, otherCSRF := a.user("other@transgenia.org")
	if rr := other.do("GET", path, nil); rr.Code != 404 || strings.Contains(rr.Body.String(), "PRIVATE_UNSUPPORTED_QUOTE") {
		t.Fatal("candidate disclosed to another owner")
	}
	if rr := other.do("POST", path+"/resolve", url.Values{"csrf_token": {otherCSRF}, "draft_digest": {"anything"}}); rr.Code != 404 {
		t.Fatal("another owner resolved candidate")
	}
}

func TestManualRetriesPreserveUsageHistory(t *testing.T) {
	e := newTestEnv(t)
	e.app.DB.CreateRoom("retry-history", "x", "")
	e.llm.out = []byte(`{"invalid":true}`)
	e.signedWebhook(eventJSON("TRANSCRIPTION_UPLOADED", "attempt-key", "s1", "retry-history", testLink))
	e.app.processOne(context.Background())
	ms, _ := e.app.DB.ListMinutes("retry-history")
	if ok, err := e.app.DB.RequeueMinutes(ms[0].ID); !ok || err != nil {
		t.Fatal("retry", err)
	}
	e.app.processOne(context.Background())
	m, _ := e.app.DB.GetMinutes(ms[0].ID)
	var count int
	e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM ai_usage WHERE minutes_id=?", m.ID).Scan(&count)
	if m.Attempts != 2 || count != 2 || e.llm.calls != 2 || e.rt.requests != 1 {
		t.Fatalf("history %d %d", m.Attempts, count)
	}
}

func TestLiteralEvidenceSupportsChannelColonButNotAlteredMeaning(t *testing.T) {
	raw := `{"messages":[{"name":"Sala: Norte","content":"No acepto el contrato. Revisaré el borrador.","timestamp":1760464800000}]}`
	for _, tc := range []struct {
		evidence string
		valid    bool
	}{
		{"Sala: Norte: No acepto el contrato.", true},
		{"“Sala: Norte: Revisaré el borrador.”", true},
		{"Norte: Revisaré el borrador.", false},
		{"Sala: Norte: Acepto el contrato.", false},
		{"Sala: Norte: No acepto... Revisaré el borrador.", false},
	} {
		r := &MinutesResult{ActionItems: []MinutesActionItem{{Evidence: tc.evidence}}}
		if (validateEvidence(r, raw) == nil) != tc.valid {
			t.Errorf("literal rule: %q", tc.evidence)
		}
	}
}
