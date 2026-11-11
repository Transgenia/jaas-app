#!/usr/bin/env bash
# Tells the host what happened to each captured meeting, so nobody has to open
# the panel to find out. Run by tgn-meetings-sessionwatch.timer.
#
# One e-mail per change that needs a person or closes the loop: capture waiting
# to be processed, meeting that ended without audio, failed capture, minutes
# that need review, minutes ready, minutes delivered. Each one carries the
# checks a person would do by hand: how much audio there is against the length
# of the meeting, how long capture was paused, how much text came out, and
# whether delivery and the Odoo sync went through.
#
# It only reads the database. The mail names the room, the capture and counts;
# never people, addresses, transcript or minutes text.
#
#   sessionwatch.sh [--dry-run] [--resend CAPTURE_ID] [--days N]
set -euo pipefail
umask 077

BASE=${TGN_BASE:-/opt/transgenia-meetings}
DB=$BASE/data/jaas.db
NOTIFY=${SESSIONWATCH_NOTIFY:-$BASE/bin/notify.sh}
FALLBACK_TO=${SESSIONWATCH_TO:-admin@transgenia.org}
URL=${SESSIONWATCH_URL:-https://meet.transgenia.org}
UTC_OFFSET=${SESSIONWATCH_UTC_OFFSET:--6 hours} # display only; Mexico City
IDLE=${SESSIONWATCH_IDLE:-600}                  # seconds without activity before a meeting counts as ended
STATE_DIR=${STATE_DIRECTORY:-/var/lib/tgn-meetings-sessionwatch}
STATE=$STATE_DIR/state

DRY=0 RESEND="" DAYS=14
while [ $# -gt 0 ]; do
	case $1 in
	--dry-run) DRY=1 ;;
	--resend)
		shift
		RESEND=${1:-}
		;;
	--days)
		shift
		DAYS=${1:-}
		;;
	*)
		echo "uso: sessionwatch.sh [--dry-run] [--resend CAPTURA] [--days N]" >&2
		exit 2
		;;
	esac
	shift
done
[[ $DAYS =~ ^[0-9]+$ ]] || {
	echo "--days necesita un número" >&2
	exit 2
}
[[ -z $RESEND || $RESEND =~ ^[0-9]+$ ]] || {
	echo "--resend necesita el número de captura" >&2
	exit 2
}
[[ $UTC_OFFSET =~ ^[+-][0-9]+\ hours$ ]] || {
	echo "SESSIONWATCH_UTC_OFFSET inválido" >&2
	exit 2
}

minutes() { echo $((($1 + 30) / 60)); }

declare -A seen=()
baseline=0
if [ -f "$STATE" ]; then
	while IFS=$'\x1f' read -r id situation; do
		[ -n "$id" ] && seen[$id]=$situation
	done <"$STATE"
else
	baseline=1 # first run: record what exists, do not mail history
fi

