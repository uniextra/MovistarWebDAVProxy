package webdav

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"movistar-webdav-proxy/internal/config"
	"movistar-webdav-proxy/internal/sapi"
)

type browserBreadcrumb struct {
	Name string
	Href string
}

type browserRow struct {
	IconID    string
	Kind      string
	TypeLabel string
	Href      string
	Name      string
	Size      string
	SizeBytes int64
	Date      string
	ModUnix   int64
	IsDir     bool
	CanRename bool
	CanDelete bool
}

type browserHeadData struct {
	HeadTitle    string
	UseLocalFont bool
	BootMessage  string
}

type browserPageData struct {
	Nonce       string
	Title       string
	Path        string
	EncodedDir  string
	UploadDir   string
	Count       int
	IsGallery   bool
	CanUpload   bool
	CanMkcol    bool
	ShowLogout  bool
	LoadError   string
	ParentHref  string
	Breadcrumbs []browserBreadcrumb
	Rows        []browserRow
}

var browserHeadTmpl = template.Must(template.New("webdav-browser-head").Parse(`<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>{{.HeadTitle}}</title>
<style>
{{if .UseLocalFont}}
@font-face{font-display:swap;font-family:'Inter';font-style:normal;font-weight:400;src:url('/__static/fonts/inter-v20-latin-400-700.woff2') format('woff2');}
@font-face{font-display:swap;font-family:'Inter';font-style:normal;font-weight:500;src:url('/__static/fonts/inter-v20-latin-400-700.woff2') format('woff2');}
@font-face{font-display:swap;font-family:'Inter';font-style:normal;font-weight:600;src:url('/__static/fonts/inter-v20-latin-400-700.woff2') format('woff2');}
@font-face{font-display:swap;font-family:'Inter';font-style:normal;font-weight:700;src:url('/__static/fonts/inter-v20-latin-400-700.woff2') format('woff2');}
{{end}}
:root{
  --bg:#0d1015;
  --surface:#14171f;
  --surface-2:#1a1f29;
  --hover:#161b24;
  --border:#1f2530;
  --text:#e6edf3;
  --muted:#8b949e;
  --accent:#38bdf8;
  --accent-bg:#0ea5e9;
  --danger:#f87171;
  --ok:#34d399;
}
*{box-sizing:border-box}
html,body{height:100%}
body{
  margin:0;
  background:var(--bg);
  color:var(--text);
  font-family:'Inter',system-ui,-apple-system,"Segoe UI",Roboto,sans-serif;
  font-size:14px;
  line-height:1.4;
}
a{color:inherit;text-decoration:none}
button,input,select{font:inherit;color:inherit}
@keyframes spin{to{transform:rotate(360deg)}}
@keyframes indet{0%{transform:translateX(-100%)}100%{transform:translateX(350%)}}
.boot-loader{
  min-height:100vh;display:flex;flex-direction:column;align-items:center;justify-content:center;gap:12px;
  padding:24px;text-align:center;
}
.spinner{
  width:28px;height:28px;border-radius:50%;
  border:2.5px solid #222936;border-top-color:var(--accent);
  animation:spin .75s linear infinite;
}
.spinner.sm{width:14px;height:14px;border-width:2px;flex:0 0 14px}
.boot-msg{font-size:14px;font-weight:500;color:var(--text)}
.boot-sub{font-size:12px;color:var(--muted)}
.nav-progress{
  position:fixed;top:0;left:0;right:0;height:2.5px;z-index:80;overflow:hidden;background:rgba(56,189,248,.14);
}
.nav-progress::after{
  content:"";display:block;width:30%;height:100%;background:var(--accent);
  animation:indet 1.05s ease-in-out infinite;
}
.nav-progress[hidden]{display:none}
.nav-status{
  position:fixed;top:12px;left:50%;transform:translateX(-50%);z-index:81;
  display:flex;align-items:center;gap:8px;padding:6px 12px;border-radius:999px;
  background:var(--surface-2);border:1px solid #2e3748;color:var(--text);
  font-size:12.5px;box-shadow:0 8px 20px rgba(0,0,0,.5);
}
.nav-status[hidden]{display:none}
main{max-width:1280px;margin:0 auto;padding:16px 20px 72px}
.toolbar{
  display:flex;
  flex-wrap:wrap;
  align-items:center;
  justify-content:space-between;
  gap:10px;
  margin-bottom:12px;
}
.filters{display:flex;flex-wrap:wrap;align-items:center;gap:8px;flex:1 1 320px}
.actions{display:flex;align-items:center;gap:8px}
.pill,.search,.btn{
  height:32px;
  border-radius:8px;
  border:1px solid var(--border);
  background:var(--surface);
  color:var(--text);
  padding:0 11px;
  font-size:13px;
}
.pill{cursor:pointer}
.search{min-width:220px;max-width:360px;width:100%;padding:0 12px}
.search:focus,.pill:focus{outline:none;border-color:var(--accent)}
.btn{
  cursor:pointer;
  display:inline-flex;
  align-items:center;
  gap:6px;
  font-weight:500;
  transition:background .12s,border-color .12s;
}
.btn:hover{background:var(--surface-2);border-color:#303847}
.btn.primary{background:#0284c7;border-color:#0369a1;color:#fff}
.btn.primary:hover{background:#0369a1}
.subbar{
  display:flex;
  align-items:center;
  justify-content:space-between;
  gap:12px;
  padding:6px 4px 12px;
  border-bottom:1px solid var(--border);
  margin-bottom:2px;
}
.crumbs{display:flex;flex-wrap:wrap;align-items:center;gap:6px;font-size:13px;color:var(--muted)}
.crumb{padding:2px 6px;border-radius:6px;color:var(--text)}
.crumb:hover{background:var(--surface-2);color:var(--accent)}
.sep{color:#525b68;user-select:none}
.meta{color:var(--muted);font-size:12px;white-space:nowrap}
table{width:100%;border-collapse:collapse;table-layout:fixed}
thead th{
  text-align:left;
  font-size:12px;
  font-weight:600;
  color:var(--muted);
  padding:9px 10px;
  border-bottom:1px solid var(--border);
  user-select:none;
}
th.sort{cursor:pointer}
th.sort:hover{color:var(--text)}
.th-name{width:auto}
.th-type{width:120px}
.th-date{width:165px}
.th-size{width:105px;text-align:right}
.th-act{width:42px;text-align:center}
tbody tr{border-bottom:1px solid #161b23;transition:background .08s}
tbody tr:hover{background:var(--hover)}
tbody tr.hide{display:none}
td{
  padding:8px 10px;
  font-size:13.5px;
  vertical-align:middle;
  overflow:hidden;
  text-overflow:ellipsis;
  white-space:nowrap;
}
td.type,td.date{color:var(--muted);font-size:12.5px}
td.size{text-align:right;color:var(--muted);font-size:12.5px;font-variant-numeric:tabular-nums}
td.act-col{text-align:center;padding:4px 4px}
.name-cell{display:flex;align-items:center;gap:10px;min-width:0}
.ico{width:18px;height:18px;flex:0 0 18px}
.name-link{overflow:hidden;text-overflow:ellipsis;white-space:nowrap;display:block;flex:1 1 auto}
.name-link:hover{color:var(--accent)}
.act-btn{
  width:28px;height:28px;border:0;border-radius:6px;
  background:transparent;color:var(--muted);cursor:pointer;
  font-size:16px;line-height:1;display:inline-flex;align-items:center;justify-content:center;
}
.act-btn:hover{background:var(--surface-2);color:var(--text)}
.empty{padding:28px 12px;text-align:center;color:var(--muted);font-size:13px}
.err-banner{
  margin:12px 0;padding:12px 14px;border-radius:8px;
  background:rgba(248,113,113,.1);border:1px solid rgba(248,113,113,.35);
  color:var(--danger);font-size:13px;
}
.menu{
  position:fixed;z-index:50;min-width:160px;
  background:var(--surface);border:1px solid #2b3342;border-radius:8px;
  padding:4px;box-shadow:0 12px 28px rgba(0,0,0,.55);
}
.menu[hidden]{display:none}
.menu-item{
  display:flex;width:100%;align-items:center;gap:8px;
  padding:7px 10px;border:0;border-radius:6px;
  background:transparent;color:var(--text);text-align:left;cursor:pointer;font-size:13px;
}
.menu-item:hover{background:var(--surface-2)}
.menu-item.danger{color:var(--danger)}
.menu-item[hidden]{display:none}
.drop-overlay{
  position:fixed;inset:0;z-index:60;
  background:rgba(13,16,21,.84);
  border:2px dashed var(--accent);
  display:flex;align-items:center;justify-content:center;
  pointer-events:none;
}
.drop-overlay[hidden]{display:none}
.drop-card{
  background:var(--surface);border:1px solid #2d3646;border-radius:12px;
  padding:22px 28px;text-align:center;font-size:15px;color:var(--text);
}
.drop-sub{margin-top:6px;font-size:12.5px;color:var(--muted)}
.upload-box{
  position:fixed;right:18px;bottom:18px;z-index:55;
  width:340px;max-width:calc(100vw - 36px);
  background:var(--surface);border:1px solid #2b3342;border-radius:10px;
  box-shadow:0 12px 28px rgba(0,0,0,.6);overflow:hidden;
}
.upload-box[hidden]{display:none}
.upload-hdr{
  display:flex;align-items:center;justify-content:space-between;
  padding:9px 12px;background:var(--surface-2);border-bottom:1px solid var(--border);
  font-size:12.5px;font-weight:600;
}
.upload-close{border:0;background:transparent;color:var(--muted);cursor:pointer;font-size:14px}
.upload-list{max-height:220px;overflow-y:auto;padding:8px 12px;display:flex;flex-direction:column;gap:8px}
.up-item{font-size:12px}
.up-row{display:flex;justify-content:space-between;gap:8px;margin-bottom:4px}
.up-name{overflow:hidden;text-overflow:ellipsis;white-space:nowrap}
.up-st{color:var(--muted);white-space:nowrap}
.up-st.ok{color:var(--ok)}
.up-st.err{color:var(--danger)}
.up-bar{height:4px;background:#222834;border-radius:4px;overflow:hidden}
.up-fill{height:100%;width:0%;background:var(--accent);transition:width .1s}
.toast{
  position:fixed;left:50%;bottom:20px;transform:translateX(-50%);z-index:70;
  background:#1f2937;border:1px solid #334155;color:var(--text);
  padding:8px 14px;border-radius:8px;font-size:13px;box-shadow:0 8px 20px rgba(0,0,0,.45);
}
.toast[hidden]{display:none}
@media (max-width:700px){
  .th-type,.th-date,td.type,td.date{display:none}
  .th-size{width:84px}
}
</style>
</head>
<body>
<div id="bootLoader" class="boot-loader">
  <div class="spinner"></div>
  <div class="boot-msg">{{.BootMessage}}</div>
  <div class="boot-sub">Preparando listado desde la caché local...</div>
</div>
<svg style="display:none" aria-hidden="true">
  <symbol id="i-folder" viewBox="0 0 20 20">
    <path fill="#e5a93c" d="M2 5.5A1.5 1.5 0 0 1 3.5 4h4.1c.4 0 .78.16 1.06.44L10.22 6H16.5A1.5 1.5 0 0 1 18 7.5v7A1.5 1.5 0 0 1 16.5 16h-13A1.5 1.5 0 0 1 2 14.5v-9z"/>
  </symbol>
  <symbol id="i-image" viewBox="0 0 20 20">
    <rect x="2" y="3.5" width="16" height="13" rx="2" fill="#162436" stroke="#38bdf8" stroke-width="1.4"/>
    <circle cx="7" cy="8" r="1.4" fill="#38bdf8"/>
    <path fill="#38bdf8" d="M4.2 14.5l3.6-3.7 2.5 2.4 2.4-2.8 3.1 4.1H4.2z"/>
  </symbol>
  <symbol id="i-video" viewBox="0 0 20 20">
    <rect x="2" y="3.5" width="16" height="13" rx="2" fill="#211938" stroke="#a78bfa" stroke-width="1.4"/>
    <path fill="#a78bfa" d="M8.2 7.2v5.6l4.8-2.8-4.8-2.8z"/>
  </symbol>
  <symbol id="i-file" viewBox="0 0 20 20">
    <path fill="#1b222c" stroke="#8b949e" stroke-width="1.4" d="M5 2.5h6.5L15.5 6.5V17a1 1 0 0 1-1 1h-9.5a1 1 0 0 1-1-1V3.5a1 1 0 0 1 1-1z"/>
    <path fill="none" stroke="#8b949e" stroke-width="1.4" d="M11.5 2.5v4h4"/>
  </symbol>
</svg>
`))

