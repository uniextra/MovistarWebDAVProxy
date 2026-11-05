# Colaboradores / Contributors

Agradecemos enormemente a todas las personas que contribuyen a la mejora, estabilidad y compatibilidad de **Movistar / O2 Cloud WebDAV Proxy**.

---

### 🌟 Contribuidores de Código

* **[@bytevictor](https://github.com/bytevictor)**
  - Implementación de `DuplicatiProbeStore`: absorción en memoria volátil de la sonda de privilegios de Duplicati (`duplicati-access-privileges-test.tmp`), resolviendo el error `MED-1000` en Telefónica SAPI ([#13](https://github.com/uniextra/MovistarWebDAVProxy/issues/13), [#14](https://github.com/uniextra/MovistarWebDAVProxy/pull/14)).

* **[@aarizaga90](https://github.com/aarizaga90)**
  - Reporte responsable y resolución de la vulnerabilidad de seguridad en NTLMv2 ([GHSA-fpxr-64gm-wxvf](https://github.com/uniextra/MovistarWebDAVProxy/security/advisories/GHSA-fpxr-64gm-wxvf)).
  - Optimizaciones en `moveByReupload`: re-subida directa desde memoria RAM sin descarga de red, espera activa de confirmación de URL SAPI (`waitForDownloadURL`) y consistencia inmediata en caché ([#12](https://github.com/uniextra/MovistarWebDAVProxy/pull/12)).

---

### 🔍 Diagnóstico y Reportes de la Comunidad

* **[@taimadoCE](https://github.com/taimadoCE)**
  - Diagnóstico detallado del comportamiento de `libsoup`/GVFS en escritorios Linux frente a NTLM tras proxies inversos ([#15](https://github.com/uniextra/MovistarWebDAVProxy/issues/15)), permitiendo el diseño del desafío NTLM adaptativo en v2.2.3, y reporte de subidas de gran tamaño ([#17](https://github.com/uniextra/MovistarWebDAVProxy/issues/17)).
