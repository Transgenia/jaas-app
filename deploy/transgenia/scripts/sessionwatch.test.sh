#!/usr/bin/env bash
# Exercises sessionwatch.sh against a throwaway database and a fake mailer.
#   bash deploy/transgenia/scripts/sessionwatch.test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d)
trap 'rm -rf -- "${T:?}"' EXIT
mkdir -p "$T/base/data" "$T/state"
DB=$T/base/data/jaas.db
MAILS=$T/mails

cat >"$T/notify" <<STUB
#!/bin/sh
[ ! -f "$T/notify_fail" ] || exit 1
{ echo "TO=\$1"; echo "SUBJECT=\$2"; cat; echo "--END--"; } >>"$MAILS"
STUB
chmod +x "$T/notify"
: >"$MAILS"

sqlite3 "$DB" "
CREATE TABLE rooms(slug TEXT, owner_email TEXT);
CREATE TABLE capture_runs(id INTEGER PRIMARY KEY, room_slug TEXT, started_at INTEGER, last_activity_at INTEGER, ended_at INTEGER,
 status TEXT, processing_approved_at INTEGER, cancelled_at INTEGER, error TEXT);
CREATE TABLE audio_streams(run_id INTEGER, status TEXT, started_at INTEGER, ended_at_ms INTEGER, last_chunk_at INTEGER, transcript TEXT, excluded_at INTEGER);
CREATE TABLE capture_participation(run_id INTEGER, choice TEXT, paused_seconds INTEGER DEFAULT 0, paused_since INTEGER, ended_at INTEGER, updated_at INTEGER);
CREATE TABLE minutes(id INTEGER PRIMARY KEY, room_slug TEXT, session_id TEXT, status TEXT, error TEXT, result_json TEXT, cancelled_at INTEGER);
CREATE TABLE transcriptions(room_slug TEXT, session_id TEXT, data TEXT);
CREATE TABLE minutes_deliveries(minutes_id INTEGER, status TEXT);
CREATE TABLE odoo_sync_outbox(minutes_id INTEGER, status TEXT, error_code TEXT);
INSERT INTO rooms VALUES('sala-a','owner@example.test'),('sala-b','');"

sql() { sqlite3 "$DB" "$1"; }
NOW="strftime('%s','now')"
# capture ID ROOM AGE_OF_START LENGTH: an open, unapproved run that ended LENGTH seconds after it started
capture() {
	sql "INSERT INTO capture_runs(id,room_slug,started_at,last_activity_at,status) VALUES($1,'$2',$NOW-$3,$NOW-$3+$4,'open')"
}
stream() { # RUN SECONDS_OF_AUDIO STATUS TRANSCRIPT
	sql "INSERT INTO audio_streams(run_id,status,started_at,ended_at_ms,last_chunk_at,transcript)
	 SELECT $1,'$3',started_at*1000,started_at*1000+$2*1000,started_at+$2,$4 FROM capture_runs WHERE id=$1"
}
joined() { sql "INSERT INTO capture_participation(run_id,choice,paused_seconds,ended_at,updated_at) VALUES($1,'$2',${3:-0},$NOW-1800,$NOW-1800)"; }

watch() {
	TGN_BASE=$T/base STATE_DIRECTORY=$T/state SESSIONWATCH_NOTIFY=$T/notify SESSIONWATCH_TO=ops@example.test \
		SESSIONWATCH_URL=https://meet.example.test bash "$HERE/sessionwatch.sh" "$@"
}
mails() { grep -c '^SUBJECT=' "$MAILS" || true; }
last() { awk -v RS='--END--\n' 'NF {m=$0} END {print m}' "$MAILS"; }
fails=0
check() {
	if eval "$2"; then
		echo "ok   $1"
	else
		echo "FAIL $1"
		fails=$((fails + 1))
	fi
}

# 1: history present before the first run is recorded, never mailed
capture 1 sala-a 90000 3600
stream 1 3500 closed NULL
joined 1 enabled
watch >"$T/out"
check "first run records history without mailing it" '[ "$(mails)" = 0 ] && [ -f "$T/state/state" ] && grep -q "línea base" "$T/out"'

# 2: a meeting that ended with almost no audio is flagged before anyone processes it
capture 2 sala-a 3600 540
stream 2 2 closed NULL
joined 2 enabled 530
watch >"$T/out"
check "ended capture is announced once, to the room owner" '[ "$(mails)" = 1 ] && last | grep -q "^TO=owner@example.test" && last | grep -q "captura lista para procesar, con observaciones"'
check "the mail says how little audio there is and why" 'last | grep -q "Poco audio: 0 min en una reunión de 9 min" && last | grep -q "pausa por micrófono silenciado: 9 min"'
check "the mail carries no address" '! last | grep -v "^TO=" | grep -q "@"'
watch >"$T/out"
check "nothing is repeated on the next run" '[ "$(mails)" = 1 ]'