var browserBodyTmpl = template.Must(template.New("webdav-browser-body").Parse(`<style>#bootLoader{display:none}</style>
<div id="navProgress" class="nav-progress" hidden></div>
<div id="navStatus" class="nav-status" hidden><span class="spinner sm"></span><span id="navStatusMsg">Cargando...</span></div>
<main id="app" data-title="{{.Title}} - {{.Path}}" data-dir="{{.EncodedDir}}" data-upload-dir="{{.UploadDir}}" data-gallery="{{.IsGallery}}" data-can-upload="{{.CanUpload}}" data-can-mkcol="{{.CanMkcol}}" data-count="{{.Count}}">
  <div class="toolbar">
    <div class="filters">
      <select id="typeFilter" class="pill" aria-label="Filtrar por tipo">
        <option value="all">Tipo: Todos</option>
        <option value="folder">Carpetas</option>
        <option value="image">Imágenes</option>
        <option value="video">Vídeos</option>
        <option value="file">Archivos</option>
      </select>
      <input id="searchInput" class="search" type="search" placeholder="Filtrar en esta carpeta..." autocomplete="off">
    </div>
    <div class="actions">
      {{if .CanMkcol}}<button type="button" id="newFolderBtn" class="btn">+ Carpeta</button>{{end}}
      {{if .CanUpload}}<button type="button" id="uploadBtn" class="btn primary">⬆ Subir</button><input id="fileInput" type="file" multiple{{if .IsGallery}} accept="image/*,video/*"{{end}} hidden>{{end}}
      {{if .ShowLogout}}<button type="button" id="logoutBtn" class="btn" title="Cerrar sesión">Salir</button>{{end}}
    </div>
  </div>

  <div class="subbar">
    <nav class="crumbs" aria-label="Ruta actual">
      {{range $i, $b := .Breadcrumbs}}{{if gt $i 0}}<span class="sep">›</span>{{end}}<a class="crumb" href="{{$b.Href}}">{{$b.Name}}</a>{{end}}
    </nav>
    <span class="meta" id="itemCount">{{.Count}} elementos</span>
  </div>

  <table id="fileTable">
    <thead>
      <tr>
        <th class="th-name sort" data-sort="name">Nombre <span class="sort-ind">↑</span></th>
        <th class="th-type sort" data-sort="type">Tipo <span class="sort-ind"></span></th>
        <th class="th-date sort" data-sort="date">Modificado <span class="sort-ind"></span></th>
        <th class="th-size sort" data-sort="size">Tamaño <span class="sort-ind"></span></th>
        <th class="th-act" aria-label="Acciones"></th>
      </tr>
    </thead>
    <tbody id="fileBody">
      {{if .ParentHref}}
      <tr class="parent-row" data-parent="1">
        <td><div class="name-cell"><svg class="ico" viewBox="0 0 20 20" aria-hidden="true"><use href="#i-folder"/></svg><a class="name-link" href="{{.ParentHref}}">..</a></div></td>
        <td class="type">Carpeta</td>
        <td class="date">-</td>
        <td class="size">-</td>
        <td class="act-col"></td>
      </tr>
      {{end}}
      {{range .Rows}}
      <tr class="row" data-name="{{.Name}}" data-kind="{{.Kind}}" data-type="{{.TypeLabel}}" data-size="{{.SizeBytes}}" data-date="{{.ModUnix}}" data-href="{{.Href}}" data-dir="{{.IsDir}}" data-can-rename="{{.CanRename}}" data-can-delete="{{.CanDelete}}">
        <td><div class="name-cell"><svg class="ico" viewBox="0 0 20 20" aria-hidden="true"><use href="#{{.IconID}}"/></svg><a class="name-link" href="{{.Href}}">{{.Name}}</a></div></td>
        <td class="type">{{.TypeLabel}}</td>
        <td class="date">{{.Date}}</td>
        <td class="size">{{.Size}}</td>
        <td class="act-col">{{if or (not .IsDir) .CanRename .CanDelete}}<button type="button" class="act-btn" title="Acciones" aria-label="Acciones">⋮</button>{{end}}</td>
      </tr>
      {{end}}
    </tbody>
  </table>
  {{if .LoadError}}<div class="err-banner">{{.LoadError}}</div>{{else}}<div id="emptyState" class="empty"{{if gt .Count 0}} hidden{{end}}>No hay elementos que mostrar.</div>{{end}}
</main>

<div id="ctxMenu" class="menu" hidden>
  <a id="actDownload" class="menu-item" href="#" download>⬇ Descargar</a>
  <button id="actRename" type="button" class="menu-item">✏️ Renombrar</button>
  <button id="actDelete" type="button" class="menu-item danger">🗑️ Eliminar</button>
</div>

<div id="dropOverlay" class="drop-overlay" hidden>
  <div class="drop-card">
    <div>Suelta los archivos para subirlos</div>
    <div class="drop-sub">Destino: {{.Path}}</div>
  </div>
</div>

<div id="uploadBox" class="upload-box" hidden>
  <div class="upload-hdr">
    <span id="uploadTitle">Subidas</span>
    <button type="button" id="uploadClose" class="upload-close" aria-label="Cerrar">✕</button>
  </div>
  <div id="uploadList" class="upload-list"></div>
</div>

<div id="toast" class="toast" hidden></div>

<script nonce="{{.Nonce}}">
(function(){
  var app = document.getElementById('app');
  if (!app) return;
  if (app.dataset.title) document.title = app.dataset.title;
  var dirPath = app.dataset.dir || '/';
  var uploadDir = app.dataset.uploadDir || dirPath;
  var canUpload = app.dataset.canUpload === 'true';
  var canMkcol = app.dataset.canMkcol === 'true';

  var navProgress = document.getElementById('navProgress');
  var navStatus = document.getElementById('navStatus');
  var navStatusMsg = document.getElementById('navStatusMsg');

  function showNavLoading(msg) {
    if (navStatusMsg && msg) navStatusMsg.textContent = msg;
    if (navProgress) navProgress.hidden = false;
    if (navStatus) navStatus.hidden = false;
  }
  function hideNavLoading() {
    if (navProgress) navProgress.hidden = true;
    if (navStatus) navStatus.hidden = true;
  }
  window.addEventListener('pageshow', hideNavLoading);

  app.addEventListener('click', function(ev) {
    if (ev.button !== 0 || ev.ctrlKey || ev.metaKey || ev.shiftKey || ev.altKey) return;
    var link = ev.target.closest('a.crumb, tr.parent-row a.name-link, tr.row[data-dir="true"] a.name-link');
    if (!link || link.hasAttribute('download') || link.target === '_blank') return;
    showNavLoading('Cargando carpeta...');
  });

  var tbody = document.getElementById('fileBody');
  var typeFilter = document.getElementById('typeFilter');
  var searchInput = document.getElementById('searchInput');
  var itemCount = document.getElementById('itemCount');
  var emptyState = document.getElementById('emptyState');

  var ctxMenu = document.getElementById('ctxMenu');
  var actDownload = document.getElementById('actDownload');
  var actRename = document.getElementById('actRename');
  var actDelete = document.getElementById('actDelete');
  var activeRow = null;

  var toastEl = document.getElementById('toast');
  var toastTimer = null;

  function showToast(msg) {
    if (!toastEl) return;
    toastEl.textContent = msg;
    toastEl.hidden = false;
    if (toastTimer) clearTimeout(toastTimer);
    toastTimer = setTimeout(function(){ toastEl.hidden = true; }, 3500);
  }

  function joinEncoded(base, name) {
    var b = base === '/' ? '' : base.replace(/\/+$/, '');
    return b + '/' + encodeURIComponent(name);
  }

  function getRows() {
    return Array.prototype.slice.call(tbody.querySelectorAll('tr.row'));
  }

  function updateCount(visible) {
    var total = getRows().length;
    if (searchInput.value.trim() || typeFilter.value !== 'all') {
      itemCount.textContent = visible + ' de ' + total + ' elementos';
    } else {
      itemCount.textContent = total + (total === 1 ? ' elemento' : ' elementos');
    }
    if (emptyState) emptyState.hidden = visible > 0;
  }

  function applyFilters() {
    var q = searchInput.value.trim().toLowerCase();
    var kind = typeFilter.value;
    var rows = getRows();
    var visible = 0;
    for (var i = 0; i < rows.length; i++) {
      var r = rows[i];
      var matchKind = (kind === 'all') || (r.dataset.kind === kind);
      var matchText = !q || (r.dataset.name || '').toLowerCase().indexOf(q) !== -1;
      var show = matchKind && matchText;
      r.classList.toggle('hide', !show);
      if (show) visible++;
    }
    updateCount(visible);
  }

  typeFilter.addEventListener('change', applyFilters);
  searchInput.addEventListener('input', applyFilters);

  // Sortable columns
  var sortKey = 'name';
  var sortAsc = true;
  var headers = document.querySelectorAll('th.sort');
  function sortRows(key) {
    if (sortKey === key) {
      sortAsc = !sortAsc;
    } else {
      sortKey = key;
      sortAsc = (key === 'name' || key === 'type');
    }
    for (var h = 0; h < headers.length; h++) {
      var ind = headers[h].querySelector('.sort-ind');
      if (!ind) continue;
      ind.textContent = (headers[h].dataset.sort === sortKey) ? (sortAsc ? '↑' : '↓') : '';
    }
    var rows = getRows();
    rows.sort(function(a, b) {
      if (sortKey === 'name') {
        var ad = a.dataset.dir === 'true';
        var bd = b.dataset.dir === 'true';
        if (ad !== bd) return ad ? -1 : 1;
        var an = (a.dataset.name || '').toLowerCase();
        var bn = (b.dataset.name || '').toLowerCase();
        return sortAsc ? an.localeCompare(bn) : bn.localeCompare(an);
      }
      if (sortKey === 'type') {
        var at = (a.dataset.type || '').toLowerCase();
        var bt = (b.dataset.type || '').toLowerCase();
        if (at !== bt) return sortAsc ? at.localeCompare(bt) : bt.localeCompare(at);
        return (a.dataset.name || '').localeCompare(b.dataset.name || '');
      }
      if (sortKey === 'date') {
        var am = Number(a.dataset.date || 0);
        var bm = Number(b.dataset.date || 0);
        return sortAsc ? am - bm : bm - am;
      }
      var as = Number(a.dataset.size || -1);
      var bs = Number(b.dataset.size || -1);
      return sortAsc ? as - bs : bs - as;
    });
    var frag = document.createDocumentFragment();
    for (var i = 0; i < rows.length; i++) {
      frag.appendChild(rows[i]);
    }
    tbody.appendChild(frag);
  }
  for (var i = 0; i < headers.length; i++) {
    headers[i].addEventListener('click', function(){ sortRows(this.dataset.sort); });
  }

  // Context menu
  function closeMenu() {
    ctxMenu.hidden = true;
    activeRow = null;
  }

  tbody.addEventListener('click', function(e) {
    var btn = e.target.closest('.act-btn');
    if (!btn) return;
    e.stopPropagation();
    var row = btn.closest('tr.row');
    if (!row) return;
    if (activeRow === row && !ctxMenu.hidden) {
      closeMenu();
      return;
    }
    activeRow = row;
    var isDir = row.dataset.dir === 'true';
    actDownload.hidden = isDir;
    if (!isDir) {
      actDownload.href = row.dataset.href;
      actDownload.setAttribute('download', row.dataset.name || 'archivo');
    }
    actRename.hidden = row.dataset.canRename !== 'true';
    actDelete.hidden = row.dataset.canDelete !== 'true';

    ctxMenu.hidden = false;
    var rect = btn.getBoundingClientRect();
    var mw = ctxMenu.offsetWidth || 165;
    var mh = ctxMenu.offsetHeight || 110;
    var left = Math.min(rect.right - mw, window.innerWidth - mw - 10);
    if (left < 8) left = 8;
    var top = rect.bottom + 4;
    if (top + mh > window.innerHeight - 8) {
      top = Math.max(8, rect.top - mh - 4);
    }
    ctxMenu.style.left = left + 'px';
    ctxMenu.style.top = top + 'px';
  });

  document.addEventListener('click', function(e) {
    if (!ctxMenu.hidden && !ctxMenu.contains(e.target)) {
      closeMenu();
    }
  });

  actDownload.addEventListener('click', function() {
    setTimeout(closeMenu, 50);
  });

  actRename.addEventListener('click', function() {
    var row = activeRow;
    closeMenu();
    if (!row) return;
    var oldName = row.dataset.name || '';
    var newName = window.prompt('Nuevo nombre:', oldName);
    if (newName === null) return;
    newName = newName.trim();
    if (!newName || newName === oldName) return;
    if (newName === '.' || newName === '..' || /[\/\\]/.test(newName)) {
      showToast('Nombre no válido.');
      return;
    }
    var dstHref = new URL(joinEncoded(dirPath, newName), window.location.origin).href;
    fetch(row.dataset.href, {
      method: 'MOVE',
      credentials: 'same-origin',
      headers: {
        'Destination': dstHref,
        'Overwrite': 'F'
      }
    }).then(function(resp) {
      if (resp.ok) {
        showNavLoading('Renombrando...');
        window.location.reload();
        return null;
      }
      return resp.text().then(function(txt) {
        if (resp.status === 412) {
          showToast('Ya existe un elemento con ese nombre.');
        } else {
          showToast('Error al renombrar (' + resp.status + '): ' + (txt || resp.statusText).trim());
        }
      });
    }).catch(function(err) {
      showToast('Error de red al renombrar: ' + err.message);
    });
  });

  actDelete.addEventListener('click', function() {
    var row = activeRow;
    closeMenu();
    if (!row) return;
    var name = row.dataset.name || '';
    var isDir = row.dataset.dir === 'true';
    var promptMsg = isDir
      ? '¿Enviar la carpeta "' + name + '" y todo su contenido a la papelera?'
      : '¿Enviar "' + name + '" a la papelera?';
    if (!window.confirm(promptMsg)) return;
    fetch(row.dataset.href, {
      method: 'DELETE',
      credentials: 'same-origin'
    }).then(function(resp) {
      if (resp.ok) {
        row.remove();
        applyFilters();
        showToast('Eliminado: ' + name);
        return null;
      }
      return resp.text().then(function(txt) {
        showToast('Error al eliminar (' + resp.status + '): ' + (txt || resp.statusText).trim());
      });
    }).catch(function(err) {
      showToast('Error de red al eliminar: ' + err.message);
    });
  });

  // New folder (MKCOL)
  if (canMkcol) {
    var newFolderBtn = document.getElementById('newFolderBtn');
    if (newFolderBtn) {
      newFolderBtn.addEventListener('click', function() {
        var name = window.prompt('Nombre de la nueva carpeta:');
        if (name === null) return;
        name = name.trim();
        if (!name) return;
        if (name === '.' || name === '..' || /[\/\\]/.test(name)) {
          showToast('Nombre de carpeta no válido.');
          return;
        }
        var target = joinEncoded(dirPath, name);
        fetch(target, {
          method: 'MKCOL',
          credentials: 'same-origin'
        }).then(function(resp) {
          if (resp.ok) {
            showNavLoading('Creando carpeta...');
            window.location.reload();
            return null;
          }
          return resp.text().then(function(txt) {
            showToast('No se pudo crear la carpeta (' + resp.status + '): ' + (txt || resp.statusText).trim());
          });
        }).catch(function(err) {
          showToast('Error de red creando carpeta: ' + err.message);
        });
      });
    }
  }

  // Sequential upload queue (Drag & Drop + file input)
  if (canUpload) {
    var uploadBtn = document.getElementById('uploadBtn');
    var fileInput = document.getElementById('fileInput');
    var dropOverlay = document.getElementById('dropOverlay');
    var uploadBox = document.getElementById('uploadBox');
    var uploadList = document.getElementById('uploadList');
    var uploadTitle = document.getElementById('uploadTitle');
    var uploadClose = document.getElementById('uploadClose');

    var queue = [];
    var uploading = false;
    var okCount = 0;
    var errCount = 0;

    function createUploadRow(file) {
      var item = document.createElement('div');
      item.className = 'up-item';
      var row = document.createElement('div');
      row.className = 'up-row';
      var nameSpan = document.createElement('span');
      nameSpan.className = 'up-name';
      nameSpan.textContent = file.name;
      var stSpan = document.createElement('span');
      stSpan.className = 'up-st';
      stSpan.textContent = 'En cola';
      row.appendChild(nameSpan);
      row.appendChild(stSpan);
      var bar = document.createElement('div');
      bar.className = 'up-bar';
      var fill = document.createElement('div');
      fill.className = 'up-fill';
      bar.appendChild(fill);
      item.appendChild(row);
      item.appendChild(bar);
      uploadList.appendChild(item);
      return { st: stSpan, fill: fill };
    }

    function runNextUpload() {
      if (uploading || queue.length === 0) {
        if (!uploading && queue.length === 0 && (okCount > 0 || errCount > 0)) {
          if (errCount === 0) {
            uploadTitle.textContent = 'Subida completada (' + okCount + ')';
            showNavLoading('Actualizando...');
            setTimeout(function(){ window.location.reload(); }, 600);
          } else {
            uploadTitle.textContent = 'Completado (' + okCount + ' ok, ' + errCount + ' con error)';
          }
        }
        return;
      }
      uploading = true;
      var entry = queue.shift();
      var file = entry.file;
      var ui = entry.ui;
      uploadTitle.textContent = 'Subiendo (' + (queue.length + 1) + ' restante' + (queue.length ? 's' : '') + ')...';
      ui.st.textContent = '0%';

      var xhr = new XMLHttpRequest();
      xhr.open('PUT', joinEncoded(uploadDir, file.name), true);
      xhr.withCredentials = true;
      var mtimeSec = Math.floor((file.lastModified || Date.now()) / 1000);
      xhr.setRequestHeader('X-OC-Mtime', String(mtimeSec));

      xhr.upload.onprogress = function(ev) {
        if (ev.lengthComputable && ev.total > 0) {
          var pct = Math.min(100, Math.round((ev.loaded / ev.total) * 100));
          ui.fill.style.width = pct + '%';
          ui.st.textContent = pct < 100 ? (pct + '%') : 'Guardando...';
        }
      };
      xhr.onload = function() {
        uploading = false;
        if (xhr.status >= 200 && xhr.status < 300) {
          okCount++;
          ui.fill.style.width = '100%';
          ui.st.textContent = '✔';
          ui.st.classList.add('ok');
        } else {
          errCount++;
          ui.st.textContent = 'Error ' + xhr.status;
          ui.st.classList.add('err');
          if (xhr.responseText) {
            showToast(file.name + ': ' + xhr.responseText.trim());
          }
        }
        runNextUpload();
      };
      xhr.onerror = function() {
        uploading = false;
        errCount++;
        ui.st.textContent = 'Error red';
        ui.st.classList.add('err');
        runNextUpload();
      };
      xhr.send(file);
    }

    function enqueueFiles(files) {
      if (!files || !files.length) return;
      uploadBox.hidden = false;
      for (var i = 0; i < files.length; i++) {
        var f = files[i];
        var ui = createUploadRow(f);
        queue.push({ file: f, ui: ui });
      }
      runNextUpload();
    }

    if (uploadBtn && fileInput) {
      uploadBtn.addEventListener('click', function(){ fileInput.click(); });
      fileInput.addEventListener('change', function() {
        enqueueFiles(fileInput.files);
        fileInput.value = '';
      });
    }

    if (uploadClose) {
      uploadClose.addEventListener('click', function() {
        uploadBox.hidden = true;
        if (okCount > 0 && !uploading) {
          window.location.reload();
        }
      });
    }

    var dragDepth = 0;
    function hasFiles(ev) {
      if (!ev.dataTransfer || !ev.dataTransfer.types) return false;
      for (var i = 0; i < ev.dataTransfer.types.length; i++) {
        if (ev.dataTransfer.types[i] === 'Files') return true;
      }
      return false;
    }
    window.addEventListener('dragenter', function(ev) {
      if (!hasFiles(ev)) return;
      ev.preventDefault();
      dragDepth++;
      dropOverlay.hidden = false;
    });
    window.addEventListener('dragover', function(ev) {
      if (!hasFiles(ev)) return;
      ev.preventDefault();
      ev.dataTransfer.dropEffect = 'copy';
    });
    window.addEventListener('dragleave', function(ev) {
      if (!hasFiles(ev)) return;
      dragDepth = Math.max(0, dragDepth - 1);
      if (dragDepth === 0) dropOverlay.hidden = true;
    });
    window.addEventListener('drop', function(ev) {
      if (!hasFiles(ev)) return;
      ev.preventDefault();
      dragDepth = 0;
      dropOverlay.hidden = true;
      enqueueFiles(ev.dataTransfer.files);
    });
  }

  // Keyboard shortcuts: '/' focuses search, Escape closes menu/blurs
  document.addEventListener('keydown', function(ev) {
    if (ev.key === 'Escape') {
      closeMenu();
      if (document.activeElement === searchInput) {
        searchInput.blur();
      }
    } else if (ev.key === '/' && document.activeElement !== searchInput && !ev.ctrlKey && !ev.metaKey && !ev.altKey) {
      ev.preventDefault();
      searchInput.focus();
    }
  });

  var logoutBtn = document.getElementById('logoutBtn');
  if (logoutBtn) {
    logoutBtn.addEventListener('click', function() {
      fetch('/__logout', {
        method: 'POST',
        credentials: 'same-origin',
        headers: { 'Accept': 'application/json' }
      }).finally(function() {
        window.location.href = '/__login';
      });
    });
  }
})();
</script>
</body>
</html>
`))

