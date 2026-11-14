// Package webclient accede a Movistar Cloud igual que la web oficial: un Chromium
// con la sesión del usuario ejecuta las llamadas a SAPI dentro de micloud.movistar.es.
package webclient

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"

	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
)

// LoginView sirve una página web que muestra en vivo la pestaña del Chromium sin
// pantalla y le reenvía clics, texto y teclas, para que el usuario inicie sesión
// en Movistar desde cualquier navegador sin entorno gráfico en el servidor.
type LoginView struct {
	tab      func() (context.Context, error) // pestaña chromedp actual
	LoggedIn func() bool                     // opcional: estado de la sesión para mostrarlo en la página
	mu       sync.Mutex                      // serializa la entrada para no mezclar eventos
}

// NewLoginView crea la vista sobre una pestaña fija.
func NewLoginView(tabCtx context.Context) *LoginView {
	return &LoginView{tab: func() (context.Context, error) { return tabCtx, nil }}
}

// Handler devuelve las rutas de la vista bajo el prefijo dado (p. ej. "/login/").
func (v *LoginView) Handler(prefix string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(prefix, v.serveIndex)
	mux.HandleFunc(prefix+"screen.jpg", v.serveScreen)
	mux.HandleFunc(prefix+"input", v.serveInput)
	mux.HandleFunc(prefix+"status", v.serveStatus)
	return mux
}

func (v *LoginView) serveScreen(w http.ResponseWriter, r *http.Request) {
	tab, err := v.tab()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	q := int64(60)
	res, err := chromedp.Call(tab, page.CaptureScreenshot, page.CaptureScreenshotParams{
		Format:  page.CaptureScreenshotFormatJpeg,
		Quality: &q,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(res.Data)
}

type inputEvent struct {
	Kind string  `json:"kind"` // click | text | key | wheel
	X    float64 `json:"x"`
	Y    float64 `json:"y"`
	Text string  `json:"text"`
	Key  string  `json:"key"` // Enter | Tab | Backspace
	DY   float64 `json:"dy"`
}

// Teclas especiales admitidas: nombre DOM → código virtual de Windows.
var specialKeys = map[string]int64{"Enter": 13, "Tab": 9, "Backspace": 8, "Escape": 27}

func (v *LoginView) serveInput(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST", http.StatusMethodNotAllowed)
		return
	}
	var ev inputEvent
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&ev); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()

	tab, err := v.tab()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	switch ev.Kind {
	case "click":
		for _, t := range []input.DispatchMouseEventType{input.DispatchMouseEventTypeMousePressed, input.DispatchMouseEventTypeMouseReleased} {
			if _, err = chromedp.Call(tab, input.DispatchMouseEvent, input.DispatchMouseEventParams{
				Type: t, X: ev.X, Y: ev.Y, Button: input.MouseButtonLeft, Buttons: 1, ClickCount: 1,
			}); err != nil {
				break
			}
		}
	case "wheel":
		_, err = chromedp.Call(tab, input.DispatchMouseEvent, input.DispatchMouseEventParams{
			Type: input.DispatchMouseEventTypeMouseWheel, X: ev.X, Y: ev.Y, DeltaY: ev.DY,
		})
	case "text":
		_, err = chromedp.Call(tab, input.InsertText, input.InsertTextParams{Text: ev.Text})
	case "key":
		code, ok := specialKeys[ev.Key]
		if !ok {
			http.Error(w, "tecla no admitida", http.StatusBadRequest)
			return
		}
		text := ""
		if ev.Key == "Enter" {
			text = "\r"
		}
		if _, err = chromedp.Call(tab, input.DispatchKeyEvent, input.DispatchKeyEventParams{
			Type: input.DispatchKeyEventTypeKeyDown, Key: ev.Key, Code: ev.Key, Text: text, WindowsVirtualKeyCode: code,
		}); err == nil {
			_, err = chromedp.Call(tab, input.DispatchKeyEvent, input.DispatchKeyEventParams{
				Type: input.DispatchKeyEventTypeKeyUp, Key: ev.Key, Code: ev.Key, WindowsVirtualKeyCode: code,
			})
		}
	default:
		http.Error(w, "tipo desconocido", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (v *LoginView) serveStatus(w http.ResponseWriter, r *http.Request) {
	logged := v.LoggedIn != nil && v.LoggedIn()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]bool{"loggedIn": logged})
}

func (v *LoginView) serveIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(loginHTML))
}

