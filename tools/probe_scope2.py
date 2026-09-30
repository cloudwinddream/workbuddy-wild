# -*- coding: utf-8 -*-
"""交叉实验：区分「账号级」与「设备级」去重。

已知：
  账号1 + 真实设备号 -> 9095
  账号1 + 随机设备号 -> 9095   ← 换号也一样！
  账号2 + 真实设备号 -> 成功

关键推断：账号1 换任意设备号都报 9095
  => 说明 9095 是**账号级**的"今天已签过"，跟设备号无关！

那账号1 为什么"已签过"却没有任何新增额度包？
  -> 说明上游记录了"账号1 今天签过"，但那次签到没发额度（或发在别处/已过期）。

再验证一次账号2 换号是否仍成功（它今天真领到过）。
"""
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

AUTHS = r'D:/Program Files\workbuddy-wild\auths'
CLAIM = 'https://api.trae.cn/trae/api/v2/ug/checkin_credits/claim'
ST = 'https://api.trae.cn/trae/api/v2/ug/checkin_credits/status'
REQ = b'{"req_source":1}'

def call(u, tok, dev, body=b'{}'):
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

print('=== 矩阵：账号 × 设备号 ===')
print()
for fn, label in [('trae-2222575719809915.json', '账号2(已领100)'),
                  ('trae-1554031214073648.json', '账号1(未领)')]:
    doc = json.load(open(os.path.join(AUTHS, fn), encoding='utf-8'))
    at = doc['auth']['accessToken']
    tok = dec(at.split('dpapi:')[1]) if at.startswith('dpapi:') else at
    orig = doc['auth']['deviceId']
    print(f'{label}  {doc["account"]["uid"]}')
    for dl, dv in [('原设备号', orig),
                   ('随机16位', '1234567890123456'),
                   ('另一个随机', '9999888877776666')]:
        c = call(CLAIM, tok, dv, REQ)
        s = json.loads(call(ST, tok, dv))
        print(f'    {dl:10s} claim-> {c[:60]}')
        print(f'               status: did_checked_in={s.get("did_checked_in")}')
    print()