now=$(date +%s)
rows=$(sqlite3 -readonly -separator $'\x1f' "file:$DB?mode=ro" "
SELECT r.id, r.room_slug, o.owner_email,
 strftime('%Y-%m-%d %H:%M', r.started_at, 'unixepoch', '$UTC_OFFSET'),
 r.status, r.processing_approved_at IS NOT NULL,
 r.cancelled_at IS NOT NULL OR m.cancelled_at IS NOT NULL, COALESCE(r.error,''),
 $now - r.last_activity_at,
 (SELECT COUNT(*) FROM audio_streams s WHERE s.run_id=r.id),
 (SELECT COALESCE(SUM(MAX(COALESCE(s.ended_at_ms, s.last_chunk_at*1000) - s.started_at, 0)),0)/1000 FROM audio_streams s WHERE s.run_id=r.id),
 MAX(COALESCE(r.ended_at,0), r.last_activity_at) - r.started_at,
 (SELECT COUNT(*) FROM capture_participation p WHERE p.run_id=r.id AND p.choice='enabled'),
 (SELECT COUNT(*) FROM capture_participation p WHERE p.run_id=r.id AND p.choice!='enabled'),
 (SELECT COALESCE(SUM(p.paused_seconds + CASE WHEN p.paused_since IS NULL THEN 0 ELSE MAX(COALESCE(p.ended_at,p.updated_at)-p.paused_since,0) END),0) FROM capture_participation p WHERE p.run_id=r.id),
 (SELECT COUNT(*) FROM capture_participation p WHERE p.run_id=r.id AND p.ended_at IS NULL AND p.updated_at >= $now-120)
  + (SELECT COUNT(*) FROM audio_streams s WHERE s.run_id=r.id AND s.status='recording' AND s.last_chunk_at >= $now-300),
 (SELECT COUNT(*) FROM audio_streams s WHERE s.run_id=r.id AND s.status='failed' AND s.excluded_at IS NULL),
 (SELECT COUNT(*) FROM audio_streams s WHERE s.run_id=r.id AND s.status='transcribed' AND COALESCE(s.transcript,'') IN ('','[]','null')),
 (SELECT COUNT(*) FROM audio_streams s WHERE s.run_id=r.id AND s.excluded_at IS NOT NULL),
 COALESCE(m.id,0), COALESCE(m.status,''), COALESCE(m.error,''), COALESCE(m.result_json,'')!='',
 COALESCE((SELECT SUM(LENGTH(json_extract(j.value,'\$.content'))) FROM transcriptions t, json_each(t.data,'\$.messages') j
   WHERE t.room_slug=r.room_slug AND t.session_id='captura-'||r.id),0),
 (SELECT COUNT(*) FROM minutes_deliveries d WHERE d.minutes_id=m.id AND d.status='sent'),
 (SELECT COUNT(*) FROM minutes_deliveries d WHERE d.minutes_id=m.id AND d.status='failed'),
 (SELECT COUNT(*) FROM minutes_deliveries d WHERE d.minutes_id=m.id AND d.status='queued'),
 COALESCE((SELECT x.error_code FROM odoo_sync_outbox x WHERE x.minutes_id=m.id AND x.status='failed' LIMIT 1),'')
FROM capture_runs r JOIN rooms o ON o.slug=r.room_slug
LEFT JOIN minutes m ON m.room_slug=r.room_slug AND m.session_id='captura-'||r.id
WHERE r.started_at >= $now - $DAYS*86400
ORDER BY r.id;")

sent=0 failed_mail=0
declare -A next=()
while IFS=$'\x1f' read -r id room owner started status approved cancelled error idle streams audio_s span_s enabled not_enabled paused_s live \
	failed_streams silent excluded minutes_id minutes_status minutes_error has_draft text_chars delivered undelivered queued odoo_error; do
	[ -n "$id" ] || continue

	# --- where this capture stands, and whether that is worth a mail
	kind=""
	if [ "$cancelled" = 1 ]; then
		situation=cancelada
	elif [ "$status" = open ] && [ "$approved" = 0 ]; then
		if [ "$live" -gt 0 ] || [ "$idle" -lt "$IDLE" ]; then
			situation=en_curso
		elif [ "$streams" -gt 0 ]; then
			situation=por_procesar kind=por_procesar
		elif [ $((enabled + not_enabled)) -gt 0 ]; then
			situation=sin_audio kind=sin_audio
		else
			situation=vacia
		fi
	elif [ "$status" = failed ]; then
		situation="captura_fallida:$error" kind=captura_fallida
	elif [ "$status" = done ] && [ "$minutes_status" = failed ]; then
		situation="minuta_fallida:$minutes_error" kind=minuta_fallida
	elif [ "$status" = done ] && [ "$minutes_status" = ready ]; then
		if [ "$queued" -gt 0 ]; then
			situation=enviando
		elif [ $((delivered + undelivered)) -gt 0 ]; then
			situation="enviada:$delivered:$undelivered" kind=enviada
		else
			situation=minuta_lista kind=minuta_lista
		fi
		[ -z "$odoo_error" ] || situation="$situation:odoo=$odoo_error"
	else
		situation=procesando
	fi
	next[$id]=$situation
	previous=${seen[$id]:-}
	[ "$RESEND" != "$id" ] || previous=""
	[ -n "$kind" ] || continue
	if [ "$baseline" = 1 ] && [ "$RESEND" != "$id" ]; then continue; fi
	[ "$situation" != "$previous" ] || continue

	# --- the checks a person would do by hand
	notes=()
	audio_min=$(minutes "$audio_s") span_min=$(minutes "$span_s")
	if [ "$enabled" -gt 0 ] && [ "$span_s" -ge 300 ] && [ $((audio_s * 2)) -lt "$span_s" ]; then
		notes+=("Poco audio: $audio_min min en una reunión de $span_min min. Revisa si el micrófono estuvo silenciado en Jitsi.")
	fi
	if [ "$enabled" = 0 ] && [ "$not_enabled" -gt 0 ]; then
		notes+=("Nadie entró con la captura activada.")
	fi
	words=$((text_chars / 6)) rate=0
	[ "$audio_s" -lt 60 ] || rate=$((words * 60 / audio_s))
	if [ "$text_chars" -gt 0 ] && [ "$audio_s" -ge 300 ] && [ "$rate" -lt 20 ]; then
		notes+=("Transcripción escasa: unas $rate palabras por minuto de audio.")
	fi
	[ "$failed_streams" = 0 ] || notes+=("$failed_streams pista(s) no se pudieron transcribir.")
	[ "$excluded" = 0 ] || notes+=("$excluded pista(s) quedaron fuera de la minuta por decisión del anfitrión.")
	[ "$undelivered" = 0 ] || notes+=("$undelivered correo(s) a participantes no se entregaron.")
	[ -z "$odoo_error" ] || notes+=("La sincronización con Odoo falló ($odoo_error).")

	link=$URL/admin/captures
	case $kind in
	por_procesar)
		subject="captura lista para procesar"
		action="Abre «Sesiones, audio y minutas» y pulsa «Procesar con IA»."
		;;
	sin_audio)
		subject="la reunión terminó sin audio"
		action="No hay nada que procesar. Si esperabas grabación, revisa la cobertura en «Audio y cobertura»."
		link=$URL/admin/captures/$id/audio
		;;
	captura_fallida)
		subject="la captura requiere revisión"
		action="Motivo: $error. Abre «Audio y cobertura» para reintentar las pistas fallidas o continuar sin ellas."
		link=$URL/admin/captures/$id/audio
		;;
	minuta_fallida)
		subject="la minuta requiere revisión"
		link=$URL/admin/minutes/$minutes_id
		if [ "$has_draft" = 1 ]; then
			action="El borrador está guardado. Ábrelo, revisa los elementos señalados y consérvalo sin ellos."
		else
			action="Motivo: $minutes_error. Abre la minuta para ver si conviene reintentar."
		fi
		;;
	minuta_lista)
		subject="minuta lista"
		action="Revísala y aprueba el envío a participantes."
		link=$URL/admin/minutes/$minutes_id
		;;
	enviada)
		subject="minuta enviada a $delivered participante(s)"
		action="No queda nada pendiente de esta reunión."
		[ "$undelivered" = 0 ] || action="Revisa los correos no entregados en la página de la minuta."
		[ -z "$odoo_error" ] || action="$action Falta corregir la sincronización con Odoo."
		link=$URL/admin/minutes/$minutes_id
		;;
	esac
	[ ${#notes[@]} = 0 ] || subject="$subject, con observaciones"

	body=$(
		echo "Sala: $room"
		echo "Captura $id, iniciada el $started (hora de Ciudad de México)"
		echo
		if [ ${#notes[@]} -gt 0 ]; then
			echo "Observaciones"
			printf -- '- %s\n' "${notes[@]}"
			echo
		fi
		echo "Audio"
		echo "- $streams pista(s), $audio_min min de audio en $span_min min de reunión"
		echo "- Conexiones: $enabled con captura activada, $not_enabled sin captura"
		[ "$paused_s" = 0 ] || echo "- Captura en pausa por micrófono silenciado: $(minutes "$paused_s") min"
		[ "$silent" = 0 ] || echo "- Pistas sin voz reconocible: $silent"
		if [ "$text_chars" -gt 0 ]; then
			echo
			echo "Transcripción"
			echo "- Unas $words palabras, cerca de $rate por minuto de audio"
		fi
		if [ "$minutes_id" != 0 ]; then
			echo
			echo "Minuta"
			case $minutes_status in
			ready) echo "- Lista" ;;
			failed) echo "- No se generó o requiere revisión" ;;
			*) echo "- En proceso" ;;
			esac
			[ $((delivered + undelivered)) = 0 ] || echo "- Enviada a $delivered participante(s); $undelivered sin entregar"
		fi
		echo
		echo "$action"
		echo "$link"
	)

	to=$FALLBACK_TO
	[[ $owner =~ ^[^[:space:]@]+@[^[:space:]@]+$ ]] && to=$owner
	if [ "$DRY" = 1 ]; then
		printf '[simulación] aviso a %s\nAsunto: [Meet] %s: %s\n%s\n\n' "${to#*@}" "$room" "$subject" "$body"
		continue
	fi
	if printf '%s\n' "$body" | "$NOTIFY" "$to" "[Meet] $room: $subject" >/dev/null; then
		sent=$((sent + 1))
	else
		failed_mail=$((failed_mail + 1))
		next[$id]=$previous # try again on the next run
	fi
done <<<"$rows"

if [ "$DRY" = 1 ]; then
	echo "simulación: ${#next[@]} captura(s) revisadas; no se envió ni se guardó nada"
	exit 0
fi
mkdir -p "$STATE_DIR"
tmp=$(mktemp "$STATE_DIR/.state-XXXXXX")
for id in "${!next[@]}"; do
	printf '%s\x1f%s\n' "$id" "${next[$id]}"
done | sort -n >"$tmp"
mv "$tmp" "$STATE"
if [ "$baseline" = 1 ]; then
	echo "línea base: ${#next[@]} captura(s) registradas sin avisar del historial; avisos enviados: $sent"
else
	echo "revisadas: ${#next[@]} captura(s); avisos enviados: $sent"
fi
[ "$failed_mail" = 0 ] || {
	echo "$failed_mail aviso(s) no se pudieron enviar; se reintentan en la siguiente corrida" >&2
	exit 1
}
