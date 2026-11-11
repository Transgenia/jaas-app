# Despliegue en un solo comando

- Fecha: 2026-11-10.
- Motivo: cada versión se instaló a mano desde un checkout distinto y no había
  un lugar único para el código ni un procedimiento ejecutable. El operador no
  podía ubicar dónde vive cada pieza ni cómo se despliega.
- Base: transgenia/production en 7b54fa3415beb2a3a0710f9faf035eb63c233ca1.
- Rama: chore/deploy-unify.
- Cambios: `deploy/transgenia/scripts/deploy.sh`, su banco de pruebas
  `deploy.test.sh` y el mapa de `deploy/transgenia/README.md`. No cambia el
  binario, el esquema, Compose, nginx ni los timers.

| Comprobación | Resultado |
|---|---|
| `bash -n` de ambos scripts | Sin errores |
| `bash deploy/transgenia/scripts/deploy.test.sh` | PASÓ, 18 de 18, con `docker` simulado y un remoto git local |
| `deploy.sh status` contra producción | Coincide con el estado observado por separado |
| `deploy.sh deploy --dry-run` y `rollback --dry-run` contra producción | Muestran los pasos; `.env` idéntico y contenedor sin reinicio antes y después |

- No verificado: un despliegue real con el script (el camino real sólo se
  ejercitó con `docker` simulado); `shellcheck` (no está instalado en el host).
- No se instaló el script en el servidor, no se creó el checkout `src/` y no se
  cambió la versión en ejecución.
- WebMCP: ninguna tool pública creada, modificada o retirada.
- Reversión: cerrar el PR. Si el script ya se instaló, borrar
  `bin/deploy.sh`; el servicio no depende de él.
