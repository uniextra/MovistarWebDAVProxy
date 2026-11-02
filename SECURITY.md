# Política de Seguridad

## Versiones Soportadas

Se aplican actualizaciones de seguridad a la última versión menor publicada:

| Versión | Soportada |
| ------- | --------- |
| 2.1.x   | ✅ Sí     |
| < 2.1.0 | ❌ No     |

## Reporte de Vulnerabilidades

Si descubres una vulnerabilidad de seguridad en **Movistar / O2 Cloud WebDAV Proxy**, por favor **no abras un issue público**.

1. Utiliza la pestaña [Security Advisories](https://github.com/uniextra/MovistarWebDAVProxy/security/advisories/new) del repositorio en GitHub para enviar un reporte privado.
2. Incluye una descripción detallada del problema, pasos para reproducirlo y el impacto potencial.
3. Recibirás acuse de recibo en un plazo máximo de 72 horas y coordinaremos la publicación del parche antes de divulgar los detalles.

## Recomendaciones de Despliegue Seguro

- Define siempre credenciales robustas mediante `WEBDAV_USER` y `WEBDAV_PASSWORD` (y opcionalmente `WEBUI_USER` / `WEBUI_PASSWORD`).
- No expongas directamente los puertos `5000` ni `8080` a Internet sin un proxy inverso con TLS (HTTPS) como Caddy, Nginx o Traefik.
- En despliegues locales, limita la exposición a `127.0.0.1` (`BIND_IP=127.0.0.1`) si los clientes residen en el mismo host.
