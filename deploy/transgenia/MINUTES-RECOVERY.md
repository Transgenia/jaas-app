# Recuperación de minutas y cobertura de captura

Este cambio parte del esquema 8 y añade el esquema 9. No cambia credenciales,
modelos, SMTP, retención, propietarios ni la configuración de Odoo.

## Comportamiento

- Una respuesta válida en formato pero con citas no verificables se conserva
  en `minutes.result_json`, con estado `failed`. El panel muestra el borrador,
  las citas cuestionadas y un enlace a la transcripción. No se publica, descarga,
  envía ni sincroniza ese borrador.
- El propietario puede quitar los elementos señalados después de revisarlos.
  La operación vuelve a validar la evidencia, comprueba que el borrador y la
  transcripción no cambiaron y no llama a IA. Sólo entonces pasa a `ready`;
  se conserva el aviso de omisiones en el panel, Markdown y correo. La aprobación
  de correo a participantes sigue siendo un paso separado.
- No se permite regenerar un borrador conservado. Los reintentos de fallos sin
  borrador mantienen el contador acumulado para no sobrescribir `ai_usage`.
  No es posible reconstruir los intentos sobrescritos por la versión anterior.
- El navegador registra su elección por conexión: captura activada, desactivada
  o no compatible. Los reportes de permiso, dispositivo y subida son observaciones
  del navegador, no pruebas de intención ni identidad física. Un cliente que
  no reporta sigue siendo desconocido. No se rellena retrospectivamente la historia.
- La ausencia de audio bloquea el procesamiento antes de consumir IA. El panel
  de audio y la minuta muestran la cobertura disponible. Varias pistas pueden
  pertenecer a una sola conexión y no garantizan cobertura completa de la reunión.

## Validación antes de instalar

En un checkout limpio del commit revisado:

```bash
go test -count=1 ./...
go vet ./...
go build .
node --test tests/capture-options.test.cjs
git diff --check
docker build --target test .
```

Las pruebas usan transcripciones, identidades, audio y servicios simulados.
No se necesita otra reunión real ni enviar un correo de prueba.

## Instalación por el operador root

El agente que preparó este parche no tiene acceso al socket Docker ni a la base
productiva. Estos pasos no han sido ejecutados en producción. Usar una ventana
sin reuniones, cargas de audio, aprobaciones ni trabajos pendientes.

1. Construir la imagen del **commit revisado** en un checkout limpio; conservar
   el SHA completo como evidencia y usarlo como etiqueta de versión. Ejecutar el
   target `test` antes del target de release del Dockerfile actual.

   ```bash
   TGN_RECOVERY_SHA=$(git rev-parse HEAD)
   TGN_RECOVERY_TAG=minutes-recovery-${TGN_RECOVERY_SHA:0:12}
   docker build --target test .
   docker build --build-arg VERSION="$TGN_RECOVERY_SHA" \
     -t "transgenia-meetings:$TGN_RECOVERY_TAG" .
   ```

2. Recuperar la lista **ordenada** de Compose del contenedor vigente, incluidos
   todos los overlays de SMTP, Odoo y el mapa de propietarios. No utilizar sólo
   `compose.yaml`. Este comando consulta únicamente rutas y no expone secretos:

   ```bash
   docker inspect --format '{{index .Config.Labels "com.docker.compose.project.config_files"}}' tgn-meetings
   ```

   Construir una invocación de `docker compose` con `--project-directory
   /opt/transgenia-meetings`, `--env-file /opt/transgenia-meetings/.env`, el mismo
   nombre de proyecto y un `-f` por cada ruta devuelta, en el mismo orden.
   Guardar privadamente la imagen actual, `.env` y las rutas para recuperación.

3. Confirmar que no hay pistas grabándose, capturas `transcribing`, minutas
   `queued/processing`, correos `queued` ni outbox Odoo `pending/processing`.
   Los fallos existentes que se quieren recuperar se conservan. Detener sólo
   el servicio `meetings` con la invocación Compose anterior.

4. Crear un snapshot consistente de SQLite en un expediente nuevo root/0700,
   mediante la API `sqlite3.Connection.backup` o el procedimiento operativo de
   backup ya instalado. Conservarlo 0600 y comprobar `PRAGMA integrity_check`.
   No copiar sólo el archivo principal de una SQLite viva ignorando su WAL.

5. Ensayar la migración en **otra copia** del snapshot, en un directorio privado
   montado como `/work`. El comando `backup` de la nueva imagen ejecuta las
   migraciones antes de producir la copia de salida; no inicia workers:

   ```bash
   docker run --rm --network none --read-only --user 0:0 \
     --mount "type=bind,source=$TGN_RECOVERY_REHEARSAL,target=/work" \
     -e DB_PATH=/work/source.db "transgenia-meetings:$TGN_RECOVERY_TAG" \
     backup /work/migrated.db
   ```

   `TGN_RECOVERY_REHEARSAL` debe ser el directorio privado del ensayo;
   `source.db` es la copia desechable, nunca la base productiva. Verificar
   integridad, esquema 9, tabla `capture_participation` vacía y conservación de
   todos los registros y columnas anteriores, incluyendo outbox y binding Odoo.

6. Cambiar **únicamente** `IMAGE_TAG` en `/opt/transgenia-meetings/.env`, conservando
   el resto de líneas y sus permisos. No sustituir `meetings.env`, Compose,
   overlays ni secretos por ejemplos del repositorio. Arrancar sólo `meetings`
   con la misma lista de Compose y verificar imagen, versión, `healthy`, esquema 9
   y conservación de los montajes/configuración existentes.

Si una comprobación previa al arranque falla, no continuar con la migración real.
Antes de cambiar imagen/base puede reiniciarse el contenedor anterior. Después
de arrancar la nueva imagen, conservar la base y los eventos actuales para
corregir hacia adelante; no restaurar un snapshot antiguo sobre trabajo nuevo
ni volver a una versión que desconozca el flujo de revisión.

## Recuperación de una minuta fallida en la versión anterior

Los borradores que la versión anterior descartó no se pueden recuperar del
registro de consumo. Si la minuta sigue fallida y su transcripción sigue guardada,
realizar **un reintento desde el panel después del despliegue**, no antes. Usa la
transcripción existente y genera consumo de una nueva respuesta de minuta, no
otra transcripción del audio. Puede quedar lista o quedar conservada para la
nueva revisión, sin un ciclo de regeneraciones.

Una sesión antigua mostrará que no hay elección de captura registrada; no se
puede determinar retrospectivamente si alguien entró con captura desactivada.
Revisar el borrador recuperado antes de aprobar cualquier envío a participantes.
