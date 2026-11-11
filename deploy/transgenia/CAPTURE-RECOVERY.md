# Recuperación de capturas fallidas y evidencia de pausas

Este cambio parte del esquema 9 y añade el esquema 10. No cambia credenciales,
modelos, SMTP, retención, propietarios ni la configuración de Odoo.

## Comportamiento

- **Pista sin voz.** Cuando el proveedor responde correctamente pero sin texto,
  la pista queda `transcribed` con cero segmentos y se registran el
  identificador de solicitud y el uso. Antes se clasificaba como resultado
  incierto y detenía toda la captura. Lo genuinamente incierto (conexión
  fallida, tiempo agotado, respuesta no reconocida) sigue cerrando en falla.
- **Causa registrada.** Cada transcripción fallida escribe un `WARN`
  (`capture: stream transcription failed`) con la causa acotada, el intento y
  el identificador de solicitud, y los conserva en `ai_usage.error_kind` y
  `request_id`. Nunca se registra texto del proveedor ni de la reunión.
- **Reintentar pistas fallidas** (en «Audio y cobertura»). Vuelve a enviar a
  transcripción las pistas fallidas cuyo audio se conserva. Es una decisión
  explícita del anfitrión: borra el marcador de la solicitud anterior, cuyo
  resultado se desconoce y pudo haberse cobrado. El contador de intentos sigue
  avanzando, así que el registro de consumo anterior no se sobrescribe. Las
  pistas ya transcritas no se repiten.
- **Continuar sin las pistas fallidas.** Marca esas pistas como excluidas
  (`audio_streams.excluded_at`) y consolida el resto. La transcripción declara
  `capture_complete: false`, `excluded_streams` y `excluded_audio_ms`; no usa
  `quality: "partial"`, que la verificación previa rechaza. La minuta lleva un
  aviso visible en el panel, el Markdown y el correo. El audio y el error de la
  pista se conservan. No se puede continuar si no queda ninguna pista
  transcrita, y la exclusión no se deshace.
- **Pausa por micrófono silenciado.** La captura sigue al micrófono de Jitsi:
  mientras está silenciado no se sube audio. Ahora el navegador lo reporta
  (`paused`, `resumed`) y «Audio y cobertura» muestra cuánto tiempo de la
  conexión estuvo en pausa. Es una observación del navegador; no indica quién
  silenció. En la reunión, el aviso se repite a partir del primer minuto y
  pasa a estilo de alerta cuando el micrófono es compartido.
- **Reintento de minuta.** Si la minuta falló en la verificación local de la
  transcripción, la página lo explica y no ofrece el reintento: no puede
  pasar con la misma transcripción y nunca llama a la IA.

## Decisión de producto pendiente

Con «micrófono compartido», un silencio en Jitsi deja a toda la sala sin
captura. Este cambio no altera esa regla; sólo la hace visible. Opciones:

1. Mantenerla (lo que hace este cambio): silenciar en Jitsi significa no ser
   grabado, sin excepciones.
2. Con micrófono compartido, seguir capturando aunque Jitsi esté silenciado y
   mostrar un aviso permanente. Evita perder reuniones presenciales, pero
   graba a quien creyó haberse silenciado; exige cambiar el aviso de
   privacidad y el consentimiento de entrada.
3. Con micrófono compartido, pedir confirmación al silenciar («¿pausar también
   la captura de la sala?»).

## Validación antes de instalar

```bash
go test -count=1 ./...
go vet ./...
node --test tests/capture-options.test.cjs
docker build --target test .
```

Las pruebas usan audio, identidades y proveedor simulados.

## Instalación

Seguir el procedimiento de `MINUTES-RECOVERY.md` (imagen del commit revisado,
snapshot consistente, ensayo de la migración en una copia, cambio único de
`IMAGE_TAG`). El ensayo debe terminar en esquema 10 con las columnas nuevas
vacías y todos los registros anteriores intactos.

Las capturas detenidas se conservan durante la instalación: no hace falta
«confirmar que no hay fallos» para ellas, sólo que no haya trabajo en curso.

## Reversión

La migración sólo añade columnas con valor nulo o cero. La imagen anterior
arranca con el esquema 10 sin aplicar nada y sin leer esas columnas. Una
pestaña abierta con la página nueva recibirá un rechazo al reportar una pausa
y mostrará que no pudo actualizar el estado; la captura continúa. Las pistas
excluidas y las minutas generadas con la versión nueva se conservan.

## Recuperar una captura detenida en la versión anterior

Después del despliegue, abrir «Audio y cobertura» de la captura con estado
«Revisión requerida»:

1. **Reintentar pistas fallidas.** Si la causa era una pista en silencio,
   queda transcrita sin voz y la captura se consolida sola.
2. Si la pista vuelve a fallar, el panel muestra la causa. Decidir entonces
   **Continuar sin las pistas fallidas**.
3. La minuta sigue el flujo habitual: aviso al anfitrión, revisión y
   aprobación separada del envío a participantes.

Una minuta que falló por «contenido insuficiente» no se recupera reintentando:
la captura no tiene audio suficiente.