func newCSPNonce() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return base64.StdEncoding.EncodeToString(b[:])
}

func formatHumanBytes(b int64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.2f MB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(b)/(1<<10))
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func encodePathSegments(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return "/"
	}
	for i, seg := range parts {
		parts[i] = url.PathEscape(seg)
	}
	return "/" + strings.Join(parts, "/")
}

func classifyBrowserEntry(e os.FileInfo) (iconID, kind, typeLabel, sizeStr string, sizeBytes int64) {
	if e.IsDir() {
		return "i-folder", "folder", "Carpeta", "-", -1
	}
	sz := e.Size()
	sizeStr = formatHumanBytes(sz)
	switch strings.ToLower(path.Ext(e.Name())) {
	case ".mp4", ".mov", ".m4v", ".3gp", ".avi", ".mkv", ".webm":
		return "i-video", "video", "Vídeo", sizeStr, sz
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".heic", ".heif", ".bmp", ".tiff", ".tif", ".avif":
		return "i-image", "image", "Imagen", sizeStr, sz
	case ".pdf":
		return "i-file", "file", "PDF", sizeStr, sz
	case ".zip", ".7z", ".rar", ".tar", ".gz":
		return "i-file", "file", "Comprimido", sizeStr, sz
	case ".txt", ".md", ".csv", ".json", ".xml", ".log", ".doc", ".docx", ".xls", ".xlsx", ".ppt", ".pptx":
		return "i-file", "file", "Documento", sizeStr, sz
	default:
		return "i-file", "file", "Archivo", sizeStr, sz
	}
}

