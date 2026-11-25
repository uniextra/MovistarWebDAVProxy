(function () {
  "use strict";

  var currentOutTab = "compose";
  var $ = function (id) { return document.getElementById(id); };

  function escapeSingleQuoteSh(val) {
    return "'" + String(val).replace(/'/g, "'\\''") + "'";
  }

  function cleanSingleLine(val) {
    return String(val || "").replace(/[\r\n]+/g, " ").trim();
  }

  function parsePort(el, fallback) {
    if (el && el.validity && el.validity.badInput) {
      return { value: fallback, valid: false, raw: "", badInput: true };
    }
    var cleaned = cleanSingleLine(el ? el.value : "");
    if (!cleaned) {
      return { value: fallback, valid: true, raw: "", badInput: false };
    }
    if (/^\d+$/.test(cleaned)) {
      var n = parseInt(cleaned, 10);
      if (n >= 1 && n <= 65535) {
        return { value: String(n), valid: true, raw: cleaned, badInput: false };
      }
    }
    return { value: fallback, valid: false, raw: cleaned, badInput: false };
  }

  function getRadio(name) {
    var el = document.querySelector('input[name="' + name + '"]:checked');
    return el ? el.value : "";
  }

  function buildKvHtml(items) {
    return items.map(function (it) {
      return '<div class="kv-item"><span class="kv-k">' + it.k + '</span><span class="kv-v">' + escapeHtml(it.v) + "</span></div>";
    }).join("");
  }

  function escapeHtml(str) {
    return String(str)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  function updateRadioCardStyles() {
    document.querySelectorAll("[data-radio-group]").forEach(function (group) {
      group.querySelectorAll(".choice-card").forEach(function (card) {
        var input = card.querySelector('input[type="radio"]');
        card.classList.toggle("selected", Boolean(input && input.checked));
      });
    });
  }

  function collectState() {
    var mode = getRadio("mode") || "native";
    var channel = getRadio("channel") || "latest";
    var hardware = getRadio("hardware") || "standard";
    var customTag = cleanSingleLine($("customTag").value) || "v2.7.0";

    var image = "uniextra/movistar-webdav-proxy:latest";
    if (mode === "web") {
      image = "movistar-webdav-proxy:web";
    } else if (channel === "dev") {
      image = "uniextra/movistar-webdav-proxy:dev";
    } else if (channel === "tag") {
      image = "uniextra/movistar-webdav-proxy:" + customTag;
    }

    var galleryEnabled = $("galleryEnabled").checked;
    var galleryDir = cleanSingleLine($("galleryDir").value) || "_gallery";
    var galleryLayout = $("galleryLayout").value || "year-month";
    var galleryUpload = galleryEnabled && $("galleryUpload").checked;
    var galleryDelete = galleryEnabled && $("galleryDelete").checked;

    var telegramEnabled = $("telegramEnabled").checked;
    var tgToken = cleanSingleLine($("tgToken").value);
    var tgChat = cleanSingleLine($("tgChat").value);

    var encryptEnabled = $("encryptEnabled").checked;
    var encryptKey = cleanSingleLine($("encryptKey").value);

    var proxyEnabled = $("proxyEnabled").checked;
    var webBindIp = proxyEnabled ? ($("webBindIp").value || "127.0.0.1") : "0.0.0.0";
    var webdavBindIp = proxyEnabled ? ($("webdavBindIp").value || "0.0.0.0") : "0.0.0.0";
    var trustedProxies = proxyEnabled ? cleanSingleLine($("trustedProxies").value) : "";

    var webPortInfo = parsePort($("webPort"), "5000");
    var webdavPortInfo = parsePort($("webdavPort"), "5050");
    var webdavHostPortInfo = proxyEnabled ? parsePort($("webdavHostPort"), "") : { value: "", valid: true, raw: "", badInput: false };

    var invalidPorts = [];
    if (!webPortInfo.valid) {
      invalidPorts.push({ id: "webPort", label: "Puerto Panel Web", raw: webPortInfo.raw, badInput: webPortInfo.badInput, fallback: webPortInfo.value });
    }
    if (!webdavPortInfo.valid) {
      invalidPorts.push({ id: "webdavPort", label: "Puerto WebDAV y Explorador", raw: webdavPortInfo.raw, badInput: webdavPortInfo.badInput, fallback: webdavPortInfo.value });
    }
    if (!webdavHostPortInfo.valid) {
      invalidPorts.push({ id: "webdavHostPort", label: "Puerto externo tras el proxy", raw: webdavHostPortInfo.raw, badInput: webdavHostPortInfo.badInput, fallback: webdavPortInfo.value });
    }

    var webPort = webPortInfo.value;
    var webdavPort = webdavPortInfo.value;
    var webdavHostPortRaw = webdavHostPortInfo.value;
    var webdavHostPort = webdavHostPortRaw || webdavPort;

    var authDisabled = $("authDisabled").checked;
    var webdavUser = cleanSingleLine($("webdavUser").value);
    var webdavPass = cleanSingleLine($("webdavPass").value);
    var separateWebui = $("separateWebui").checked;
    var webuiUser = separateWebui ? cleanSingleLine($("webuiUser").value) : "";
    var webuiPass = separateWebui ? cleanSingleLine($("webuiPass").value) : "";

    var tz = cleanSingleLine($("tzInput").value) || "Europe/Madrid";
    var tokensType = $("tokensType").value || "volume";
    var tokensPath = cleanSingleLine($("tokensPath").value) || (tokensType === "volume" ? "movistar_tokens" : "./tokens");

    var tmpdirEnabled = $("tmpdirEnabled").checked;
    var tmpdirHostPath = cleanSingleLine($("tmpdirHostPath").value) || "./tmp";

    var containerName = cleanSingleLine($("containerName").value) || (mode === "web" ? "movistar-webdav-proxy-web" : "movistar-webdav-proxy");
    var restartPolicy = $("restartPolicy").value || "unless-stopped";

    return {
      mode: mode,
      channel: channel,
      hardware: hardware,
      image: image,
      galleryEnabled: galleryEnabled,
      galleryDir: galleryDir,
      galleryLayout: galleryLayout,
      galleryUpload: galleryUpload,
      galleryDelete: galleryDelete,
      telegramEnabled: telegramEnabled,
      tgToken: tgToken,
      tgChat: tgChat,
      encryptEnabled: encryptEnabled,
      encryptKey: encryptKey,
      proxyEnabled: proxyEnabled,
      webBindIp: webBindIp,
      webdavBindIp: webdavBindIp,
      trustedProxies: trustedProxies,
      webPort: webPort,
      webdavPort: webdavPort,
      webdavHostPort: webdavHostPort,
      webdavHostPortCustom: webdavHostPortRaw,
      invalidPorts: invalidPorts,
      authDisabled: authDisabled,
      webdavUser: webdavUser,
      webdavPass: webdavPass,
      separateWebui: separateWebui,
      webuiUser: webuiUser,
      webuiPass: webuiPass,
      tz: tz,
      tokensType: tokensType,
      tokensPath: tokensPath,
      tmpdirEnabled: tmpdirEnabled,
      tmpdirHostPath: tmpdirHostPath,
      containerName: containerName,
      restartPolicy: restartPolicy,
      maxDownloads: cleanSingleLine($("maxDownloads").value),
      maxUploads: cleanSingleLine($("maxUploads").value),
      bufferMb: cleanSingleLine($("bufferMb").value),
      bwLimit: cleanSingleLine($("bwLimit").value),
      syncInterval: cleanSingleLine($("syncInterval").value),
      smallCacheMb: cleanSingleLine($("smallCacheMb").value),
      maxUploadMb: cleanSingleLine($("maxUploadMb").value),
      caseInsensitive: $("caseInsensitive").value,
      ntlmMode: $("ntlmMode").value,
      sapiProfile: $("sapiProfile").value,
      logLevel: $("logLevel").value,
      bruteForceExcluded: cleanSingleLine($("bruteForceExcluded").value),
      webRenewBefore: cleanSingleLine($("webRenewBefore").value),
      webTrashEvery: cleanSingleLine($("webTrashEvery").value),
      chromeNoSandbox: $("chromeNoSandbox").value,
      useEnvRefs: $("useEnvRefs").checked
    };
  }

  function buildEnvList(s, forComposeRefs) {
    var env = [];
    var secretVal = function (key, raw, placeholder) {
      if (forComposeRefs) {
        return "${" + key + ":-" + (raw || placeholder || "") + "}";
      }
      return raw || placeholder || "";
    };

    env.push(["TZ", s.tz]);

    if (s.authDisabled) {
      env.push(["AUTH_DISABLED", "true"]);
    } else {
      env.push(["WEBDAV_USER", secretVal("WEBDAV_USER", s.webdavUser, "admin")]);
      env.push(["WEBDAV_PASSWORD", secretVal("WEBDAV_PASSWORD", s.webdavPass, "CambiaEstaClaveSegura")]);
      if (s.separateWebui) {
        env.push(["WEBUI_USER", secretVal("WEBUI_USER", s.webuiUser, "panel")]);
        env.push(["WEBUI_PASSWORD", secretVal("WEBUI_PASSWORD", s.webuiPass, "OtraClaveSegura")]);
      }
    }

    if (s.webdavHostPort !== "5050" || s.webdavHostPortCustom) {
      env.push(["WEBDAV_HOST_PORT", s.webdavHostPort]);
    }

    if (s.galleryEnabled) {
      env.push(["GALLERY_ENABLED", "true"]);
      env.push(["GALLERY_LAYOUT", s.galleryLayout]);
      if (s.galleryDir !== "_gallery") {
        env.push(["GALLERY_DIR", s.galleryDir]);
      }
      if (s.galleryUpload) env.push(["GALLERY_UPLOAD", "true"]);
      if (s.galleryDelete) env.push(["GALLERY_DELETE", "true"]);
    }

    if (s.telegramEnabled) {
      env.push(["TELEGRAM_BOT_TOKEN", secretVal("TELEGRAM_BOT_TOKEN", s.tgToken, "")]);
      env.push(["TELEGRAM_CHAT_ID", secretVal("TELEGRAM_CHAT_ID", s.tgChat, "")]);
    }

    if (s.encryptEnabled) {
      env.push(["TOKENS_ENCRYPTION_KEY", secretVal("TOKENS_ENCRYPTION_KEY", s.encryptKey, "TuClaveMaestraDeCifrado")]);
    }

    if (s.proxyEnabled && s.trustedProxies) {
      env.push(["TRUSTED_PROXIES", s.trustedProxies]);
    }

    if (s.hardware === "lowram") {
      env.push(["WEBDAV_UPLOAD_SPOOL", "false"]);
      if (!s.bufferMb) env.push(["WEBDAV_BUFFER_MB", "16"]);
      if (!s.smallCacheMb) env.push(["WEBDAV_SMALLFILE_CACHE_MB", "16"]);
      env.push(["TLS_PREFER_CHACHA", "true"]);
    }

    if (s.tmpdirEnabled) {
      env.push(["WEBDAV_TMPDIR", "/app/tmp"]);
    }

    if (s.maxDownloads) env.push(["WEBDAV_MAX_DOWNLOADS", s.maxDownloads]);
    if (s.maxUploads) env.push(["WEBDAV_MAX_UPLOADS", s.maxUploads]);
    if (s.bufferMb) env.push(["WEBDAV_BUFFER_MB", s.bufferMb]);
    if (s.bwLimit) env.push(["WEBDAV_UPLOAD_BWLIMIT", s.bwLimit]);
    if (s.syncInterval) env.push(["SYNC_INTERVAL_SEC", s.syncInterval]);
    if (s.smallCacheMb) env.push(["WEBDAV_SMALLFILE_CACHE_MB", s.smallCacheMb]);
    if (s.maxUploadMb) env.push(["MAX_UPLOAD_MB", s.maxUploadMb]);
    if (s.caseInsensitive) env.push(["WEBDAV_CASE_INSENSITIVE", s.caseInsensitive]);
    if (s.ntlmMode) env.push(["WEBDAV_NTLM", s.ntlmMode]);
    if (s.sapiProfile) env.push(["SAPI_CLIENT_PROFILE", s.sapiProfile]);
    if (s.logLevel) env.push(["LOG_LEVEL", s.logLevel]);
    if (s.bruteForceExcluded) env.push(["BRUTE_FORCE_EXCLUDED_IPS", s.bruteForceExcluded]);

    if (s.mode === "web") {
      if (s.webRenewBefore) env.push(["WEB_SESSION_RENEW_BEFORE", s.webRenewBefore]);
      if (s.webTrashEvery) env.push(["WEB_TRASH_EMPTY_EVERY", s.webTrashEvery]);
      if (s.chromeNoSandbox) env.push(["CHROME_NO_SANDBOX", s.chromeNoSandbox]);
    }

    return env;
  }

  function generateCompose(s) {
    var svcName = s.mode === "web" ? "movistar-webdav-proxy-web" : "movistar-webdav-proxy";
    var lines = ["services:", "  " + svcName + ":"];

    if (s.mode === "web") {
      lines.push("    build:");
      lines.push("      context: .");
      lines.push("      dockerfile: Dockerfile.web");
    }
    lines.push("    image: " + s.image);
    lines.push("    container_name: " + s.containerName);
    lines.push("    restart: " + s.restartPolicy);
    lines.push("    read_only: true");
    lines.push("    tmpfs:");
    lines.push(s.mode === "web" ? "      - /tmp:size=256m" : "      - /tmp");
    if (s.mode === "web") {
      lines.push("    shm_size: 256m");
    }
    lines.push("    security_opt:");
    lines.push("      - no-new-privileges:true");

    var envList = buildEnvList(s, s.useEnvRefs);
    lines.push("    environment:");
    envList.forEach(function (pair) {
      lines.push("      - " + pair[0] + "=" + pair[1]);
    });

    lines.push("    ports:");
    var webHostPrefix = s.webBindIp !== "0.0.0.0" ? (s.webBindIp + ":") : "";
    var davHostPrefix = s.webdavBindIp !== "0.0.0.0" ? (s.webdavBindIp + ":") : "";
    lines.push('      - "' + webHostPrefix + s.webPort + ':5000"   # Panel Web');
    lines.push('      - "' + davHostPrefix + s.webdavPort + ':8080"   # Endpoint WebDAV y Explorador Web');

    lines.push("    volumes:");
    lines.push("      - " + s.tokensPath + ":/app/tokens");
    if (s.tmpdirEnabled) {
      lines.push("      - " + s.tmpdirHostPath + ":/app/tmp");
    }

    if (s.tokensType === "volume") {
      lines.push("");
      lines.push("volumes:");
      lines.push("  " + s.tokensPath + ":");
      lines.push("    name: " + s.tokensPath);
    }

    return lines.join("\n");
  }

  function generateDockerRun(s) {
    var parts = [];
    if (s.mode === "web") {
      parts.push("# Modo Web requiere compilar Dockerfile.web desde el repositorio:");
      parts.push("docker build -f Dockerfile.web -t movistar-webdav-proxy:web .");
      parts.push("");
    } else {
      parts.push("docker pull " + s.image);
      parts.push("");
    }

    var cmd = [
      "docker run -d \\",
      "  --name " + escapeSingleQuoteSh(s.containerName) + " \\",
      "  --restart " + s.restartPolicy + " \\",
      "  --read-only \\",
      "  --security-opt no-new-privileges:true \\",
      (s.mode === "web" ? "  --tmpfs /tmp:size=256m \\\n  --shm-size 256m \\" : "  --tmpfs /tmp \\")
    ];

    buildEnvList(s, false).forEach(function (pair) {
      cmd.push("  -e " + escapeSingleQuoteSh(pair[0] + "=" + pair[1]) + " \\");
    });

    var webHostPrefix = s.webBindIp !== "0.0.0.0" ? (s.webBindIp + ":") : "";
    var davHostPrefix = s.webdavBindIp !== "0.0.0.0" ? (s.webdavBindIp + ":") : "";
    cmd.push("  -p " + webHostPrefix + s.webPort + ":5000 \\");
    cmd.push("  -p " + davHostPrefix + s.webdavPort + ":8080 \\");
    cmd.push("  -v " + escapeSingleQuoteSh(s.tokensPath + ":/app/tokens") + " \\");
    if (s.tmpdirEnabled) {
      cmd.push("  -v " + escapeSingleQuoteSh(s.tmpdirHostPath + ":/app/tmp") + " \\");
    }
    cmd.push("  " + s.image);

    return parts.concat(cmd).join("\n");
  }

  function generateEnvFile(s) {
    var lines = [
      "# Variables para usar junto con docker-compose.yml",
      "# (Guarda este archivo como .env en la misma carpeta)",
      ""
    ];
    buildEnvList(s, false).forEach(function (pair) {
      lines.push(pair[0] + "=" + pair[1]);
    });
    return lines.join("\n");
  }

  function renderDynamicNotices(s) {
    var items = [];
    if (s.invalidPorts && s.invalidPorts.length) {
      s.invalidPorts.forEach(function (p) {
        var detail = p.badInput
          ? "el valor introducido no es un puerto entero válido (<code>1–65535</code>)"
          : "el puerto <code>" + escapeHtml(p.raw) + "</code> debe ser un entero entre <code>1</code> y <code>65535</code>";
        items.push('<div class="notice notice-warn">⚠️ <strong>' + escapeHtml(p.label) + ':</strong> ' + detail + '; se ha usado <code>' + escapeHtml(p.fallback) + '</code> en la configuración generada.</div>');
      });
    }
    if (s.authDisabled) {
      items.push('<div class="notice notice-warn">⚠️ <strong>AUTH_DISABLED=true:</strong> cualquier equipo que alcance el puerto <code>' + escapeHtml(s.webPort) + '</code> o <code>' + escapeHtml(s.webdavPort) + '</code> entrará sin contraseña. Úsalo solo en localhost o detrás de autenticación externa.</div>');
    }
    if (s.galleryEnabled) {
      items.push('<div class="notice notice-warn">🖼️ <strong>Galería virtual activa en <code>/' + escapeHtml(s.galleryDir) + '</code>:</strong> si ejecutas <code>rclone sync</code> contra la raíz <code>/</code>, añade <code>--exclude "/' + escapeHtml(s.galleryDir) + '/**"</code> para que Rclone no intente borrar la carpeta virtual.</div>');
    }
    if (s.hardware === "lowram") {
      items.push('<div class="notice notice-info">🍓 <strong>Perfil Baja RAM:</strong> <code>WEBDAV_UPLOAD_SPOOL=false</code> no guarda copia temporal en disco al subir. Ten en cuenta que Synology HyperBackup requiere <code>WEBDAV_UPLOAD_SPOOL=true</code>.</div>');
    }
    if (s.channel === "dev" && s.mode === "native") {
      items.push('<div class="notice notice-info">🧪 Estás usando el canal <strong><code>:dev</code></strong> (compilación de pruebas). Para producción estable usa <code>:latest</code>.</div>');
    }
    $("dynamicWarnings").innerHTML = items.join("");
  }

  function renderNextSteps(s) {
    var webHost = s.webBindIp === "127.0.0.1" ? "127.0.0.1" : "IP_DE_TU_SERVIDOR";
    var davHost = s.webdavBindIp === "127.0.0.1" ? "127.0.0.1" : "IP_DE_TU_SERVIDOR";
    var startCmd = s.mode === "web" ? "docker compose up -d --build" : "docker compose up -d";
    var steps = [];
    if (currentOutTab === "compose") {
      steps.push("Guarda el bloque en un archivo <code>docker-compose.yml</code>" + (s.useEnvRefs ? " junto con el archivo <code>.env</code>." : "."));
      steps.push("Arranca el contenedor con <code>" + escapeHtml(startCmd) + "</code>.");
    } else if (currentOutTab === "run") {
      steps.push("Ejecuta el comando en tu servidor o NAS (requiere Docker).");
    } else {
      steps.push("Guarda este contenido como <code>.env</code> y marca <em>Referenciar secretos como ${VAR}</em> en Compose.");
    }
    steps.push("Abre el <strong>Panel Web</strong> en <code>http://" + webHost + ":" + escapeHtml(s.webPort) + "</code>" + (s.mode === "web" ? " y pulsa <strong>🔐 Abrir inicio de sesión</strong>." : " y vincula tu cuenta por SMS (OAuth2)."));
    steps.push("Conecta tus clientes WebDAV o abre el <strong>Explorador Web</strong> directamente en <code>http://" + davHost + ":" + escapeHtml(s.webdavHostPort) + "</code>.");
    $("nextStepsList").innerHTML = steps.map(function (st) { return "<li>" + st + "</li>"; }).join("");
  }

  function renderClients(s) {
    var host = cleanSingleLine($("clientHost").value) || "192.168.1.50";
    var remote = cleanSingleLine($("rcloneRemote").value) || "o2cloud";
    var action = $("rcloneAction").value || "sync";
    var useChunker = $("rcloneChunker").checked;
    var useCrypt = $("rcloneCrypt").checked;

    var user = s.authDisabled ? "admin" : (s.webdavUser || "admin");
    var webdavUrl = "http://" + host + ":" + s.webdavHostPort;

    var conf = [
      "[" + remote + "]",
      "type = webdav",
      "url = " + webdavUrl,
      "vendor = owncloud",
      "user = " + user,
      "pass = PEGA_AQUI_EL_RESULTADO_DE_rclone_obscure"
    ];

    var targetRemote = remote;
    if (useChunker) {
      conf.push("");
      conf.push("[" + remote + "-chunker]");
      conf.push("type = chunker");
      conf.push("remote = " + remote + ":");
      conf.push("chunk_size = 2Gi");
      conf.push("hash_type = md5quick");
      targetRemote = remote + "-chunker";
    }
    if (useCrypt) {
      conf.push("");
      conf.push("[" + remote + "-crypt]");
      conf.push("type = crypt");
      conf.push("remote = " + targetRemote + ":BackupsCifrados");
      conf.push("password = PEGA_AQUI_CLAVE_OFUSCADA");
      conf.push("password2 = PEGA_AQUI_SALT_OFUSCADO");
      targetRemote = remote + "-crypt";
    }
    $("rcloneConfCode").textContent = conf.join("\n");

    var excl = (s.galleryEnabled && action === "sync" && !useCrypt)
      ? ' --exclude "/' + s.galleryDir + '/**"'
      : "";
    var cmdText = "";
    if (action === "mount") {
      cmdText = "rclone mount " + targetRemote + ": M: --vfs-cache-mode full";
    } else {
      cmdText = "rclone " + action + " /ruta/local/copia " + targetRemote + ":/MiBackup" + excl + " --transfers 2 --checkers 4 --progress";
    }
    $("rcloneCmdCode").textContent = cmdText;

    var rNotice = "";
    if (s.galleryEnabled && action === "sync" && !useCrypt) {
      rNotice = '<div class="notice notice-warn" style="margin-top:10px;">Se ha añadido automáticamente <code>--exclude "/' + escapeHtml(s.galleryDir) + '/**"</code> porque tienes la galería virtual activada.</div>';
    }
    $("rcloneNotice").innerHTML = rNotice;

    $("synologyKv").innerHTML = buildKvHtml([
      { k: "Protocolo", v: "WebDAV (HTTP)" },
      { k: "Servidor / URL", v: webdavUrl },
      { k: "Puerto", v: s.webdavHostPort },
      { k: "Usuario", v: s.authDisabled ? "(sin auth)" : user },
      { k: "Carpeta HyperBackup", v: "HyperBackup_NAS / Copia.hbk" },
      { k: "Galería virtual", v: s.galleryEnabled ? (webdavUrl + "/" + s.galleryDir) : "Desactivada" }
    ]);

    $("duplicatiKv").innerHTML = buildKvHtml([
      { k: "Tipo de almacenamiento", v: "WebDAV" },
      { k: "Usar SSL", v: "Desactivado (en red local / VPN)" },
      { k: "Servidor y puerto", v: host + " : " + s.webdavHostPort },
      { k: "Ruta en servidor", v: "Duplicati_Server" },
      { k: "Usuario", v: s.authDisabled ? "(sin auth)" : user }
    ]);
  }

  function syncVisibility(s) {
    $("channelGroup").hidden = (s.mode === "web");
    $("webOnlyAdvanced").hidden = (s.mode !== "web");
    $("customTagWrap").hidden = (s.mode === "web" || s.channel !== "tag");
    $("galleryOptions").hidden = !s.galleryEnabled;
    $("telegramOptions").hidden = !s.telegramEnabled;
    $("encryptOptions").hidden = !s.encryptEnabled;
    $("proxyOptions").hidden = !s.proxyEnabled;
    $("webuiCreds").hidden = !s.separateWebui;
    $("tmpdirWrap").hidden = !s.tmpdirEnabled;

    $("webdavUser").disabled = s.authDisabled;
    $("webdavPass").disabled = s.authDisabled;
    $("separateWebui").disabled = s.authDisabled;

    var invalidIds = {};
    (s.invalidPorts || []).forEach(function (p) { invalidIds[p.id] = true; });
    ["webPort", "webdavPort", "webdavHostPort"].forEach(function (id) {
      var el = $(id);
      var isInvalid = Boolean(invalidIds[id]);
      el.classList.toggle("input-invalid", isInvalid);
      el.setAttribute("aria-invalid", isInvalid ? "true" : "false");
    });
  }

  function renderAll() {
    updateRadioCardStyles();
    var s = collectState();
    syncVisibility(s);

    var output = "";
    if (currentOutTab === "compose") {
      output = generateCompose(s);
    } else if (currentOutTab === "run") {
      output = generateDockerRun(s);
    } else {
      output = generateEnvFile(s);
    }
    $("outputCode").textContent = output;
    renderDynamicNotices(s);
    renderNextSteps(s);
    renderClients(s);
  }

  function copyWithFeedback(btn, text) {
    var orig = btn.textContent;
    var done = function () {
      btn.textContent = "✔ Copiado";
      setTimeout(function () { btn.textContent = orig; }, 1600);
    };
    if (navigator.clipboard && navigator.clipboard.writeText) {
      navigator.clipboard.writeText(text).then(done).catch(function () {
        fallbackCopy(text);
        done();
      });
    } else {
      fallbackCopy(text);
      done();
    }
  }

  function fallbackCopy(text) {
    var ta = document.createElement("textarea");
    ta.value = text;
    ta.style.position = "fixed";
    ta.style.opacity = "0";
    document.body.appendChild(ta);
    ta.select();
    try { document.execCommand("copy"); } catch (_) {}
    ta.remove();
  }

  function generateRandomPassword(length) {
    var charset = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789";
    var bytes = new Uint8Array(length);
    window.crypto.getRandomValues(bytes);
    var res = "";
    for (var i = 0; i < bytes.length; i++) {
      res += charset.charAt(bytes[i] % charset.length);
    }
    return res;
  }

  function activateMainTab(tab) {
    document.querySelectorAll(".tab-btn").forEach(function (b) { b.classList.toggle("active", b.dataset.tab === tab); });
    document.querySelectorAll(".tab-panel").forEach(function (p) { p.hidden = (p.id !== "tab-" + tab); });
  }

  function activateOutTab(out) {
    currentOutTab = out || "compose";
    document.querySelectorAll(".sub-tab").forEach(function (b) { b.classList.toggle("active", b.dataset.out === currentOutTab); });
  }

  function applyUrlHash() {
    var hash = "";
    try {
      hash = decodeURIComponent(window.location.hash || "").toLowerCase().trim();
    } catch (_) {
      hash = (window.location.hash || "").toLowerCase().trim();
    }
    if (!hash) return;

    if (hash === "#modo-web" || hash === "#web") {
      activateMainTab("docker");
      var webRadio = document.querySelector('input[type="radio"][name="mode"][value="web"]');
      if (webRadio && !webRadio.checked) {
        webRadio.checked = true;
        var cname = cleanSingleLine($("containerName").value);
        if (!cname || cname === "movistar-webdav-proxy") {
          $("containerName").value = "movistar-webdav-proxy-web";
        }
      }
      renderAll();
    } else if (hash === "#nativo" || hash === "#native") {
      activateMainTab("docker");
      var natRadio = document.querySelector('input[type="radio"][name="mode"][value="native"]');
      if (natRadio && !natRadio.checked) {
        natRadio.checked = true;
        var cname2 = cleanSingleLine($("containerName").value);
        if (!cname2 || cname2 === "movistar-webdav-proxy-web") {
          $("containerName").value = "movistar-webdav-proxy";
        }
      }
      renderAll();
    } else if (hash === "#clientes" || hash === "#clients") {
      activateMainTab("clients");
    } else if (hash === "#guias" || hash === "#guides" || hash === "#docs") {
      activateMainTab("guides");
    } else if (hash === "#docker-run" || hash === "#run") {
      activateMainTab("docker");
      activateOutTab("run");
      renderAll();
    } else if (hash === "#env") {
      activateMainTab("docker");
      activateOutTab("env");
      renderAll();
    }
  }

  // Navegación por pestañas principales
  document.querySelectorAll(".tab-btn").forEach(function (btn) {
    btn.addEventListener("click", function () {
      activateMainTab(btn.dataset.tab);
    });
  });

  // Sub-pestañas de salida Docker
  document.querySelectorAll(".sub-tab").forEach(function (btn) {
    btn.addEventListener("click", function () {
      activateOutTab(btn.dataset.out || "compose");
      renderAll();
    });
  });

  // Ajuste automático tokensPath al cambiar entre volume y bind
  $("tokensType").addEventListener("change", function () {
    var cur = cleanSingleLine($("tokensPath").value);
    if ($("tokensType").value === "bind" && (!cur || cur === "movistar_tokens")) {
      $("tokensPath").value = "./tokens";
    } else if ($("tokensType").value === "volume" && (!cur || cur === "./tokens")) {
      $("tokensPath").value = "movistar_tokens";
    }
    renderAll();
  });

  // Ajuste automático containerName si no fue editado a un nombre distinto
  document.querySelectorAll('input[name="mode"]').forEach(function (radio) {
    radio.addEventListener("change", function () {
      var cname = cleanSingleLine($("containerName").value);
      if (radio.value === "web" && (!cname || cname === "movistar-webdav-proxy")) {
        $("containerName").value = "movistar-webdav-proxy-web";
      } else if (radio.value === "native" && (!cname || cname === "movistar-webdav-proxy-web")) {
        $("containerName").value = "movistar-webdav-proxy";
      }
    });
  });

  // Generadores aleatorios seguros en navegador
  $("genWebdavPassBtn").addEventListener("click", function () {
    $("webdavPass").value = generateRandomPassword(20);
    renderAll();
  });

  $("genWebuiPassBtn").addEventListener("click", function () {
    $("webuiPass").value = generateRandomPassword(20);
    renderAll();
  });

  $("genKeyBtn").addEventListener("click", function () {
    var bytes = new Uint8Array(32);
    window.crypto.getRandomValues(bytes);
    var hex = Array.prototype.map.call(bytes, function (b) {
      return ("0" + b.toString(16)).slice(-2);
    }).join("");
    $("encryptKey").value = hex;
    renderAll();
  });

  $("copyOutBtn").addEventListener("click", function () {
    copyWithFeedback($("copyOutBtn"), $("outputCode").textContent);
  });

  $("dlOutBtn").addEventListener("click", function () {
    var filename = currentOutTab === "compose" ? "docker-compose.yml" : (currentOutTab === "run" ? "docker-run.sh" : ".env");
    var blob = new Blob([$("outputCode").textContent + "\n"], { type: "text/plain;charset=utf-8" });
    var url = URL.createObjectURL(blob);
    var a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
  });

  document.querySelectorAll(".copy-target").forEach(function (btn) {
    btn.addEventListener("click", function () {
      var target = $(btn.dataset.target);
      if (target) copyWithFeedback(btn, target.textContent);
    });
  });

  window.addEventListener("hashchange", applyUrlHash);
  document.addEventListener("input", renderAll);
  document.addEventListener("change", renderAll);

  applyUrlHash();
  renderAll();
})();
