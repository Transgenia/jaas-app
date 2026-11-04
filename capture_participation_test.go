package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postCaptureReport(e *testEnv, p participant, slug string, report captureReport, csrf string) *httptest.ResponseRecorder {
	raw, _ := json.Marshal(report)
	req := httptest.NewRequest("POST", testOrigin+"/m/"+slug+"/capture-status", strings.NewReader(string(raw)))
	req.Header.Set("X-CSRF-Token", csrf)
	req.Header.Set("Origin", testOrigin)
	for _, c := range p.b.cookies {
		req.AddCookie(&http.Cookie{Name: c.Name, Value: c.Value})
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, req)
	return w
}

func TestCaptureChoiceIsExplicitScopedAndNotAIApproval(t *testing.T) {
	e, ft := captureEnv(t)
	p := joinCaptureRoom(t, e, "sala-captura", "Invitada", "guest@example.test", false)
	report := captureReport{ConnectionID: streamA, Choice: "disabled", Event: "joined"}
	if w := postCaptureReport(e, p, "sala-captura", report, "bad"); w.Code != 403 {
		t.Fatal("CSRF", w.Code)
	}
	if w := postCaptureReport(e, p, "other-room", report, p.csrf); w.Code != 403 {
		t.Fatal("room isolation", w.Code)
	}
	w := postCaptureReport(e, p, "sala-captura", report, p.csrf)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	report.RunID = 1
	coverage, err := e.app.DB.captureCoverage(1)
	if err != nil || len(coverage.Reports) != 1 || coverage.Reports[0].Choice != "disabled" || coverage.TotalStreams != 0 {
		t.Fatalf("coverage %+v %v", coverage, err)
	}
	if err = e.app.DB.ApproveCapture(1); err != errCaptureNoAudio {
		t.Fatal("empty run approved", err)
	}
	if ft.calls != 0 || e.llm.calls != 0 {
		t.Fatal("choice invoked AI")
	}
	report.Choice = "enabled"
	if w = postCaptureReport(e, p, "sala-captura", report, p.csrf); w.Code != 409 {
		t.Fatal("choice rewritten")
	}
	// A new connection can make a different explicit choice without rewriting history.
	report.ConnectionID = streamB
	report.RunID = 0
	if w = postCaptureReport(e, p, "sala-captura", report, p.csrf); w.Code != 200 {
		t.Fatal("new connection", w.Body.String())
	}
	coverage, _ = e.app.DB.captureCoverage(1)
	if len(coverage.Reports) != 2 {
		t.Fatal("connection history lost")
	}
}

func TestCapturePermissionReportAndUploadBinding(t *testing.T) {
	e, _ := captureEnv(t)
	guest := joinCaptureRoom(t, e, "sala-captura", "Invitada", "guest@example.test", false)
	report := captureReport{ConnectionID: streamA, Choice: "enabled", Event: "joined"}
	if w := postCaptureReport(e, guest, "sala-captura", report, guest.csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p, err := e.app.DB.GetParticipantSession(guest.b.cookies[e.app.cookieName("tgm_part")].Value)
	if err != nil {
		t.Fatal(err)
	}
	hints := CaptureHints{RunID: 1, ConnectionID: streamB, Language: "es"}
	if err = e.app.AppendChunkWithMetadata(p, streamA, 0, e.now, []byte("audio"), hints); err != errChunkForbidden {
		t.Fatal("unknown connection uploaded", err)
	}
	hints.ConnectionID = streamA
	if err = e.app.AppendChunkWithMetadata(p, streamA, 0, e.now, []byte("audio"), hints); err != nil {
		t.Fatal(err)
	}
	report.RunID = 1
	report.Event = "permission_denied"
	if w := postCaptureReport(e, guest, "sala-captura", report, guest.csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	coverage, _ := e.app.DB.captureCoverage(1)
	if coverage.Reports[0].Issue != "permission_denied" || coverage.Reports[0].Streams != 1 || coverage.UnknownStreams != 0 {
		t.Fatalf("%+v", coverage)
	}
	b, _ := e.loginAdminNoChange()
	page := b.do("GET", "/admin/captures/1/audio", nil)
	if !strings.Contains(page.Body.String(), "no concedió acceso al micrófono") {
		t.Fatal("permission reason missing")
	}
	if strings.Contains(e.logs.String(), "guest@example.test") {
		t.Fatal("identity leaked in logs")
	}
}

func TestLegacyAudioCoverageIsUnknownNotRefusal(t *testing.T) {
	e, _ := captureEnv(t)
	guest := joinCaptureRoom(t, e, "sala-captura", "Invitada", "guest@example.test", false)
	if status := guest.upload(e, "sala-captura", streamA, 0, []byte("audio"), guest.csrf); status != 204 {
		t.Fatal(status)
	}
	coverage, _ := e.app.DB.captureCoverage(1)
	if len(coverage.Reports) != 0 || coverage.UnknownStreams != 1 {
		t.Fatal("legacy choice inferred")
	}
	b, _ := e.loginAdminNoChange()
	page := b.do("GET", "/admin/captures/1/audio", nil)
	if !strings.Contains(page.Body.String(), "No se puede afirmar que alguien rechazó") {
		t.Fatal("unknown reason missing")
	}
}

func TestReportedConnectionMustEndBeforeProcessing(t *testing.T) {
	e, _ := captureEnv(t)
	guest := joinCaptureRoom(t, e, "sala-captura", "Invitada", "guest@example.test", false)
	report := captureReport{ConnectionID: streamA, Choice: "enabled", Event: "joined"}
	if w := postCaptureReport(e, guest, "sala-captura", report, guest.csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p, _ := e.app.DB.GetParticipantSession(guest.b.cookies[e.app.cookieName("tgm_part")].Value)
	if err := e.app.AppendChunkWithMetadata(p, streamA, 0, e.now, []byte("audio"), CaptureHints{RunID: 1, ConnectionID: streamA, Language: "es"}); err != nil {
		t.Fatal(err)
	}
	if err := e.app.FinalizeStream(p, streamA, 1, 5, e.now); err != nil {
		t.Fatal(err)
	}
	if err := e.app.DB.ApproveCapture(1); err != ErrCaptureActive {
		t.Fatal("active connection approved", err)
	}
	report.RunID = 1
	report.Event = "ended"
	if w := postCaptureReport(e, guest, "sala-captura", report, guest.csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if err := e.app.DB.ApproveCapture(1); err != nil {
		t.Fatal("ended connection blocked", err)
	}
}

func TestDisabledConnectionCannotUpload(t *testing.T) {
	e, _ := captureEnv(t)
	guest := joinCaptureRoom(t, e, "sala-captura", "Invitada", "guest@example.test", false)
	report := captureReport{ConnectionID: streamA, Choice: "disabled", Event: "joined"}
	if w := postCaptureReport(e, guest, "sala-captura", report, guest.csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	p, _ := e.app.DB.GetParticipantSession(guest.b.cookies[e.app.cookieName("tgm_part")].Value)
	err := e.app.AppendChunkWithMetadata(p, streamA, 0, e.now, []byte("audio"), CaptureHints{RunID: 1, ConnectionID: streamA, Language: "es"})
	if err != errChunkForbidden {
		t.Fatal("disabled connection uploaded", err)
	}
	coverage, _ := e.app.DB.captureCoverage(1)
	if coverage.TotalStreams != 0 {
		t.Fatal("denied upload persisted")
	}
}
