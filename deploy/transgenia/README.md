# Transgenia meetings (meet.transgenia.org)

Hardened fork of `cbrake/jaas-app` (used with the author's permission) that
adds signed JaaS webhooks, server-side sessions, CSRF, rate limits, a durable
minutes queue and an OpenAI Responses API worker. Minutes are **drafts for
human review**: the service never sends e-mail or creates tasks.

## Layout on the server

```
/opt/transgenia-meetings/            root:root 0755
├── compose.yaml                     from deploy/transgenia/compose.yaml
├── .env                             IMAGE_TAG, TGN_UID, TGN_GID (non-secret)
├── meetings.env                     from meetings.env.example, root:tgn-meetings 0640
├── secrets/                         root:tgn-meetings 0750, files 0440
│   ├── jaas_private_key             JaaS API key (PEM)
│   ├── jaas_webhook_secret          JaaS webhook endpoint secret ("Reveal secret")
│   ├── openai_api_key               dedicated project key for this service
│   └── admin_initial_password       only read on first start (bootstrap)
├── data/                            tgn-meetings 0700 — SQLite (WAL)
├── backups/                         root 0700 — verified snapshots + sha256
└── bin/                             scripts from deploy/transgenia/scripts
```

Container `tgn-meetings` (Compose project `transgenia-meetings`): non-root
(uid of the `tgn-meetings` system user), read-only root filesystem, all
capabilities dropped, `no-new-privileges`, 512 MB / 128 pids, published only on
`127.0.0.1:8370`.

## Build and deploy a commit

```sh
cd <checkout del fork> && git checkout <sha>
docker build --target test .                                # must pass
docker build --build-arg VERSION=$(git rev-parse --short HEAD) -t transgenia-meetings:$(git rev-parse --short HEAD) .
sed -i "s/^IMAGE_TAG=.*/IMAGE_TAG=$(git rev-parse --short HEAD)/" /opt/transgenia-meetings/.env
cd /opt/transgenia-meetings && docker compose up -d && docker compose ps
```

Never use the upstream `update` command (removed here): updates go through a
reviewed commit, the test stage and a tagged image. Roll back by setting the
previous `IMAGE_TAG` and `docker compose up -d`.

## Secrets

Load each one with `bin/set-secret.sh <name>` (no echo, no argv, no history),
then `docker compose up -d --force-recreate`. Nothing secret lives in
`meetings.env`, Compose files, Git or logs.

## nginx and TLS (first time)

1. DNS: `A meet → <IP del origen>`, **DNS only** while the certificate is issued.
2. `bin/update-cloudflare-ips.sh` (writes the geo/zone/log-format conf and the real-IP snippet, `nginx -t`, reload).
3. Temporary port-80-only server for ACME, `certbot certonly --webroot -w /var/www/acme -d meet.transgenia.org`.
4. Install `nginx/meet.transgenia.org.conf`, `nginx -t && systemctl reload nginx`.
5. Switch the record to **Proxied**; SSL mode Full (strict) for the hostname.
6. Cloudflare Access application on `meet.transgenia.org/admin`, policy: `admin@transgenia.org`.
   Put the team domain and AUD tag in `meetings.env`, set `REQUIRE_CF_ACCESS=true`, recreate.
7. JaaS console → Webhooks: `https://meet.transgenia.org/webhook/recording`,
   events `TRANSCRIPTION_UPLOADED` and `RECORDING_UPLOADED`; copy the endpoint
   secret into `secrets/jaas_webhook_secret` **before** enabling the endpoint.

## Backups

`bin/backup.sh` (daily via `tgn-meetings-backup.timer`): `VACUUM INTO` inside
the container, `verify-db` (integrity check + counts) with the same image and
no network, sha256, keeps 14. Restore:

```sh
cd /opt/transgenia-meetings && docker compose stop
cp -p data/jaas.db data/jaas.db.before-restore-$(date -u +%Y%m%dT%H%MZ)
rm -f data/jaas.db-wal data/jaas.db-shm
install -o tgn-meetings -g tgn-meetings -m 0600 backups/meetings-<ts>.db data/jaas.db
docker compose up -d
```

## Admin access

**Identity mode (production).** With `CF_ACCESS_TEAM_DOMAIN`/`CF_ACCESS_AUD`
set, Cloudflare Access authenticates each person (one-time PIN to their
`@transgenia.org` mailbox) and the app verifies the Access JWT on every
`/admin` request. There is no shared password: each person creates and sees
only their own rooms, transcripts and minutes (`rooms.owner_email`);
`ADMIN_EMAILS` see and manage everything. Rooms of other users answer 404.
If the Access identity changes, the old panel session is revoked. Logout also
ends the Access session (`/cdn-cgi/access/logout`).

**Password mode (break-glass / local).** Without Access the panel falls back
to the single admin password:

* First login uses the temporary password from `secrets/admin_initial_password`
  and forces a change (min. 14 chars); every change revokes all sessions.
* Lost password: `docker exec -i tgn-meetings /app/jaas-app reset-admin` (reads
  a new temporary password from stdin).
* Sessions: random 256-bit tokens stored hashed, `__Host-` cookies (Secure,
  HttpOnly, SameSite=Lax), 2 h idle / 12 h absolute, revoked on logout.

## Retention

Deleting a room deletes its recordings, transcripts, minutes and participant
sessions (webhook idempotency keys are kept 30 days without room/session).
Recording links expire with JaaS (24 h; the videos stay at 8x8, not here).
Transcripts and minutes: `TRANSCRIPT_RETENTION_MONTHS` / `MINUTES_RETENTION_MONTHS`
(calendar months, Transgenia uses 9) or the `*_DAYS` variants (defaults 90 and
365). The effective values are logged at start-up.

## Logs

Only method, path, status, timing and client IP; never query strings, bodies,
webhook payloads, pre-authenticated links, JWTs, transcripts or minutes.
`TestNoSecretsInLogs` enforces this.