# 3: a meeting still in progress is left alone
capture 3 sala-b 600 590
stream 3 500 recording NULL
watch >"$T/out"
check "a live meeting is not announced" '[ "$(mails)" = 1 ]'

# 4: a failed capture reaches the fallback address when the room has no owner
sql "UPDATE capture_runs SET status='failed',processing_approved_at=$NOW,error='captura parcial: revisar pistas fallidas antes de generar minuta',last_activity_at=$NOW-3600 WHERE id=3"
sql "UPDATE audio_streams SET status='failed' WHERE run_id=3"
watch >"$T/out"
check "failed capture is announced with its reason" '[ "$(mails)" = 2 ] && last | grep -q "^TO=ops@example.test" && last | grep -q "la captura requiere revisión" && last | grep -q "captura parcial" && last | grep -q "/admin/captures/3/audio"'

# 5: minutes ready, then delivered
sql "UPDATE capture_runs SET status='done',processing_approved_at=$NOW WHERE id=1"
sql "UPDATE audio_streams SET status='transcribed',transcript='[{\"text\":\"x\"}]' WHERE run_id=1"
sql "INSERT INTO minutes(id,room_slug,session_id,status,result_json) VALUES(7,'sala-a','captura-1','ready','{}')"
sql "INSERT INTO transcriptions VALUES('sala-a','captura-1','{\"messages\":[{\"content\":\"$(printf 'palabra %.0s' $(seq 1 6000))\"}]}')"
watch >"$T/out"
check "ready minutes are announced with the transcript check" '[ "$(mails)" = 3 ] && last | grep -q "SUBJECT=\[Meet\] sala-a: minuta lista$" && last | grep -q "palabras, cerca de" && last | grep -q "/admin/minutes/7"'
sql "INSERT INTO minutes_deliveries VALUES(7,'queued'),(7,'sent')"
watch >"$T/out"
check "a delivery in progress is not announced" '[ "$(mails)" = 3 ]'
sql "UPDATE minutes_deliveries SET status='sent'"
sql "INSERT INTO odoo_sync_outbox VALUES(7,'failed','owner_mapping_missing')"
watch >"$T/out"
check "completed delivery closes the loop and reports the Odoo failure" '[ "$(mails)" = 4 ] && last | grep -q "minuta enviada a 2 participante(s), con observaciones" && last | grep -q "Odoo falló (owner_mapping_missing)"'

# 6: a mail that cannot be sent is retried, and the run fails so the unit alerts
sql "UPDATE minutes SET status='failed',error='esquema inválido' WHERE id=7"
sql "UPDATE minutes SET result_json=NULL WHERE id=7"
touch "$T/notify_fail"
if watch >"$T/out" 2>&1; then status=0; else status=$?; fi
check "a failed mail makes the run fail" '[ "$status" != 0 ] && [ "$(mails)" = 4 ]'
rm "$T/notify_fail"
watch >"$T/out"
check "and it is sent on the next run" '[ "$(mails)" = 5 ] && last | grep -q "la minuta requiere revisión"'

# 7: dry run and resend
before=$(cat "$T/state/state")
watch --dry-run --resend 2 >"$T/out"
check "dry run prints the notice and changes nothing" 'grep -q "simulación" "$T/out" && grep -q "captura lista para procesar" "$T/out" && [ "$(mails)" = 5 ] && [ "$(cat "$T/state/state")" = "$before" ]'
watch --resend 2 >"$T/out"
check "resend repeats the notice of one capture" '[ "$(mails)" = 6 ] && last | grep -q "captura lista para procesar"'

# 8: with automatic processing, a capture about to be processed by itself is
# not announced; one that was left behind is, and the mail says why
capture 8 sala-a 1260 60
stream 8 30 closed NULL
joined 8 enabled
before=$(mails)
SESSIONWATCH_AUTO=1 watch >"$T/out"
check "automatic mode waits before announcing a pending capture" '[ "$(mails)" = "$before" ]'
sql "UPDATE capture_runs SET last_activity_at=$NOW-2400 WHERE id=8"
SESSIONWATCH_AUTO=1 watch >"$T/out"
check "and explains why one was left for the host" '[ "$(mails)" = $((before + 1)) ] && last | grep -q "No se procesó sola"'

echo
if [ "$fails" = 0 ]; then
	echo "sessionwatch.sh: todas las comprobaciones pasaron"
else
	echo "sessionwatch.sh: $fails comprobación(es) fallaron"
	exit 1
fi
