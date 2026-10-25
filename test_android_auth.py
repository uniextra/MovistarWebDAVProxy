import requests
import json
import base64
import time

access_token = "5a36f383-f6ce-462e-a47f-f26af186c269"
refresh_token = "79451d2d-8d37-4f64-b855-2038226770a3"
device_id = "fac-0123456789abcdef"
device_name = "Samsung SM-S928B (Test)"

def build_oauth_auth_header(acc, ref=""):
    payload = {
        "data": {
            "accesstoken": acc,
            "refreshtoken": ref,
            "platform": "android",
            "expiresin": "3600",
            "lastrefreshdate": int(time.time() * 1000),
            "msisdn": ""
        }
    }
    raw_json = json.dumps(payload, separators=(',', ':')).encode("utf-8")
    return "oauth " + base64.b64encode(raw_json).decode("utf-8")

s = requests.Session()
s.headers.update({
    "User-Agent": "omh android client 31.1.2026011416.0",
    "Accept": "application/json, text/plain, */*",
    "X-deviceid": device_id,
    "X-devicename": device_name,
    "Authorization": build_oauth_auth_header(access_token, refresh_token)
})

# Test 1: Register device
url_reg = "https://micloud.movistar.es/sapi/profile/device?action=add&responsetime=true"
payload = {
    "data": {
        "carrierid": "10019",
        "modelid": "3482",
        "countrya2": "ES",
        "platform": "android",
        "name": device_name,
        "deviceid": device_id
    }
}
r1 = s.post(url_reg, json=payload, headers={"Content-Type": "application/json;charset=UTF-8"})
print("Test 1 (register_device):", r1.status_code, r1.text[:300])

# Test 2: media/folder/root?action=get
r2 = s.get("https://micloud.movistar.es/sapi/media/folder/root?action=get")
print("Test 2 (folder/root):", r2.status_code, r2.text[:300])

# Test 3: user profile
r3 = s.get("https://micloud.movistar.es/sapi/profile/user?action=get")
print("Test 3 (profile/user):", r3.status_code, r3.text[:300])