func buildBrowserBreadcrumbs(canonicalDir string) []browserBreadcrumb {
	crumbs := []browserBreadcrumb{{Name: "Inicio", Href: "/"}}
	trimmed := strings.Trim(canonicalDir, "/")
	if trimmed == "" {
		return crumbs
	}
	parts := strings.Split(trimmed, "/")
	acc := ""
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		acc += "/" + seg
		crumbs = append(crumbs, browserBreadcrumb{
			Name: seg,
			Href: encodePathSegments(acc),
		})
	}
	return crumbs
}

func (mfs *MovistarFS) resolveCanonicalFolder(cleanPath string) (string, int64, bool) {
	if mfs == nil {
		return "", 0, false
	}
	rootID, err := mfs.getRootFolderID()
	if err != nil {
		return "", 0, false
	}
	if cleanPath == "/" || cleanPath == "." {
		return "/", rootID, true
	}
	parts := strings.Split(strings.Trim(cleanPath, "/"), "/")
	currID := rootID
	fold := mfs.caseInsensitive()
	canonicalParts := make([]string, 0, len(parts))
	now := time.Now()

	for _, part := range parts {
		var matched *sapi.FolderItem
		mfs.cacheMu.RLock()
		for k, cf := range mfs.createdFolders {
			if exp, tomb := mfs.deletedTombstones[k]; tomb && now.Before(exp) {
				continue
			}
			if time.Since(cf.Time) < 60*time.Second && cf.Info.ParentID == currID {
				if cf.Info.Name == part || (fold && strings.EqualFold(cf.Info.Name, part)) {
					info := cf.Info
					matched = &info
					break
				}
			}
		}
		mfs.cacheMu.RUnlock()

		if matched == nil {
			subs, err := mfs.getSubfolders(currID)
			if err != nil {
				return "", 0, false
			}
			for i := range subs {
				if strings.EqualFold(subs[i].Name, ProxyInternalDir) {
					continue
				}
				if subs[i].Name == part {
					matched = &subs[i]
					break
				}
				if fold && matched == nil && strings.EqualFold(subs[i].Name, part) {
					matched = &subs[i]
				}
			}
		}
		if matched == nil {
			return "", 0, false
		}
		canonicalParts = append(canonicalParts, matched.Name)
		currID = matched.ID
	}
	return "/" + strings.Join(canonicalParts, "/"), currID, true
}

