# Colaboradores / Contributors

Agradecemos enormemente a todas las personas que contribuyen a la mejora, estabilidad y compatibilidad de **Movistar / O2 Cloud WebDAV Proxy**.

---

### 🌟 Contribuidores de Código

* **[@bytevictor](https://github.com/bytevictor)**
  - Implementación de `DuplicatiProbeStore`: absorción en memoria volátil de la sonda de privilegios de Duplicati (`duplicati-access-privileges-test.tmp`), resolviendo el error `MED-1000` en Telefónica SAPI ([#13](https://github.com/uniextra/MovistarWebDAVProxy/issues/13), [#14](https://github.com/uniextra/MovistarWebDAVProxy/pull/14)).

* **[@aarizaga90](https://github.com/aarizaga90)**
  - Diagnóstico A/B del bloqueo de CloudFront por User-Agent en O2 y validación de la huella Edge que permitió corregir el login SAPI en v2.4.3 ([#33](https://github.com/uniextra/MovistarWebDAVProxy/issues/33#issuecomment-6863810469)).
  - Diagnóstico en profundidad de la reconexión de Windows WebClient tras reinicio (Microsoft KB 2673544) y diseño de la solución de montaje persistente con DPAPI ([#18](https://github.com/uniextra/MovistarWebDAVProxy/issues/18)).
  - Confirmación estricta del destino por ID, tamaño y SHA-256 antes de eliminar el archivo origen en re-subidas (`moveByReupload`), protección frente a lecturas con cuerpo vacío de SAPI (`getBodyGuard` -> 503) y exclusión mutua ordenada por ruta en operaciones de ficheros ([#20](https://github.com/uniextra/MovistarWebDAVProxy/pull/20)).
  - Reporte responsable y resolución de la vulnerabilidad de seguridad en NTLMv2 ([GHSA-fpxr-64gm-wxvf](https://github.com/uniextra/MovistarWebDAVProxy/security/advisories/GHSA-fpxr-64gm-wxvf)).
  - Optimizaciones en `moveByReupload`: re-subida directa desde memoria RAM sin descarga de red, espera activa de confirmación de URL SAPI (`waitForDownloadURL`) y consistencia inmediata en caché ([#12](https://github.com/uniextra/MovistarWebDAVProxy/pull/12)).

* **[@jesusiriondo-byte](https://github.com/jesusiriondo-byte)**
  - Análisis de tres fallos críticos relacionados con subidas interrumpidas, descargas inmediatamente posteriores a una subida y resolución incorrecta de nombres de archivo; aportó parches y pruebas de regresión que ayudaron a corregirlos ([#4](https://github.com/uniextra/MovistarWebDAVProxy/issues/4)).
  - Identificación de errores de SAPI al alcanzar el límite de carpetas de la cuenta ([#21](https://github.com/uniextra/MovistarWebDAVProxy/issues/21)).

---

### 🧪 Pruebas y Validación

* **[@aitorsua93](https://github.com/aitorsua93)**
  - Aportación de pruebas de regresión para movimientos y renombrados con cambio de extensión; identificó riesgos de integridad de datos y escenarios en los que no debía eliminarse el original sin verificar el destino ([PR #12](https://github.com/uniextra/MovistarWebDAVProxy/pull/12)).

---

### 🔍 Diagnóstico y Reportes de la Comunidad

* **[@taimadoCE](https://github.com/taimadoCE)**
  - Diagnóstico detallado del comportamiento de `libsoup`/GVFS en escritorios Linux frente a NTLM tras proxies inversos ([#15](https://github.com/uniextra/MovistarWebDAVProxy/issues/15)), permitiendo el diseño del desafío NTLM adaptativo en v2.2.3, y reporte de subidas de gran tamaño ([#17](https://github.com/uniextra/MovistarWebDAVProxy/issues/17)).

* **[@miguelismos](https://github.com/miguelismos)**
  - Análisis de las llamadas SAPI que contribuyen a los bloqueos WAF, mediciones comparativas de rendimiento y propuesta de optimizaciones WebDAV acompañadas de pruebas ([#27](https://github.com/uniextra/MovistarWebDAVProxy/issues/27), [#28](https://github.com/uniextra/MovistarWebDAVProxy/issues/28)).

