# -*- coding: utf-8 -*-
"""决定性实验：用【一个新设备号】给【已签的账号2】签，看能否再拿 100 分。

推理：
  若去重是「账号+设备」级 -> 换新设备号应当能再领一次
  若去重是「账号」级     -> 换设备号也没用（但那样账号1 也该能签，矛盾）

同时用【已签的账号2】+【它的原设备号】做对照。

⚠️ 注意：本实验需要"新的、服务端认可的设备号"，而新号必须是真实注册的。
   本机只有一个真实号，所以这里只能验证"字面行为"，不能造出真号。
   因此改为验证更关键的一点：**设备号不同是否改变结果**。
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
REQ = b'{"req_source":1}'

def claim(tok, dev):
    r = urllib.request.Request(CLAIM, data=REQ, method='POST')
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

# 用账号1（今天没领到的那个）测试：换设备号能否签到
doc = json.load(open(os.path.join(AUTHS, 'trae-1554031214073648.json'), encoding='utf-8'))
at = doc['auth']['accessToken']
tok = dec(at.split('dpapi:')[1]) if at.startswith('dpapi:') else at
orig = doc['auth']['deviceId']

print(f'账号1 {doc["account"]["uid"]}（今天没领到）')
print(f'  原设备号 {orig}      -> {claim(tok, orig)}')
print(f'  换一个随机 16 位号   -> {claim(tok, "1234567890123456")}')
print()
print('说明：随机号会被判设备未注册(9074)，所以无法用它验证"换号能否重领"。')
print('     => 要验证设备级独占，必须有两个**真实注册**的设备号。')
