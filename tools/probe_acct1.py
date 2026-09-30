# -*- coding: utf-8 -*-
"""深挖账号1：它今天到底有没有过入账记录？有效期内的包都是什么？"""
import json, os, base64, urllib.request, urllib.error, ctypes, ctypes.wintypes as wt, datetime

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
ENT = 'https://api.trae.cn/trae/api/v2/pay/user_current_entitlement_list'

def post(u, tok, dev):
    r = urllib.request.Request(u, data=b'{}', method='POST')
    for k, v in [('Content-Type', 'application/json'), ('User-Agent', 'Trae/0.1.43'),
                 ('Authorization', 'Cloud-IDE-JWT ' + tok), ('X-User-Region', 'CN')]:
        r.add_header(k, v)
    if dev:
        r.add_header('X-Device-Id', dev)
    with urllib.request.urlopen(r, timeout=25) as x:
        return x.read().decode('utf-8', 'ignore')

doc = json.load(open(os.path.join(AUTHS, 'trae-1554031214073648.json'), encoding='utf-8'))
at = doc['auth']['accessToken']
tok = dec(at.split('dpapi:')[1]) if at.startswith('dpapi:') else at
dev = doc['auth']['deviceId']

d = json.loads(post(ENT, tok, dev))
us = d.get('usage_summary') or {}
print(f'账号1 total={us.get("total_amount")} consumed={us.get("consumed_amount")}')
print()
print('=== 全部权益包（按 start_time 排序）===')
packs = []
for p in d.get('user_entitlement_pack_list') or []:
    bi = p.get('entitlement_base_info') or {}
    q = bi.get('quota') or {}
    u = p.get('usage') or {}
    lim = q.get('credits_limit')
    st = bi.get('start_time')
    et = bi.get('end_time')
    if st:
        packs.append((int(st), lim, et, u.get("credits_amount")))
packs.sort(key=lambda x: x[0])
now = datetime.datetime.now()
for st, lim, et, used in packs:
    s = datetime.datetime.fromtimestamp(st).strftime('%Y-%m-%d %H:%M')
    e = datetime.datetime.fromtimestamp(et).strftime('%Y-%m-%d %H:%M') if et else '?'
    # 标出今天/昨天新建的
    age = (now - datetime.datetime.fromtimestamp(st)).total_seconds() / 3600
    mark = '  ★今天' if age < 24 else ('  昨天' if age < 48 else '')
    print(f'  start={s}  end={e}  limit={lim}  used={used}{mark}')
print()
print(f'共 {len(packs)} 个包')
