package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedTranscriber answers per call, so a test can model provider outcomes.
type scriptedTranscriber struct {
	mu    sync.Mutex
	calls int
	reply func(audio []byte, call int) ([]transcriptSegment, error)
}

func (s *scriptedTranscriber) Transcribe(_ context.Context, audio []byte, _ string) ([]transcriptSegment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	return s.reply(audio, s.calls)
}

func spokenFixture() []transcriptSegment {
	return []transcriptSegment{{Text: "Se revisó el alcance del proyecto y se acordó enviar la propuesta de capacitación el viernes.",
		TimePrecision: "window", RequestID: "req_spoken", Model: "gpt-transcribe", Usage: json.RawMessage(`{"seconds":600}`)}}
}

func silentAnswer() error {
	return &audioTranscriptionError{Kind: audioKindEmptyTranscript, RequestID: "req_silent", Usage: json.RawMessage(`{"type":"duration","seconds":34}`)}
}

func streamState(t *testing.T, e *testEnv, id string) (status, transcript, msg string) {
	t.Helper()
	if err := e.app.DB.conn.QueryRow("SELECT status,COALESCE(transcript,''),COALESCE(error,'') FROM audio_streams WHERE id=?", id).Scan(&status, &transcript, &msg); err != nil {
		t.Fatal(err)
	}
	return
}

func usageRow(t *testing.T, e *testEnv, stream string, attempt int) (status, requestID, usage, kind string) {
	t.Helper()
	key := "transcription:" + stream + ":" + itoa(int64(attempt))
	if err := e.app.DB.conn.QueryRow("SELECT status,COALESCE(request_id,''),COALESCE(usage_json,''),COALESCE(error_kind,'') FROM ai_usage WHERE operation_key=?", key).Scan(&status, &requestID, &usage, &kind); err != nil {
		t.Fatalf("usage %s: %v", key, err)
	}
	return
}

func countRows(t *testing.T, e *testEnv, table string) (n int) {
	t.Helper()
	if err := e.app.DB.conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return
}

