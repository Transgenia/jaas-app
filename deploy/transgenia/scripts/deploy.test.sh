#!/usr/bin/env bash
# Exercises deploy.sh end to end against a throwaway directory, a local git
# remote and a fake `docker`. It never touches the real service.
#   bash deploy/transgenia/scripts/deploy.test.sh
set -euo pipefail

HERE=$(cd "$(dirname "$0")" && pwd)
T=$(mktemp -d)
trap 'rm -rf -- "${T:?}"' EXIT
export STUB_STATE=$T/state STUB_BASE=$T/base
mkdir -p "$STUB_STATE" "$STUB_BASE"/{bin,data,backups} "$T/stubs"
: >"$STUB_STATE/calls"

# --- fake docker: remembers which tag "runs" and what each image reports.
cat >"$T/stubs/docker" <<'STUB'
#!/usr/bin/env bash
S=$STUB_STATE
echo "docker $*" >>"$S/calls"
running() { cat "$S/running_tag"; }
case $1 in
inspect)
	fmt=""
	while [ $# -gt 0 ]; do
		[ "$1" = --format ] && fmt=$2
		shift
	done
	case $fmt in
	*config_files*) echo "$STUB_BASE/compose.yaml" ;;
	*StartedAt*) echo "Contenedor: fake" ;;
	*Health*) if grep -qx "$(running)" "$S/unhealthy" 2>/dev/null; then echo unhealthy; else echo healthy; fi ;;
	*Config.Image*) echo "transgenia-meetings:$(running)" ;;
	esac
	;;
image) grep -qx "${3#transgenia-meetings:}" "$S/images" ;;
build)
	tag="" version=""
	while [ $# -gt 0 ]; do
		case $1 in
		-t) tag=${2#transgenia-meetings:} ;;
		VERSION=*) version=${1#VERSION=} ;;
		esac
		shift
	done
	[ ! -f "$S/tests_fail" ] || exit 1
	if [ -n "$tag" ]; then
		echo "$tag" >>"$S/images"
		echo "$version" >"$S/version_of_$tag"
	fi
	;;
run)
	dir="" db="" tag=""
	while [ $# -gt 0 ]; do
		case $1 in
		type=bind,source=*) dir=${1#type=bind,source=} dir=${dir%,target=/work} ;;
		DB_PATH=*) db=${1#DB_PATH=/work/} ;;
		transgenia-meetings:*)
			tag=${1#transgenia-meetings:}
			shift
			break
			;;
		esac
		shift
	done
	case ${1:-} in
	version) cat "$S/version_of_$tag" ;;
	backup)
		! grep -qx "$tag" "$S/cannot_open" 2>/dev/null || exit 1
		cp "$dir/$db" "$dir/${2#/work/}"
		;;
	esac
	;;
exec)
	case $4 in
	version) cat "$S/version_of_$(running)" 2>/dev/null ;;
	verify-db) echo "integrity_check=ok schema_version=10" ;;
	esac
	;;
compose) sed -n 's/^IMAGE_TAG=//p' "$STUB_BASE/.env" >"$S/running_tag" ;;
esac
STUB
chmod +x "$T/stubs/docker"

# --- fake installation
printf 'IMAGE_TAG=old-aaaaaaaaaaaa\nTGN_UID=1\nTGN_GID=1\n' >"$STUB_BASE/.env"
echo old-aaaaaaaaaaaa >"$STUB_STATE/running_tag"
echo old-aaaaaaaaaaaa >"$STUB_STATE/images"
echo "old-version" >"$STUB_STATE/version_of_old-aaaaaaaaaaaa"
: >"$STUB_BASE/compose.yaml"
cat >"$STUB_BASE/bin/backup.sh" <<'STUB'
#!/bin/sh
echo "backup.sh" >>"$STUB_STATE/calls"
dest="$STUB_BASE/backups/meetings-$(date -u +%Y%m%dT%H%M%SZ)-$$.db"
cp "$STUB_BASE/data/jaas.db" "$dest"
echo "backup ok: $dest"
STUB
chmod +x "$STUB_BASE/bin/backup.sh"
sqlite3 "$STUB_BASE/data/jaas.db" "
CREATE TABLE rooms(slug TEXT, active INTEGER);
CREATE TABLE capture_participation(ended_at INTEGER, updated_at INTEGER);
CREATE TABLE audio_streams(status TEXT, last_chunk_at INTEGER);
CREATE TABLE capture_runs(status TEXT, cancelled_at INTEGER);
CREATE TABLE minutes(status TEXT, cancelled_at INTEGER);
CREATE TABLE minutes_deliveries(status TEXT);
CREATE TABLE host_notifications(status TEXT);
CREATE TABLE odoo_sync_outbox(status TEXT);
INSERT INTO rooms VALUES('sala',0);"

# --- fake remote with a production branch
git init --quiet --bare "$T/origin.git"
git init --quiet "$T/work"
git -C "$T/work" config user.email test@example.test
git -C "$T/work" config user.name test
commit() {
	echo "$1" >"$T/work/file"
	git -C "$T/work" add file
	git -C "$T/work" commit --quiet -m "$1"
	git -C "$T/work" rev-parse HEAD
}
git -C "$T/work" checkout --quiet -b transgenia/production
FIRST=$(commit first)
git -C "$T/work" push --quiet "$T/origin.git" transgenia/production

