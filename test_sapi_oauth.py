import requests

access_token = '5a36f383-f6ce-462e-a47f-f26af186c269'
refresh_token = '79451d2d-8d37-4f64-b855-2038226770a3'

combos = [
    ('web', 'accesstoken', access_token),
    ('web', 'refreshtoken', refresh_token),
    ('web', 'authorizationcode', access_token),
    ('android', 'accesstoken', access_token),
    ('android', 'refreshtoken', refresh_token),
    ('android', 'authorizationcode', access_token),
]

for plat, kt, k in combos:
    url = f'https://micloud.movistar.es/sapi/login/oauth?%61ction=%6cogin&platform={plat}&keytype={kt}&key={k}'
    r = requests.get(url, allow_redirects=False)
    loc = r.headers.get('Location', '')
    dev = r.headers.get('X-device-status', '')
    body = r.text[:150]
    print(f"{plat:7} | {kt:18} -> {r.status_code} | Loc: {loc} | DevStatus: {dev} | Cookies: {r.cookies.get_dict()} | Body: {body}")