// A window the provider answers with no text is a definite result. It used to
// be filed as an uncertain request and held back the whole meeting.
func TestSilentTrackIsADefiniteResultAndDoesNotBlockTheMeeting(t *testing.T) {
	e, _ := captureEnv(t)
	tr := &scriptedTranscriber{reply: func(audio []byte, _ int) ([]transcriptSegment, error) {
		if bytes.HasPrefix(audio, []byte("SILENCE")) {
			return nil, silentAnswer()
		}
		return spokenFixture(), nil
	}}
	e.app.Transcriber = tr
	seedWorkflow(t, e, streamA, []byte("SILENCE"), CaptureHints{})
	seedWorkflow(t, e, streamB, []byte("VOICE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	drain(e)

	if status, transcript, msg := streamState(t, e, streamA); status != "transcribed" || transcript != "[]" || msg != "" {
		t.Fatalf("silent track: %s %q %q", status, transcript, msg)
	}
	if status, requestID, usage, kind := usageRow(t, e, streamA, 1); status != "succeeded" || requestID != "req_silent" || !strings.Contains(usage, `"seconds":34`) || kind != "" {
		t.Fatalf("silent usage: %s %s %s %s", status, requestID, usage, kind)
	}
	run, _ := e.app.DB.GetCaptureRun(1)
	if run.Status != "done" || run.Error != "" {
		t.Fatalf("run %+v", run)
	}
	if n := countRows(t, e, "minutes"); n != 1 {
		t.Fatalf("minutes rows %d", n)
	}
	coverage, err := e.app.DB.captureCoverage(1)
	if err != nil || coverage.SilentStreams != 1 || coverage.ExcludedStreams != 0 {
		t.Fatalf("coverage %+v %v", coverage, err)
	}
	if tr.calls != 2 {
		t.Fatalf("provider calls %d", tr.calls)
	}
}

func TestSilentResultSurvivesDatabaseFailureWithoutSecondRequest(t *testing.T) {
	e, _ := captureEnv(t)
	tr := &scriptedTranscriber{reply: func([]byte, int) ([]transcriptSegment, error) { return nil, silentAnswer() }}
	e.app.Transcriber = tr
	seedWorkflow(t, e, streamA, []byte("SILENCE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	if _, err := e.app.DB.conn.Exec(`CREATE TRIGGER fail_transcribed BEFORE UPDATE ON audio_streams WHEN NEW.status='transcribed' BEGIN SELECT RAISE(FAIL,'fixture persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	e.app.transcribeStream(context.Background(), streamA, 1)
	e.app.DB.conn.Exec("DROP TRIGGER fail_transcribed")
	e.advance(16 * time.Minute)
	e.app.transcribeStream(context.Background(), streamA, 2)
	if tr.calls != 1 {
		t.Fatalf("recovery repeated the provider request: %d", tr.calls)
	}
	if status, transcript, _ := streamState(t, e, streamA); status != "transcribed" || transcript != "[]" {
		t.Fatalf("recovered state %s %q", status, transcript)
	}
	if n := countRows(t, e, "ai_usage"); n != 1 {
		t.Fatalf("usage rows %d", n)
	}
	// The durable result keeps the identity a silent window has no segment for.
	if status, requestID, usage, _ := usageRow(t, e, streamA, 1); status != "succeeded" || requestID != "req_silent" || !strings.Contains(usage, `"seconds":34`) {
		t.Fatalf("recovered usage %s %s %s", status, requestID, usage)
	}
}

// An uncertain request still fails closed, but now says why, and the host can
// decide to send it again instead of losing the meeting.
func TestUncertainTrackRecordsItsCauseAndTheHostCanRetry(t *testing.T) {
	e, _ := captureEnv(t)
	tr := &scriptedTranscriber{reply: func(_ []byte, call int) ([]transcriptSegment, error) {
		if call == 1 {
			return nil, &audioTranscriptionError{Kind: "connection failed"}
		}
		return spokenFixture(), nil
	}}
	e.app.Transcriber = tr
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	drain(e)

	status, _, msg := streamState(t, e, streamA)
	if status != "failed" || !strings.Contains(msg, "sin conexión con el proveedor") || !strings.Contains(msg, "revisión manual") {
		t.Fatalf("failed state %s %q", status, msg)
	}
	if usage, _, _, kind := usageRow(t, e, streamA, 1); usage != "uncertain" || kind != "connection failed" {
		t.Fatalf("usage %s %q", usage, kind)
	}
	if logs := e.logs.String(); !strings.Contains(logs, "capture: stream transcription failed") || !strings.Contains(logs, `cause="connection failed"`) {
		t.Fatalf("failure cause not logged: %s", logs)
	}
	run, _ := e.app.DB.GetCaptureRun(1)
	if run.Status != "failed" || !strings.Contains(run.Error, "parcial") || run.FailedStreams != 1 {
		t.Fatalf("run %+v", run)
	}
	// Nothing is sent again without a host decision.
	e.advance(time.Hour)
	drain(e)
	if tr.calls != 1 {
		t.Fatalf("automatic retry of an uncertain request: %d", tr.calls)
	}
	if _, err := e.app.DB.ExcludeFailedStreams(1); !errors.Is(err, ErrCaptureNoTranscript) {
		t.Fatalf("excluded the only track: %v", err)
	}

	b, csrf := e.loginAdminNoChange()
	if list := b.do("GET", "/admin/captures", nil).Body.String(); !strings.Contains(list, "captura parcial") {
		t.Fatal("session list does not say why the capture needs review")
	}
	page := b.do("GET", "/admin/captures/1/audio", nil).Body.String()
	if !strings.Contains(page, "Reintentar pistas fallidas") || !strings.Contains(page, "sin conexión con el proveedor") {
		t.Fatal("retry action or failure cause missing")
	}
	if strings.Contains(page, "Continuar sin las pistas fallidas") {
		t.Fatal("offered to continue with no transcribed track")
	}
	if rr := b.do("POST", "/admin/captures/1/retry", url.Values{}); rr.Code != 403 {
		t.Fatalf("retry without CSRF: %d", rr.Code)
	}
	if rr := b.do("POST", "/admin/captures/1/retry", url.Values{"csrf_token": {csrf}}); rr.Code != 303 {
		t.Fatalf("retry: %d %s", rr.Code, rr.Body.String())
	}
	if _, err := os.Stat(e.app.requestPath(streamA)); !os.IsNotExist(err) {
		t.Fatal("retry kept the marker of the abandoned request")
	}
	drain(e)
	if tr.calls != 2 {
		t.Fatalf("provider calls after retry: %d", tr.calls)
	}
	if status, _, msg = streamState(t, e, streamA); status != "transcribed" || msg != "" {
		t.Fatalf("after retry %s %q", status, msg)
	}
	if run, _ = e.app.DB.GetCaptureRun(1); run.Status != "done" || run.Error != "" {
		t.Fatalf("run after retry %+v", run)
	}
	// Both attempts stay on record: the uncertain one is not overwritten.
	if usage, _, _, kind := usageRow(t, e, streamA, 1); usage != "uncertain" || kind != "connection failed" {
		t.Fatalf("first attempt rewritten: %s %q", usage, kind)
	}
	if usage, requestID, _, kind := usageRow(t, e, streamA, 2); usage != "succeeded" || requestID != "req_spoken" || kind != "" {
		t.Fatalf("second attempt: %s %s %q", usage, requestID, kind)
	}
	if n := countRows(t, e, "minutes"); n != 1 {
		t.Fatalf("minutes rows %d", n)
	}
	if rr := b.do("POST", "/admin/captures/1/retry", url.Values{"csrf_token": {csrf}}); rr.Code != 409 {
		t.Fatalf("retry of a finished capture: %d", rr.Code)
	}
}

// When a track cannot be transcribed at all, the host may continue without it.
// The transcript and the minutes declare the omission.
func TestHostCanContinueWithoutAFailedTrackAndTheMinutesSaySo(t *testing.T) {
	e, _ := captureEnv(t)
	tr := &scriptedTranscriber{reply: func(audio []byte, _ int) ([]transcriptSegment, error) {
		if bytes.HasPrefix(audio, []byte("BROKEN")) {
			return nil, &audioTranscriptionError{StatusCode: 400, RequestID: "req_rejected", cause: &httpStatusError{Op: "openai audio", Code: 400}}
		}
		return spokenFixture(), nil
	}}
	e.app.Transcriber = tr
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	seedWorkflow(t, e, streamB, []byte("BROKEN"), CaptureHints{})
	approveWorkflow(t, e, 1)
	drain(e)

	if status, _, msg := streamState(t, e, streamB); status != "failed" || msg != "openai audio: HTTP 400" {
		t.Fatalf("rejected track %s %q", status, msg)
	}
	if usage, requestID, _, kind := usageRow(t, e, streamB, 1); usage != "failed" || requestID != "req_rejected" || kind != "http 400" {
		t.Fatalf("rejected usage %s %s %q", usage, requestID, kind)
	}
	if _, err := os.Stat(e.app.requestPath(streamB)); !os.IsNotExist(err) {
		t.Fatal("a definite provider refusal left an uncertainty marker")
	}
	if run, _ := e.app.DB.GetCaptureRun(1); run.Status != "failed" {
		t.Fatalf("run %+v", run)
	}
	if n := countRows(t, e, "minutes"); n != 0 {
		t.Fatal("partial capture produced minutes before the host decided")
	}

	b, csrf := e.loginAdminNoChange()
	page := b.do("GET", "/admin/captures/1/audio", nil).Body.String()
	if !strings.Contains(page, "Reintentar pistas fallidas") || !strings.Contains(page, "Continuar sin las pistas fallidas") {
		t.Fatal("recovery actions missing")
	}
	if rr := b.do("POST", "/admin/captures/1/exclude-failed", url.Values{}); rr.Code != 403 {
		t.Fatalf("exclude without CSRF: %d", rr.Code)
	}
	if rr := b.do("POST", "/admin/captures/1/exclude-failed", url.Values{"csrf_token": {csrf}}); rr.Code != 303 {
		t.Fatalf("exclude: %d %s", rr.Code, rr.Body.String())
	}
	drain(e)

	run, _ := e.app.DB.GetCaptureRun(1)
	if run.Status != "done" || run.FailedStreams != 0 {
		t.Fatalf("run after exclusion %+v", run)
	}
	if status, _, msg := streamState(t, e, streamB); status != "failed" || msg == "" {
		t.Fatalf("excluded track lost its record: %s %q", status, msg)
	}
	if _, err := os.Stat(e.app.streamPath(streamB)); err != nil {
		t.Fatal("excluded audio was removed")
	}
	var data string
	if err := e.app.DB.conn.QueryRow("SELECT data FROM transcriptions WHERE session_id='captura-1'").Scan(&data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data, `"excluded_streams":1`) || !strings.Contains(data, `"capture_complete":false`) {
		t.Fatalf("transcript hides the omission: %s", data)
	}
	if note := excludedAudioNote(data); !strings.Contains(note, "sin 1 pista(s) de audio") {
		t.Fatalf("note %q", note)
	}
	coverage, _ := e.app.DB.captureCoverage(1)
	if coverage.ExcludedStreams != 1 {
		t.Fatalf("coverage %+v", coverage)
	}
	page = b.do("GET", "/admin/captures/1/audio", nil).Body.String()
	if !strings.Contains(page, "excluida por el anfitrión") || strings.Contains(page, "Reintentar pistas fallidas") {
		t.Fatal("audio page after exclusion")
	}
	// The stored draft carries the warning and can still be opened for review.
	var id int64
	var result string
	if err := e.app.DB.conn.QueryRow("SELECT id,COALESCE(result_json,'') FROM minutes").Scan(&id, &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Esta minuta se generó sin 1 pista(s) de audio") {
		t.Fatalf("minutes result lacks the omission warning: %s", result)
	}
	view := b.do("GET", "/admin/minutes/"+itoa(id), nil)
	if view.Code != 200 || !strings.Contains(view.Body.String(), "Esta minuta se generó sin 1 pista(s) de audio") {
		t.Fatalf("minutes page %d", view.Code)
	}
	if rr := b.do("POST", "/admin/captures/1/exclude-failed", url.Values{"csrf_token": {csrf}}); rr.Code != 409 {
		t.Fatalf("second exclusion: %d", rr.Code)
	}
}

func TestCaptureRecoveryRequiresAFailedApprovedCapture(t *testing.T) {
	e, _ := captureEnv(t)
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	if _, err := e.app.RetryFailedStreams(1); !errors.Is(err, ErrCaptureNotApproved) {
		t.Fatalf("retry before approval: %v", err)
	}
	if _, err := e.app.DB.ExcludeFailedStreams(1); !errors.Is(err, ErrCaptureNotApproved) {
		t.Fatalf("exclude before approval: %v", err)
	}
	if _, err := e.app.RetryFailedStreams(99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown capture: %v", err)
	}
	approveWorkflow(t, e, 1)
	if _, err := e.app.RetryFailedStreams(1); !errors.Is(err, ErrCaptureNotRecoverable) {
		t.Fatalf("retry while transcribing: %v", err)
	}
	e.app.DB.conn.Exec("UPDATE audio_streams SET status='failed',error='fixture' WHERE id=?", streamA)
	e.app.DB.conn.Exec("UPDATE capture_runs SET status='failed' WHERE id=1")
	if err := e.app.DB.CancelCapture(1); err != nil {
		t.Fatal(err)
	}
	if _, err := e.app.RetryFailedStreams(1); !errors.Is(err, ErrCaptureCancelled) {
		t.Fatalf("retry of a cancelled capture: %v", err)
	}
}

func TestCaptureRecoveryActionsEnforceOwner(t *testing.T) {
	e := newTestEnv(t)
	db := e.app.DB
	db.CreateRoom("owned-room", "x", "owner@example.test")
	res, err := db.conn.Exec("INSERT INTO capture_runs(room_slug,started_at,last_activity_at,status)VALUES('owned-room',?,?,'failed')", e.now.Unix(), e.now.Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for name, handler := range map[string]func(*httptest.ResponseRecorder) int{
		"retry": func(w *httptest.ResponseRecorder) int {
			r := httptest.NewRequest("POST", "/admin/captures/"+itoa(id)+"/retry", nil)
			r.SetPathValue("id", itoa(id))
			e.app.handleRetryCapture(w, r.WithContext(contextWithAdmin(r, &AdminSession{Email: "stranger@example.test"})))
			return w.Code
		},
		"exclude": func(w *httptest.ResponseRecorder) int {
			r := httptest.NewRequest("POST", "/admin/captures/"+itoa(id)+"/exclude-failed", nil)
			r.SetPathValue("id", itoa(id))
			e.app.handleExcludeFailedStreams(w, r.WithContext(contextWithAdmin(r, &AdminSession{Email: "stranger@example.test"})))
			return w.Code
		},
	} {
		if code := handler(httptest.NewRecorder()); code != 404 {
			t.Fatalf("%s across owners: %d", name, code)
		}
	}
}

// A capture that follows the Jitsi mute uploads nothing while muted. The host
// must be able to see that time instead of a heartbeat that says "enabled".
func TestMutedCaptureTimeIsRecordedAsEvidence(t *testing.T) {
	e, _ := captureEnv(t)
	room := joinCaptureRoom(t, e, "sala-captura", "Sala", "sala@example.test", false)
	report := captureReport{ConnectionID: streamA, Choice: "enabled", Event: "joined"}
	send := func(event string) int {
		report.Event = event
		return postCaptureReport(e, room, "sala-captura", report, room.csrf).Code
	}
	if send("joined") != 200 {
		t.Fatal("join report")
	}
	report.RunID = 1
	e.advance(2 * time.Second)
	if send("paused") != 200 || send("paused") != 200 {
		t.Fatal("pause report")
	}
	e.advance(5 * time.Minute)
	send("heartbeat")
	// An open pause is measured up to the last report, never to the present.
	e.advance(30 * time.Minute)
	coverage, _ := e.app.DB.captureCoverage(1)
	if got := coverage.Reports[0]; got.Paused != "5 min 00 s" || got.PauseCount != 1 {
		t.Fatalf("open pause %+v", got)
	}
	e.advance(-30 * time.Minute)
	e.advance(4 * time.Minute)
	send("resumed")
	e.advance(time.Minute)
	send("paused")
	e.advance(2 * time.Minute)
	if send("ended") != 200 {
		t.Fatal("end report")
	}
	coverage, _ = e.app.DB.captureCoverage(1)
	if got := coverage.Reports[0]; got.Paused != "11 min 00 s" || got.PauseCount != 2 || got.Connected != "12 min 02 s" {
		t.Fatalf("closed pauses %+v", got)
	}
	b, _ := e.loginAdminNoChange()
	page := b.do("GET", "/admin/captures/1/audio", nil).Body.String()
	if !strings.Contains(page, "silenciado en Jitsi durante 11 min 00 s de 12 min 02 s de conexión (2 pausa(s))") {
		t.Fatal("pause evidence missing from coverage")
	}

	// Only a connection that chose to capture can report a pause.
	other := joinCaptureRoom(t, e, "sala-captura", "Otra", "otra@example.test", false)
	declined := captureReport{ConnectionID: streamB, Choice: "disabled", Event: "joined", RunID: 0}
	if w := postCaptureReport(e, other, "sala-captura", declined, other.csrf); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	declined.RunID, declined.Event = 1, "paused"
	if w := postCaptureReport(e, other, "sala-captura", declined, other.csrf); w.Code != 409 {
		t.Fatalf("declined connection reported a pause: %d", w.Code)
	}
}

// A failure of the local transcript check cannot be fixed by retrying, and a
// retry there never reaches the provider. The page must not say otherwise.
func TestLocalGateFailureDoesNotOfferACostlyRetry(t *testing.T) {
	e, ft := captureEnv(t)
	ft.byMark["VOICE"] = []transcriptSegment{{Text: "Hola, buenas tardes.", TimePrecision: "window"}}
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	approveWorkflow(t, e, 1)
	drain(e)
	var id int64
	var status, msg string
	if err := e.app.DB.conn.QueryRow("SELECT id,status,COALESCE(error,'') FROM minutes").Scan(&id, &status, &msg); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || !strings.Contains(msg, "contenido insuficiente") {
		t.Fatalf("minutes %s %q", status, msg)
	}
	if e.llm.calls != 0 {
		t.Fatalf("local gate reached the model %d times", e.llm.calls)
	}
	b, _ := e.loginAdminNoChange()
	page := b.do("GET", "/admin/minutes/"+itoa(id), nil).Body.String()
	if !strings.Contains(page, "verificación local previa a la IA") || !strings.Contains(page, "/admin/captures/1/audio") {
		t.Fatal("gate explanation missing")
	}
	if strings.Contains(page, "/admin/minutes/"+itoa(id)+"/retry") || strings.Contains(page, "consumo adicional") {
		t.Fatal("page still offers a retry that cannot succeed")
	}
}

// The minutes of a capture are dated at its start. People who join after the
// first minutes still took part and must be offered as recipients; someone
// who enters the room long after the capture ended must not.
func TestCaptureRecipientsIncludeLateJoiners(t *testing.T) {
	e, ft := captureEnv(t)
	ft.byMark["VOICE"] = spokenFixture()
	early := joinCaptureRoom(t, e, "sala-captura", "Ana", "ana@cliente.example", false)
	if status := early.upload(e, "sala-captura", streamA, 0, []byte("VOICE"), early.csrf); status != 204 {
		t.Fatalf("first chunk: %d", status)
	}
	e.advance(40 * time.Minute)
	joinCaptureRoom(t, e, "sala-captura", "Tarde", "tarde@cliente.example", false)
	e.advance(50 * time.Minute)
	if status := early.upload(e, "sala-captura", streamA, 1, []byte("end"), early.csrf); status != 204 {
		t.Fatalf("last chunk: %d", status)
	}
	approveWorkflow(t, e, 1)
	drain(e)
	e.advance(2 * time.Hour)
	joinCaptureRoom(t, e, "sala-captura", "Después", "despues@cliente.example", false)

	ms, _ := e.app.DB.ListMinutes("sala-captura")
	if len(ms) != 1 {
		t.Fatalf("minutes: %+v", ms)
	}
	rs, err := e.app.DB.ProposedRecipients(&ms[0])
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, r := range rs {
		got[r.Email] = true
	}
	if !got["ana@cliente.example"] || !got["tarde@cliente.example"] {
		t.Fatalf("participant who joined 40 minutes in is not proposed: %v", got)
	}
	if got["despues@cliente.example"] {
		t.Fatalf("someone who entered after the capture ended is proposed: %v", got)
	}
}

const streamC = "CCCCCCCCCCCCCCCCCCCCCCCC"

// longCapture uploads two chunks 90 seconds apart, so the track has more than
// a minute of audio, and finalizes the run it lands in.
func longCapture(t *testing.T, e *testEnv, stream string) int64 {
	t.Helper()
	seedWorkflow(t, e, stream, []byte("VOICE"), CaptureHints{})
	e.advance(90 * time.Second)
	if err := e.app.AppendChunk(workflowParticipant(), stream, 1, e.now, []byte("more")); err != nil {
		t.Fatal(err)
	}
	var run int64
	if err := e.app.DB.conn.QueryRow("SELECT run_id FROM audio_streams WHERE id=?", stream).Scan(&run); err != nil {
		t.Fatal(err)
	}
	finalizeAllCaptureStreams(t, e, run)
	return run
}

// Automatic processing is the operator's standing approval. Without it a
// finished capture waits; with it the capture goes through the same approval.
func TestFinishedCaptureIsProcessedAutomaticallyOnlyWhenEnabled(t *testing.T) {
	e, ft := captureEnv(t)
	ft.byMark["VOICE"] = spokenFixture()
	run := longCapture(t, e, streamA)
	e.advance(30 * time.Minute)
	drain(e)
	if r, _ := e.app.DB.GetCaptureRun(run); r.Status != "open" || !r.ApprovedAt.IsZero() || ft.calls != 0 {
		t.Fatalf("processed without approval or configuration: %+v calls=%d", r, ft.calls)
	}
	e.app.Config.CaptureAutoProcess = true
	e.app.Config.CaptureAutoProcessAfter = 10 * time.Minute
	drain(e)
	r, _ := e.app.DB.GetCaptureRun(run)
	if r.Status != "done" || r.ApprovedAt.IsZero() || ft.calls != 1 {
		t.Fatalf("finished capture not processed automatically: %+v calls=%d", r, ft.calls)
	}
	if n := countRows(t, e, "minutes"); n != 1 {
		t.Fatalf("minutes rows %d", n)
	}
	if !strings.Contains(e.logs.String(), "capture: processing approved automatically") {
		t.Fatal("automatic approval left no trace")
	}
}

func TestAutomaticProcessingLeavesDoubtfulCapturesToTheHost(t *testing.T) {
	e, ft := captureEnv(t)
	e.app.Config.CaptureAutoProcess = true
	e.app.Config.CaptureAutoProcessAfter = 10 * time.Minute
	open := func(run int64) bool {
		r, err := e.app.DB.GetCaptureRun(run)
		if err != nil {
			t.Fatal(err)
		}
		return r.Status == "open" && r.ApprovedAt.IsZero()
	}

	// Seconds of audio: not worth a paid transcription nobody asked for.
	seedWorkflow(t, e, streamA, []byte("VOICE"), CaptureHints{})
	finalizeAllCaptureStreams(t, e, 1)
	e.advance(40 * time.Minute)
	drain(e)
	if !open(1) {
		t.Fatal("a capture with no meaningful audio was processed")
	}

	// The meeting only just ended: people may still come back.
	recent := longCapture(t, e, streamB)
	e.advance(5 * time.Minute)
	drain(e)
	if !open(recent) {
		t.Fatal("processed before the grace period")
	}
	// A cancelled capture stays cancelled.
	if err := e.app.DB.CancelCapture(recent); err != nil {
		t.Fatal(err)
	}
	e.advance(40 * time.Minute)
	drain(e)
	if r, _ := e.app.DB.GetCaptureRun(recent); !r.ApprovedAt.IsZero() {
		t.Fatal("automatic processing overrode a cancellation")
	}

	// A capture left behind for more than a day is a decision for a person.
	old := longCapture(t, e, streamC)
	e.advance(25 * time.Hour)
	drain(e)
	if !open(old) {
		t.Fatal("an old pending capture was processed automatically")
	}
	if ft.calls != 0 {
		t.Fatalf("provider calls %d", ft.calls)
	}
}

// Participants and hosts are told which mode applies.
func TestNoticesMatchTheProcessingMode(t *testing.T) {
	e := newTestEnv(t)
	b, csrf := e.loginAdmin()
	e.createRoom(b, csrf, "notice-room")
	join := e.browser().do("GET", "/m/notice-room", nil).Body.String()
	if !strings.Contains(join, "Sólo tras la aprobación del anfitrión") || strings.Contains(join, "automáticamente a OpenAI") {
		t.Fatal("manual mode notice")
	}
	if list := b.do("GET", "/admin/captures", nil).Body.String(); strings.Contains(list, "Procesamiento automático activado") {
		t.Fatal("host told of automatic processing while it is off")
	}
	e.app.Config.CaptureAutoProcess = true
	e.app.Config.CaptureAutoProcessAfter = 10 * time.Minute
	join = e.browser().do("GET", "/m/notice-room", nil).Body.String()
	if strings.Contains(join, "Sólo tras la aprobación del anfitrión") || !strings.Contains(join, "Al terminar la reunión se envía automáticamente a OpenAI") {
		t.Fatal("automatic mode notice")
	}
	if list := b.do("GET", "/admin/captures", nil).Body.String(); !strings.Contains(list, "se procesa con IA 10 minutos después") {
		t.Fatal("host not told of automatic processing")
	}
}