// serveGalleryBrowserHTML sirve el explorador web en modo listado cuando se abre "/",
// una carpeta de «Mis Archivos» o una subcarpeta de /_gallery desde un navegador (GET / HEAD).
func (s *Server) serveGalleryBrowserHTML(w http.ResponseWriter, r *http.Request, urlPath string) bool {
	if s == nil || s.fs == nil {
		return false
	}
	cleanPath := path.Clean("/" + urlPath)

	var canonicalDir string
	var entries []os.FileInfo
	var folderID int64
	pageTitle := "Movistar Cloud"
	isGallery := s.fs.isGalleryPath(cleanPath)

	if isGallery {
		if !s.fs.isGalleryVirtualDir(cleanPath) {
			return false
		}
		pageTitle = "Galería"
	} else {
		if cleanPath != "/" && cleanPath != "." {
			_, mediaInfo, isFolder, err := s.fs.resolvePath(cleanPath)
			if err != nil || !isFolder || mediaInfo != nil {
				return false
			}
		}
		var ok bool
		canonicalDir, folderID, ok = s.fs.resolveCanonicalFolder(cleanPath)
		if !ok {
			return false
		}
	}

	nonce := newCSPNonce()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; font-src 'self'; script-src 'nonce-"+nonce+"'; connect-src 'self'; frame-ancestors 'none'")

	var loadErrMsg string
	if r.Method == http.MethodHead {
		if isGallery {
			idx, err := s.fs.getGalleryIndex()
			if err != nil {
				http.Error(w, "Error cargando galería", http.StatusInternalServerError)
				return true
			}
			if _, _, ok := idx.lookupCanonicalDir(cleanPath, s.fs.caseInsensitive()); !ok {
				http.NotFound(w, r)
				return true
			}
		}
		w.WriteHeader(http.StatusOK)
		return true
	}

	// Para GET enviamos cabecera + spinner de carga al instante antes de consultar SQLite/caché.
	useLocalFont := s.browserAuth != nil && s.browserAuth.HandleStatic != nil
	headTitle := "Cargando archivos - Movistar Cloud"
	bootMsg := "Cargando archivos..."
	if isGallery {
		headTitle = "Cargando Galería - Movistar Cloud"
		bootMsg = "Cargando galería..."
	}
	w.WriteHeader(http.StatusOK)
	_ = browserHeadTmpl.Execute(w, browserHeadData{
		HeadTitle:    headTitle,
		UseLocalFont: useLocalFont,
		BootMessage:  bootMsg,
	})
	if fl, ok := w.(http.Flusher); ok {
		fl.Flush()
	}

	if isGallery {
		idx, err := s.fs.getGalleryIndex()
		if err != nil {
			loadErrMsg = "Error cargando la galería."
			canonicalDir = "/" + config.GalleryDir()
		} else {
			var ok bool
			canonicalDir, entries, ok = idx.lookupCanonicalDir(cleanPath, s.fs.caseInsensitive())
			if !ok {
				canonicalDir = "/" + config.GalleryDir()
			}
		}
	} else {
		dr := NewWebDAVDirReader(s.fs, canonicalDir, folderID)
		var err error
		entries, err = dr.Readdir(-1)
		if err != nil {
			loadErrMsg = "Error listando el directorio."
		} else {
			sort.Slice(entries, func(i, j int) bool {
				if entries[i].IsDir() != entries[j].IsDir() {
					return entries[i].IsDir()
				}
				li := strings.ToLower(entries[i].Name())
				lj := strings.ToLower(entries[j].Name())
				if li != lj {
					return li < lj
				}
				return entries[i].Name() < entries[j].Name()
			})
		}
	}

	var parentHref string
	parent := path.Dir(canonicalDir)
	if canonicalDir != "/" && canonicalDir != "." && parent != canonicalDir && parent != "." {
		parentHref = encodePathSegments(parent)
	}

	canUpload := !isGallery || config.GalleryUpload()
	canMkcol := !isGallery
	canGalleryDelete := config.GalleryDelete()
	encodedDir := encodePathSegments(canonicalDir)
	uploadDir := encodedDir
	if isGallery && !s.fs.validGalleryUploadPath(path.Clean(canonicalDir+"/x.jpg")) {
		uploadDir = encodePathSegments("/" + config.GalleryDir())
	}

	rows := make([]browserRow, 0, len(entries))
	for _, e := range entries {
		childPath := path.Clean(canonicalDir + "/" + e.Name())
		iconID, kind, typeLabel, sizeStr, sizeBytes := classifyBrowserEntry(e)

		dateStr := "-"
		var modUnix int64
		if !e.ModTime().IsZero() && e.ModTime().Year() > 1970 {
			dateStr = e.ModTime().Local().Format("2006-01-02 15:04")
			modUnix = e.ModTime().Unix()
		}

		isVirtualGalleryEntry := isGallery || (canonicalDir == "/" && e.IsDir() && s.fs.isGalleryPath(childPath))
		canRename := !isVirtualGalleryEntry
		canDelete := !isVirtualGalleryEntry || (isGallery && !e.IsDir() && canGalleryDelete)

		rows = append(rows, browserRow{
			IconID:    iconID,
			Kind:      kind,
			TypeLabel: typeLabel,
			Href:      encodePathSegments(childPath),
			Name:      e.Name(),
			Size:      sizeStr,
			SizeBytes: sizeBytes,
			Date:      dateStr,
			ModUnix:   modUnix,
			IsDir:     e.IsDir(),
			CanRename: canRename,
			CanDelete: canDelete,
		})
	}

	showLogout := s.authCfg.IsEnabled() && s.browserAuth != nil && s.browserAuth.ValidateCookie != nil && s.browserAuth.ValidateCookie(r)

	_ = browserBodyTmpl.Execute(w, browserPageData{
		Nonce:       nonce,
		Title:       pageTitle,
		Path:        canonicalDir,
		EncodedDir:  encodedDir,
		UploadDir:   uploadDir,
		Count:       len(entries),
		IsGallery:   isGallery,
		CanUpload:   canUpload,
		CanMkcol:    canMkcol,
		ShowLogout:  showLogout,
		LoadError:   loadErrMsg,
		ParentHref:  parentHref,
		Breadcrumbs: buildBrowserBreadcrumbs(canonicalDir),
		Rows:        rows,
	})
	return true
}
