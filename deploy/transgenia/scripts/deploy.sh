#!/usr/bin/env bash
# One-command deploy of the meetings service on the Transgenia host.
# Installed as /opt/transgenia-meetings/bin/deploy.sh (see ../README.md).
#
# Only reviewed code is deployed: the commit must be merged into the
# production branch, the Dockerfile test stage must pass, and the database is
# backed up and the migration rehearsed on a private copy before the running
# version changes. A failed verification puts the previous image back.
set -euo pipefail
umask 077

BASE=${TGN_BASE:-/opt/transgenia-meetings}
SRC=${TGN_SRC:-$BASE/src}
REPO_URL=${TGN_REPO_URL:-https://github.com/Transgenia/jaas-app.git}
BRANCH=${TGN_BRANCH:-transgenia/production}
IMAGE=${TGN_IMAGE:-transgenia-meetings}
CONTAINER=${TGN_CONTAINER:-tgn-meetings}
SERVICE=${TGN_SERVICE:-meetings}
TAG_PREFIX=${TGN_TAG_PREFIX:-release}
HEALTH_TIMEOUT=${TGN_HEALTH_TIMEOUT:-120}
STATE=$BASE/.deploy-state
DB=$BASE/data/jaas.db

DRY=0 FORCE=0 ONE_WAY=0 TARGET="" ROLLBACK_TO=""
SHA="" TAG="" PREV="" BACKUP="" REHEARSAL=""
COMPOSE=()

usage() {
	cat <<EOF
Uso:
  deploy.sh status                      qué versión corre y si hay trabajo en curso
  deploy.sh deploy [COMMIT] [opciones]  despliega COMMIT (por defecto, la punta de $BRANCH)
  deploy.sh rollback [--to TAG]         regresa a la versión anterior registrada

Opciones:
  --dry-run          muestra cada paso; no construye, respalda ni cambia la versión
  --force            continúa aunque haya reuniones o trabajo en curso
  --accept-one-way   continúa aunque la versión anterior no pueda abrir la base migrada
EOF
	exit "${1:-0}"
}

say() { printf '%s\n' "$*"; }
step() { printf '\n== %s\n' "$*"; }
die() {
	printf 'ERROR: %s\n' "$*" >&2
	exit 1
}

# run executes a command that changes the host; with --dry-run it only prints it.
run() {
	if [ "$DRY" = 1 ]; then
		printf '   [simulación]'
		printf ' %q' "$@"
		printf '\n'
		return 0
	fi
	"$@"
}

cleanup() {
	if [ -n "$REHEARSAL" ] && [ -d "$REHEARSAL" ]; then
		rm -rf -- "${REHEARSAL:?}"
	fi
}
trap cleanup EXIT

need() {
	local tool
	for tool in "$@"; do
		command -v "$tool" >/dev/null 2>&1 || die "falta la herramienta $tool"
	done
}

preflight() {
	[ "$(id -u)" = 0 ] || die "ejecuta este script como root"
	need docker git sqlite3 flock
	[ -f "$BASE/.env" ] || die "no existe $BASE/.env"
	[ -x "$BASE/bin/backup.sh" ] || die "no existe $BASE/bin/backup.sh"
}

current_tag() { sed -n 's/^IMAGE_TAG=//p' "$BASE/.env" | head -1; }

valid_tag() { [[ $1 =~ ^[A-Za-z0-9_.-]+$ ]]; }

running_version() { docker exec "$CONTAINER" /app/jaas-app version 2>/dev/null | head -1 || true; }

# The running container knows which Compose files it was created from,
# overlays included. Never assume compose.yaml alone.
compose_args() {
	local files f
	files=${TGN_COMPOSE_FILES:-$(docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' "$CONTAINER" 2>/dev/null || true)}
	[ -n "$files" ] || die "no se pudo leer la lista de archivos Compose de $CONTAINER; indícala en TGN_COMPOSE_FILES separada por comas"
	COMPOSE=(docker compose --project-directory "$BASE" --env-file "$BASE/.env")
	local IFS=,
	for f in $files; do
		[ -f "$f" ] || die "falta el archivo Compose $f"
		COMPOSE+=(-f "$f")
	done
}

# pending_work prints one line per kind of work that a restart would interrupt.
# Counts only; nothing about rooms, people or content.
pending_work() {
	sqlite3 -readonly "file:$DB?mode=ro" "
	SELECT 'reuniones activas: '||n FROM (SELECT COUNT(*) n FROM rooms WHERE active=1) WHERE n>0;
	SELECT 'conexiones capturando: '||n FROM (SELECT COUNT(*) n FROM capture_participation WHERE ended_at IS NULL AND updated_at>=strftime('%s','now')-120) WHERE n>0;
	SELECT 'pistas grabándose: '||n FROM (SELECT COUNT(*) n FROM audio_streams WHERE status='recording' AND last_chunk_at>=strftime('%s','now')-300) WHERE n>0;
	SELECT 'capturas transcribiéndose: '||n FROM (SELECT COUNT(*) n FROM capture_runs WHERE status='transcribing' AND cancelled_at IS NULL) WHERE n>0;
	SELECT 'minutas en cola o en proceso: '||n FROM (SELECT COUNT(*) n FROM minutes WHERE status IN ('queued','processing') AND cancelled_at IS NULL) WHERE n>0;
	SELECT 'correos en cola: '||n FROM (SELECT (SELECT COUNT(*) FROM minutes_deliveries WHERE status='queued')+(SELECT COUNT(*) FROM host_notifications WHERE status='queued') n) WHERE n>0;
	SELECT 'eventos de Odoo pendientes: '||n FROM (SELECT COUNT(*) n FROM odoo_sync_outbox WHERE status IN ('pending','processing')) WHERE n>0;"
}

check_idle() {
	local work
	work=$(pending_work) || die "no se pudo revisar el trabajo en curso en $DB"
	if [ -z "$work" ]; then
		say "   sin reuniones, capturas, minutas ni correos en curso"
		return 0
	fi
	say "   un reinicio interrumpiría:"
	printf '%s\n' "$work" | sed 's/^/     /'
	[ "$FORCE" = 1 ] || die "espera a que termine o repite con --force; no se cambió nada"
	say "   --force: se continúa de todos modos"
}

lock() {
	[ "$DRY" = 1 ] && return 0
	exec 9>"$BASE/.deploy.lock"
	flock -n 9 || die "ya hay otro despliegue en curso"
}

# resolve_target sets SHA. The checkout is created on the first real deploy;
# a dry run without it asks the remote instead of creating anything.
resolve_target() {
	if [ ! -d "$SRC/.git" ]; then
		if [ "$DRY" = 1 ]; then
			say "   [simulación] git clone --branch $BRANCH $REPO_URL $SRC"
			SHA=$(git ls-remote "$REPO_URL" "refs/heads/$BRANCH" | cut -f1)
			[ -n "$SHA" ] || die "no se pudo consultar $BRANCH en $REPO_URL"
			[ -z "$TARGET" ] || say "   sin checkout no se puede comprobar $TARGET; se muestra la punta de $BRANCH"
			return 0
		fi
		git clone --quiet --branch "$BRANCH" "$REPO_URL" "$SRC"
	fi
	git -C "$SRC" fetch --quiet --prune origin
	[ -z "$(git -C "$SRC" status --porcelain)" ] || die "$SRC tiene cambios locales; no se despliega desde un árbol modificado"
	SHA=$(git -C "$SRC" rev-parse --verify --quiet "${TARGET:-origin/$BRANCH}^{commit}") || die "no existe el commit ${TARGET:-origin/$BRANCH}"
	git -C "$SRC" merge-base --is-ancestor "$SHA" "origin/$BRANCH" || die "el commit $SHA no está fusionado en $BRANCH"
}

build_image() {
	say "   pruebas del Dockerfile (gofmt, vet, go test) y construcción; tarda unos minutos"
	if [ "$DRY" = 1 ]; then
		run docker build --target test "$SRC"
		run docker build --build-arg "VERSION=$SHA" -t "$IMAGE:$TAG" "$SRC"
		return 0
	fi
	local log reported
	log=$(mktemp)
	if ! docker build --target test "$SRC" >"$log" 2>&1; then
		tail -n 40 "$log"
		rm -f -- "${log:?}"
		die "las pruebas fallaron; no se cambió nada"
	fi
	if ! docker build --build-arg "VERSION=$SHA" -t "$IMAGE:$TAG" "$SRC" >"$log" 2>&1; then
		tail -n 40 "$log"
		rm -f -- "${log:?}"
		die "la construcción de la imagen falló; no se cambió nada"
	fi
	rm -f -- "${log:?}"
	reported=$(docker run --rm --network none --read-only "$IMAGE:$TAG" version | head -1)
	[ "$reported" = "$SHA" ] || die "la imagen reporta la versión '$reported', no $SHA"
	say "   pruebas en verde; imagen $IMAGE:$TAG"
}

# backup.sh verifies the snapshot with the image named by IMAGE_TAG, so it
# must run before that line changes.
take_backup() {
	if [ "$DRY" = 1 ]; then
		run "$BASE/bin/backup.sh"
		return 0
	fi
	local out
	out=$("$BASE/bin/backup.sh") || die "el respaldo falló; no se cambió nada"
	BACKUP=$(printf '%s\n' "$out" | sed -n 's/^backup ok: //p' | tail -1)
	[ -n "$BACKUP" ] && [ -f "$BACKUP" ] || die "el respaldo no informó su archivo; no se cambió nada"
	say "   $BACKUP"
}

# rehearse migrates a private copy of the fresh backup with the new image,
# compares every table and checks that the image now running can still open
# the migrated database. The copy holds customer data and is always removed.
rehearse() {
	if [ "$DRY" = 1 ]; then
		say "   [simulación] migrar una copia privada del respaldo con $IMAGE:$TAG, comparar filas por tabla,"
		say "                comprobar que $IMAGE:$PREV abre la base migrada y borrar la copia"
		return 0
	fi
	local table before after diffs=0
	REHEARSAL=$(mktemp -d "$BASE/backups/.rehearsal-XXXXXX")
	cp -p "$BACKUP" "$REHEARSAL/source.db"
	docker run --rm --network none --read-only --user 0:0 --mount "type=bind,source=$REHEARSAL,target=/work" \
		-e DB_PATH=/work/source.db "$IMAGE:$TAG" backup /work/migrated.db >/dev/null ||
		die "la migración de ensayo falló; no se cambió nada"
	[ "$(sqlite3 -readonly "$REHEARSAL/migrated.db" 'PRAGMA integrity_check')" = ok ] || die "la base migrada en el ensayo no pasó integrity_check; no se cambió nada"
	for table in $(sqlite3 -readonly "$BACKUP" "SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name"); do
		before=$(sqlite3 -readonly "$BACKUP" "SELECT COUNT(*) FROM \"$table\"")
		after=$(sqlite3 -readonly "$REHEARSAL/migrated.db" "SELECT COUNT(*) FROM \"$table\"")
		if [ "$before" != "$after" ]; then
			say "   $table: $before filas antes, $after después"
			diffs=$((diffs + 1))
		fi
	done
	[ "$diffs" = 0 ] || die "la migración de ensayo cambió el número de filas; no se cambió nada"
	say "   esquema $(sqlite3 -readonly "$BACKUP" 'PRAGMA user_version') → $(sqlite3 -readonly "$REHEARSAL/migrated.db" 'PRAGMA user_version'), mismas filas en todas las tablas"
	cp -p "$REHEARSAL/migrated.db" "$REHEARSAL/back.db"
	if docker run --rm --network none --read-only --user 0:0 --mount "type=bind,source=$REHEARSAL,target=/work" \
		-e DB_PATH=/work/back.db "$IMAGE:$PREV" backup /work/back-out.db >/dev/null 2>&1; then
		say "   la versión en ejecución ($PREV) abre la base migrada: la reversión es directa"
	elif [ "$ONE_WAY" = 1 ]; then
		say "   AVISO: $PREV no abre la base migrada; regresar exigirá restaurar el respaldo (--accept-one-way)"
	else
		die "la versión en ejecución ($PREV) no abre la base migrada: no habría reversión cambiando solo la imagen. Repite con --accept-one-way si es intencional; no se cambió nada"
	fi
	cleanup
	REHEARSAL=""
}

switch_to() {
	local stamp
	stamp=$(date -u +%Y%m%dT%H%M%SZ)
	run cp -p "$BASE/.env" "$BASE/.env.bak-$stamp"
	run sed -i "s/^IMAGE_TAG=.*/IMAGE_TAG=$1/" "$BASE/.env"
	run "${COMPOSE[@]}" up -d "$SERVICE"
}

wait_healthy() {
	local deadline=$((SECONDS + HEALTH_TIMEOUT)) health
	while :; do
		health=$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}sin-healthcheck{{end}}' "$CONTAINER" 2>/dev/null || echo ausente)
		[ "$health" = healthy ] && return 0
		if [ "$SECONDS" -ge "$deadline" ]; then
			say "   estado del contenedor: $health tras ${HEALTH_TIMEOUT}s"
			return 1
		fi
		sleep 3
	done
}

# verify_running TAG [VERSION]: healthy, the expected image, the expected
# commit when known, and a database that passes its integrity check.
verify_running() {
	local image reported out
	wait_healthy || return 1
	image=$(docker inspect --format '{{.Config.Image}}' "$CONTAINER")
	if [ "$image" != "$IMAGE:$1" ]; then
		say "   imagen en ejecución: $image"
		return 1
	fi
	if [ -n "${2:-}" ]; then
		reported=$(running_version)
		if [ "$reported" != "$2" ]; then
			say "   versión reportada: $reported"
			return 1
		fi
	fi
	out=$(docker exec "$CONTAINER" /app/jaas-app verify-db /data/jaas.db) || return 1
	printf '%s\n' "$out" | sed -n '1p' | sed 's/^/   /'
	printf '%s\n' "$out" | grep -q '^integrity_check=ok' || return 1
}

write_state() {
	[ "$DRY" = 1 ] && return 0
	printf 'PREVIOUS_TAG=%s\nCURRENT_TAG=%s\nVERSION=%s\nBACKUP=%s\nAT=%s\n' \
		"$1" "$2" "$3" "$4" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$STATE.tmp"
	mv "$STATE.tmp" "$STATE"
}

cmd_status() {
	need docker sqlite3
	local work
	say "Versión configurada (IMAGE_TAG): $(current_tag)"
	docker inspect --format 'Contenedor: {{.Config.Image}} · {{.State.Status}}{{if .State.Health}} · {{.State.Health.Status}}{{end}} · desde {{.State.StartedAt}}' "$CONTAINER" 2>/dev/null || say "Contenedor: ausente"
	say "Commit en ejecución: $(running_version)"
	say "Esquema de la base: $(sqlite3 -readonly "file:$DB?mode=ro" 'PRAGMA user_version' 2>/dev/null || echo desconocido)"
	say "Último respaldo: $(find "$BASE/backups" -maxdepth 1 -name 'meetings-*.db' 2>/dev/null | sort | tail -1)"
	if [ -f "$STATE" ]; then
		say "Versión anterior (para rollback): $(sed -n 's/^PREVIOUS_TAG=//p' "$STATE")"
	else
		say "Versión anterior (para rollback): sin registro; usa rollback --to TAG"
	fi
	if [ -d "$SRC/.git" ]; then
		say "Código: $SRC en $(git -C "$SRC" rev-parse --short=12 HEAD)"
	else
		say "Código: $SRC todavía no existe; lo crea el primer deploy"
	fi
	work=$(pending_work 2>/dev/null) || work="no se pudo consultar"
	say "Trabajo en curso: ${work:-ninguno}"
}

cmd_deploy() {
	preflight
	PREV=$(current_tag)
	[ -n "$PREV" ] || die "no hay IMAGE_TAG en $BASE/.env"

	step "1. Código"
	resolve_target
	TAG=$TAG_PREFIX-${SHA:0:12}
	valid_tag "$TAG" || die "etiqueta inválida: $TAG"
	say "   commit $SHA"
	say "   en ejecución: $IMAGE:$PREV → nueva: $IMAGE:$TAG"
	if [ "$(running_version)" = "$SHA" ]; then
		say
		say "Ese commit ya es la versión en ejecución; no hay nada que desplegar."
		return 0
	fi
	if [ -d "$SRC/.git" ]; then
		run git -C "$SRC" checkout --quiet --detach "$SHA"
		if [ "$DRY" = 0 ] && [ -f "$SRC/deploy/transgenia/scripts/deploy.sh" ] && ! cmp -s "$0" "$SRC/deploy/transgenia/scripts/deploy.sh"; then
			say "   AVISO: este script difiere del incluido en ese commit; reinstálalo después (ver README)"
		fi
	fi

	step "2. Pruebas e imagen"
	build_image

	step "3. Trabajo en curso"
	compose_args
	check_idle
	lock

	step "4. Respaldo"
	take_backup

	step "5. Ensayo de la migración"
	rehearse

	step "6. Cambio de versión"
	switch_to "$TAG"

	step "7. Verificación"
	if [ "$DRY" = 1 ]; then
		say "   [simulación] esperar healthy, comprobar imagen, commit y verify-db; si falla, regresar a $PREV"
		say
		say "Simulación terminada: no se construyó, respaldó ni cambió nada."
		return 0
	fi
	if verify_running "$TAG" "$SHA"; then
		write_state "$PREV" "$TAG" "$SHA" "$BACKUP"
		say
		say "Desplegado: $IMAGE:$TAG ($SHA)."
		say "Para regresar: $0 rollback   (versión anterior: $PREV; respaldo: $BACKUP)"
		return 0
	fi
	say
	say "La versión nueva no pasó la verificación; se regresa a $PREV."
	switch_to "$PREV"
	if verify_running "$PREV"; then
		die "se regresó a $PREV y el servicio está sano. La versión nueva NO quedó desplegada; la base conserva la migración. Respaldo previo: $BACKUP"
	fi
	die "tampoco se pudo regresar a $PREV: el servicio requiere atención inmediata. Respaldo previo: $BACKUP"
}

cmd_rollback() {
	preflight
	local current
	current=$(current_tag)
	PREV=$current
	TAG=${ROLLBACK_TO:-$(sed -n 's/^PREVIOUS_TAG=//p' "$STATE" 2>/dev/null || true)}
	[ -n "$TAG" ] || die "no hay versión anterior registrada; indica una con --to TAG (docker images $IMAGE)"
	valid_tag "$TAG" || die "etiqueta inválida: $TAG"
	[ "$TAG" != "$current" ] || die "la versión configurada ya es $TAG"
	docker image inspect "$IMAGE:$TAG" >/dev/null 2>&1 || die "no existe la imagen $IMAGE:$TAG en este servidor"

	step "1. Trabajo en curso"
	say "   en ejecución: $IMAGE:$current → se regresa a: $IMAGE:$TAG"
	compose_args
	check_idle
	lock

	step "2. Respaldo"
	take_backup

	step "3. Cambio de versión"
	switch_to "$TAG"

	step "4. Verificación"
	if [ "$DRY" = 1 ]; then
		say "   [simulación] esperar healthy, comprobar imagen y verify-db"
		say
		say "Simulación terminada: no se respaldó ni cambió nada."
		return 0
	fi
	if verify_running "$TAG"; then
		write_state "$current" "$TAG" "" "$BACKUP"
		say
		say "Se regresó a $IMAGE:$TAG. La base no se restauró: conserva su esquema actual."
		return 0
	fi
	die "la versión $TAG no quedó sana. El archivo .env apunta a $TAG; la anterior era $current. Respaldo previo: $BACKUP"
}

[ $# -ge 1 ] || usage 1
command=$1
shift
while [ $# -gt 0 ]; do
	case $1 in
	--dry-run) DRY=1 ;;
	--force) FORCE=1 ;;
	--accept-one-way) ONE_WAY=1 ;;
	--to)
		shift
		ROLLBACK_TO=${1:-}
		[ -n "$ROLLBACK_TO" ] || die "--to necesita una etiqueta"
		;;
	-h | --help) usage ;;
	-*) die "opción desconocida: $1" ;;
	*)
		[ -z "$TARGET" ] || die "solo se admite un commit"
		TARGET=$1
		;;
	esac
	shift
done

case $command in
status) cmd_status ;;
deploy) cmd_deploy ;;
rollback) cmd_rollback ;;
-h | --help | help) usage ;;
*) usage 1 ;;
esac