const loginHTML = `<!doctype html>
<html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Inicio de sesión remoto</title>
<style>
:root{--bg:#f6f7f9;--fg:#1d2330;--muted:#5c6475;--card:#fff;--line:#d9dde5;--ok:#1f7a3f;--accent:#0b66d6}
@media (prefers-color-scheme:dark){:root{--bg:#14171c;--fg:#e8ebf0;--muted:#9aa3b2;--card:#1d2128;--line:#323844;--ok:#4cc27a;--accent:#5aa2ff}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.4 system-ui,sans-serif}
main{max-width:1300px;margin:0 auto;padding:16px}
h1{font-size:18px;margin:0 0 4px}p{margin:0 0 12px;color:var(--muted)}
#screen{width:100%;max-width:1280px;border:1px solid var(--line);border-radius:8px;cursor:pointer;display:block;background:var(--card)}
.bar{display:flex;flex-wrap:wrap;gap:8px;margin:12px 0}
input{flex:1;min-width:200px;padding:8px 10px;border:1px solid var(--line);border-radius:6px;background:var(--card);color:var(--fg);font:inherit}
button{padding:8px 12px;border:1px solid var(--line);border-radius:6px;background:var(--card);color:var(--fg);font:inherit;cursor:pointer}
button.primary{background:var(--accent);border-color:var(--accent);color:#fff}
#st{font-weight:600}#st.ok{color:var(--ok)}
</style></head><body><main>
<h1>Inicio de sesión en Movistar Cloud</h1>
<p>Haz clic en la imagen para enfocar un campo, escribe abajo y pulsa Enviar. Estado: <span id="st">esperando…</span></p>
<img id="screen" alt="Pantalla del navegador del servidor">
<div class="bar">
<input id="txt" placeholder="Texto para el campo enfocado (teléfono, código SMS…)" autocomplete="off">
<button class="primary" id="send">Enviar texto</button>
<button data-key="Backspace">⌫</button><button data-key="Tab">Tab</button><button data-key="Enter">Intro</button>
</div></main>
<script>
const img=document.getElementById('screen'),st=document.getElementById('st'),txt=document.getElementById('txt');
const post=b=>fetch('input',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify(b)});
let busy=false;function refresh(){if(busy)return;busy=true;const n=new Image();n.onload=()=>{img.src=n.src;busy=false};n.onerror=()=>{busy=false};n.src='screen.jpg?'+Date.now()}
setInterval(refresh,700);refresh();
const pos=e=>{const r=img.getBoundingClientRect();return{x:(e.clientX-r.left)*img.naturalWidth/r.width,y:(e.clientY-r.top)*img.naturalHeight/r.height}};
img.addEventListener('click',e=>post({kind:'click',...pos(e)}).then(refresh));
img.addEventListener('wheel',e=>{e.preventDefault();post({kind:'wheel',...pos(e),dy:e.deltaY})},{passive:false});
document.getElementById('send').onclick=()=>{if(txt.value){post({kind:'text',text:txt.value}).then(refresh);txt.value=''}};
txt.addEventListener('keydown',e=>{if(e.key==='Enter'){e.preventDefault();document.getElementById('send').click()}});
document.querySelectorAll('[data-key]').forEach(b=>b.onclick=()=>post({kind:'key',key:b.dataset.key}).then(refresh));
setInterval(()=>fetch('status').then(r=>r.json()).then(s=>{if(s.loggedIn){st.textContent='sesión iniciada ✔';st.className='ok'}}),3000);
</script></body></html>`