deploy() {
	PATH="$T/stubs:$PATH" TGN_BASE=$STUB_BASE TGN_REPO_URL=$T/origin.git TGN_HEALTH_TIMEOUT=0 \
		bash "$HERE/deploy.sh" "$@"
}
tag() { sed -n 's/^IMAGE_TAG=//p' "$STUB_BASE/.env"; }
calls() { grep -c "$1" "$STUB_STATE/calls" || true; }
fails=0
check() {
	if eval "$2"; then
		echo "ok   $1"
	else
		echo "FAIL $1"
		fails=$((fails + 1))
	fi
}

deploy status >"$T/out" 2>&1
check "status reports the configured tag" 'grep -q "IMAGE_TAG): old-aaaaaaaaaaaa" "$T/out"'

deploy deploy --dry-run >"$T/out" 2>&1
check "dry run changes nothing" '[ "$(tag)" = old-aaaaaaaaaaaa ] && [ ! -e "$STUB_BASE/src" ] && [ -z "$(ls "$STUB_BASE/backups")" ]'
check "dry run does not build, back up or restart" '[ "$(calls "docker build")" = 0 ] && [ "$(calls backup.sh)" = 0 ] && [ "$(calls "docker compose")" = 0 ]'
check "dry run names the commit it would deploy" 'grep -q "$FIRST" "$T/out"'

deploy deploy >"$T/out" 2>&1
NEW=release-${FIRST:0:12}
check "deploy switches to the tested image" '[ "$(tag)" = "$NEW" ] && [ "$(cat "$STUB_STATE/running_tag")" = "$NEW" ]'
check "tests run before the release build" '[ "$(grep -n "docker build" "$STUB_STATE/calls" | head -1 | grep -c -- "--target test")" = 1 ]'
check "backup happens before the restart" '[ "$(grep -n "backup.sh\|docker compose" "$STUB_STATE/calls" | head -1 | grep -c backup.sh)" = 1 ]'
check "rehearsal copy is removed" '[ -z "$(find "$STUB_BASE/backups" -name ".rehearsal-*")" ]'
check "previous version is recorded" 'grep -qx "PREVIOUS_TAG=old-aaaaaaaaaaaa" "$STUB_BASE/.deploy-state"'
check "source checkout sits at the commit" '[ "$(git -C "$STUB_BASE/src" rev-parse HEAD)" = "$FIRST" ]'

before=$(calls "docker compose")
deploy deploy >"$T/out" 2>&1
check "redeploying the running commit is a no-op" 'grep -q "ya es la versión en ejecución" "$T/out" && [ "$(calls "docker compose")" = "$before" ]'

deploy rollback >"$T/out" 2>&1
check "rollback returns to the recorded version" '[ "$(tag)" = old-aaaaaaaaaaaa ] && grep -qx "PREVIOUS_TAG=$NEW" "$STUB_BASE/.deploy-state"'

SECOND=$(commit second)
git -C "$T/work" push --quiet "$T/origin.git" transgenia/production
BAD=release-${SECOND:0:12}
echo "$BAD" >"$STUB_STATE/unhealthy"
if deploy deploy >"$T/out" 2>&1; then status=0; else status=$?; fi
check "an unhealthy release is rolled back and reported" '[ "$status" != 0 ] && [ "$(tag)" = old-aaaaaaaaaaaa ] && [ "$(cat "$STUB_STATE/running_tag")" = old-aaaaaaaaaaaa ] && grep -q "NO quedó desplegada" "$T/out"'
: >"$STUB_STATE/unhealthy"

sqlite3 "$STUB_BASE/data/jaas.db" "UPDATE rooms SET active=1"
before=$(calls "docker compose")
if deploy deploy >"$T/out" 2>&1; then status=0; else status=$?; fi
check "a meeting in progress blocks the deploy" '[ "$status" != 0 ] && [ "$(calls "docker compose")" = "$before" ] && grep -q "reuniones activas: 1" "$T/out"'
sqlite3 "$STUB_BASE/data/jaas.db" "UPDATE rooms SET active=0"

echo old-aaaaaaaaaaaa >"$STUB_STATE/cannot_open"
before=$(calls "docker compose")
if deploy deploy >"$T/out" 2>&1; then status=0; else status=$?; fi
check "a one-way migration needs explicit acceptance" '[ "$status" != 0 ] && [ "$(calls "docker compose")" = "$before" ] && grep -q -- "--accept-one-way" "$T/out"'
deploy deploy --accept-one-way >"$T/out" 2>&1
check "accepted one-way migration deploys" '[ "$(tag)" = "$BAD" ]'
: >"$STUB_STATE/cannot_open"

git -C "$T/work" checkout --quiet -b unreviewed
THIRD=$(commit third)
git -C "$T/work" push --quiet "$T/origin.git" unreviewed
if deploy deploy "$THIRD" >"$T/out" 2>&1; then status=0; else status=$?; fi
check "a commit outside the production branch is refused" '[ "$status" != 0 ] && [ "$(tag)" = "$BAD" ] && grep -q "no está fusionado" "$T/out"'

touch "$STUB_STATE/tests_fail"
git -C "$T/work" checkout --quiet transgenia/production
FOURTH=$(commit fourth)
git -C "$T/work" push --quiet "$T/origin.git" transgenia/production
before=$(calls backup.sh)
if deploy deploy >"$T/out" 2>&1; then status=0; else status=$?; fi
check "failing tests stop before backup and restart" '[ "$status" != 0 ] && [ "$(tag)" = "$BAD" ] && [ "$(calls backup.sh)" = "$before" ] && grep -q "pruebas fallaron" "$T/out"'

echo
if [ "$fails" = 0 ]; then
	echo "deploy.sh: todas las comprobaciones pasaron"
else
	echo "deploy.sh: $fails comprobación(es) fallaron"
	exit 1
fi
