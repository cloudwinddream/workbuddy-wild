# -*- coding: utf-8 -*-
"""查证：account1 的 9095 是账号自身已签，还是设备被占。"""
import json, os, base64, urllib.request, urllib.error, ctypes, ctypes.wintypes as wt

class B(ctypes.Structure):
    _fields_ = [("cbData", wt.DWORD), ("pbData", ctypes.POINTER(ctypes.c_char))]

def dec(s):
    raw = base64.b64decode(s)
    bi = B(len(raw), ctypes.cast(ctypes.create_string_buffer(raw), ctypes.POINTER(ctypes.c_char)))
    bo = B()
    if not ctypes.windll.crypt32.CryptUnprotectData(ctypes.byref(bi), None, None, None, None, 0, ctypes.byref(bo)):
        raise RuntimeError('dpapi')
    o = ctypes.string_at(bo.pbData, bo.cbData)
    ctypes.windll.kernel32.LocalFree(bo.pbData)
    return o.decode('utf-8', 'ignore')

AUTHS = r'D:\Program Files\workbuddy-wild\auths'
CLAIM = 'https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim'
ST = 'https://api.trae.cn/trae/api/v2/ug/checkin_credits/status'
REQ = b'{"req_source":1}'

def post(u, tok, dev, body=b'{}'):
    r = urllib.request.Request(u, data=body, method='POST')
    for k, v in [('Content-Type', 'application/json'), ('User-Agent', 'Trae/0.1.43'),
                 ('Authorization', 'Cloud-IDE-JWT ' + tok), ('X-User-Region', 'CN')]:
        r.add_header(k, v)
    if dev:
        r.add_header('X-Device-Id', dev)
    try:
        with urllib.request.urlopen(r, timeout=25) as x:
            return x.read().decode('utf-8', 'ignore')
    except urllib.error.HTTPError as e:
        return e.read().decode('utf-8', 'ignore')
    except Exception as e:
        return repr(e)

for fn in ['trae-1554031214073648.json', 'trae-2222575719809915.json']:
    doc = json.load(open(os.path.join(AUTHS, fn), encoding='utf-8'))
    at = doc['auth']['accessToken']
    tok = dec(at.split('dpapi:')[1]) if at.startswith('dpapi:') else at
    dev = doc['auth']['deviceId']
    uid = doc['account']['uid']
    print('=' * 70)
    print(f'账号 {uid}  device={dev}')
    print('  claim (带设备号)  :', post(CLAIM, tok, dev, REQ))
    print('  claim (不带设备号):', post(CLAIM, tok, None, REQ))
    print('  status            :', post(ST, tok, dev))
