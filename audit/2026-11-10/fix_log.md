# Capturas fallidas y pausas sin evidencia

- Fecha: 2026-11-10.
- Origen: diagnóstico de dos capturas de producción que no produjeron minuta.
  Una tenía una sola pista fallida entre muchas transcritas; la otra recibió
  unos segundos de audio porque la captura estuvo en pausa casi toda la
  reunión. El diagnóstico usó sólo metadatos, sin leer transcripciones.
- Base: transgenia/production en 2e98b7a2e6bab3bc9ac748684adb3b271130a99d.
- Rama: fix/capture-failed-tracks.
- Cambios: ver `CHANGELOG.md` y `deploy/transgenia/CAPTURE-RECOVERY.md`.
- Decisión que no se tomó: si el micrófono compartido debe seguir capturando
  con Jitsi silenciado. Queda planteada con opciones en el runbook.

| Comprobación | Resultado |
|---|---|
| `gofmt -s -l .` | Sin diferencias |
| `go vet ./...` | Sin hallazgos |
| `go test -count=1 ./...` | PASÓ, imagen `golang:1.26-bookworm` del digest fijado en el Dockerfile |
| `node tests/capture-options.test.cjs` | PASÓ, 19 de 19 (Node 16 local; la CI usa Node 22) |
| Esquema de producción | Lectura de `PRAGMA user_version`: 9, sin columnas en conflicto con el esquema 10 |

- No verificado: micrófono, Jitsi y proveedor reales; `docker build --target
  test`; el ensayo de la migración sobre una copia de la base productiva.
- No hubo despliegue, cambios en la base productiva, llamadas pagadas ni envíos.
- WebMCP: ninguna tool pública creada, modificada o retirada.
- Reversión: cerrar el PR sin integrarlo. Tras un despliegue, ver «Reversión»
  en el runbook.
