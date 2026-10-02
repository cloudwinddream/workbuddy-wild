#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
福利吧论坛 (wnflb2023.com) 自动签到脚本
=======================================
基于 Discuz! X3.4，支持两种认证方式：
  1. 账号密码登录（自动处理验证码，ddddocr 识别）
  2. 直接传入 Cookie（兼容旧方式）

特性：
  - 登录成功后把 Cookie 保存到本地/cache，下次优先复用
  - Cookie 失效自动重新用账号密码登录（含验证码）
  - 新 IP 登录需要验证码时自动拉取并识别（ddddocr）
  - 支持 PushPlus / Server 酱 微信推送
  - 论坛为 GBK 编码，已做兼容

环境变量：
  FORUM_USERNAME   账号
  FORUM_PASSWORD   密码
  FORUM_COOKIE     直接传入的 Cookie 字符串（优先级高于 cookie 文件）
  COOKIE_FILE      Cookie 缓存文件路径（默认 cookies.json）
  PUSHPLUS_TOKEN   PushPlus 推送 token
  SERVERCHAN_KEY   Server 酱推送 key
"""

import argparse
import json
import os
import re
import sys
import time
import urllib.parse
from datetime import datetime, timezone, timedelta

import requests

# ========================= 配置 =========================
BASE_URL = "https://www.wnflb2023.com"
FORUM_URL = BASE_URL + "/forum.php"
LOGIN_PAGE_URL = BASE_URL + "/member.php?mod=logging&action=login"
TIMEOUT = 30
MAX_RETRIES = 3
RETRY_DELAY = 5

HEADERS = {
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
        "AppleWebKit/537.36 (KHTML, like Gecko) "
        "Chrome/120.0.0.0 Safari/537.36"
    ),
    "Accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    "Accept-Language": "zh-CN,zh;q=0.9,en;q=0.8",
}


# ========================= 工具函数 =========================

def get_cst_time():
    utc_now = datetime.now(timezone.utc)
    return (utc_now + timedelta(hours=8)).strftime("%Y-%m-%d %H:%M:%S")


def parse_cookies(raw):
    """Cookie 字符串 -> 字典"""
    cookies = {}
    for item in raw.split(";"):
        item = item.strip()
        if "=" in item:
            k, v = item.split("=", 1)
            cookies[k.strip()] = v.strip()
    return cookies


def get_page_text(resp):
    """优先按 GBK 解码（论坛是 GBK）"""
    if resp.encoding and resp.encoding.lower() in ("gbk", "gb2312", "gb18030"):
        return resp.text
    try:
        return resp.content.decode("gbk")
    except (UnicodeDecodeError, LookupError):
        resp.encoding = resp.apparent_encoding or "utf-8"
        return resp.text


def fetch_forum(session):
    for attempt in range(1, MAX_RETRIES + 1):
        try:
            resp = session.get(FORUM_URL, timeout=TIMEOUT)
            return resp
        except requests.RequestException as e:
            print(f"  [网络] 第 {attempt}/{MAX_RETRIES} 次请求失败: {e}")
            if attempt < MAX_RETRIES:
                time.sleep(RETRY_DELAY)
    return None


# ========================= Cookie 读写 =========================

def load_cookies(session, raw_cookie, cookie_file):
    """把 cookie 载入 session。返回是否成功载入。"""
    if raw_cookie:
        session.cookies.update(parse_cookies(raw_cookie))
        return True
    if cookie_file and os.path.exists(cookie_file):
        try:
            with open(cookie_file, "r", encoding="utf-8") as f:
                data = json.load(f)
            if isinstance(data, dict) and data:
                session.cookies.update(data)
                return True
        except Exception as e:
            print(f"  [Cookie] 读取缓存失败: {e}")
    return False


def save_cookies(session, cookie_file):
    if not cookie_file:
        return
    try:
        data = {c.name: c.value for c in session.cookies}
        with open(cookie_file, "w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False, indent=2)
        print(f"  [Cookie] 已保存到 {cookie_file}")
    except Exception as e:
        print(f"  [Cookie] 保存失败: {e}")


def verify_login(session):
    """访问论坛首页，判断是否已登录。返回 (bool, html)"""
    resp = fetch_forum(session)
    if resp is None:
        return False, ""
    html = get_page_text(resp)
    logged = check_logged_in(html)
    return logged, html


def check_logged_in(html):
    """
    检测页面是否处于登录态。
    最可靠信号：页面 JS 里的 discuz_uid（游客为 '0'，登录后为真实 UID）。
    用它能避免把访客页里的 fx_checkin 模块误判成已登录。
    """
    m = re.search(r"discuz_uid\s*=\s*'(\d+)'", html)
    if m:
        return m.group(1) != "0"
    # 兜底：有登出链接视为已登录
    if 'class="logout"' in html or "mod=logging&action=logout" in html:
        return True
    # 兜底：出现登录表单视为未登录
    if 'name="username"' in html and 'name="password"' in html:
        return False
    return False


def extract_message(html):
    """提取 Discuz 提示信息页的正文（登录失败/成功提示、签到提示等）。"""
    # 方式1: <div id="messagetext"> ... </div>
    m = re.search(r'id="messagetext"[^>]*>(.*?)</div>\s*</div>', html, re.DOTALL)
    if m:
        t = re.sub(r"<[^>]+>", "", m.group(1)).strip()
        if t:
            return t
    # 方式2: 通用 alert 文案 div
    m = re.search(r'class="alert_(?:right|error|info)"[^>]*>(.*?)</div>', html, re.DOTALL)
    if m:
        return re.sub(r"<[^>]+>", "", m.group(1)).strip()
    return ""


# ========================= 登录（账号密码） =========================

def fetch_login_page(session):
    """GET 登录页，返回 (html, formhash, loginhash)"""
    resp = session.get(LOGIN_PAGE_URL, timeout=TIMEOUT)
    html = get_page_text(resp)
    formhash_m = re.search(r'name="formhash"\s+value="([a-f0-9]+)"', html)
    formhash = formhash_m.group(1) if formhash_m else None
    # loginhash 在表单 action 的 URL 里
    lh_m = re.search(r"loginhash=([A-Za-z0-9]+)", html)
    loginhash = lh_m.group(1) if lh_m else None
    return html, formhash, loginhash


def detect_captcha(html):
    """
    识别登录页/挑战页是否需要验证码，并提取关键参数。
    支持两种形态：
      A. 标准：misc.php?mod=seccode&update=<rand>&idhash=<hash>
      B. 二次挑战（新 IP）：响应带 name="auth" 令牌
         + updateseccode('IDHASH', ...) / <span id="seccode_IDHASH">
    返回 dict: {needed, idhash, update, seccodehash, auth}
    """
    res = {
        "needed": False,
        "idhash": "",
        "update": str(int(time.time() * 1000)),
        "seccodehash": "",
        "auth": "",
    }

    # auth 令牌（二次挑战，必须随登录一起提交）
    am = re.search(r'name="auth"\s+value="([A-Za-z0-9%_./=+]+)"', html)
    if am:
        res["auth"] = am.group(1)

    # idhash：优先 updateseccode('IDHASH', ...) 或 <span id="seccode_IDHASH">
    ih = re.search(r"updateseccode\(\s*['\"]([A-Za-z0-9]+)['\"]", html)
    if not ih:
        ih = re.search(r'id="seccode_([A-Za-z0-9]+)"', html)
    if not ih:
        sm = re.search(
            r"misc\.php\?mod=seccode&update=([^&\"']+)&idhash=([A-Za-z0-9]+)", html
        )
        if sm:
            res["idhash"] = sm.group(2)
            res["update"] = sm.group(1)
    if ih and not res["idhash"]:
        res["idhash"] = ih.group(1)

    # 输入框 id 形如 seccodeverify_<idhash>
    if not res["idhash"]:
        sid = re.search(r'id="seccodeverify_([A-Za-z0-9]+)"', html)
        if sid:
            res["idhash"] = sid.group(1)
    # 隐藏域 seccodehash
    if not res["idhash"]:
        sh = re.search(r'name="seccodehash"\s+value="([A-Za-z0-9]+)"', html)
        if sh:
            res["idhash"] = sh.group(1)

    if not res["idhash"] and re.search(r'name="seccodeverify"', html):
        res["idhash"] = "SkyV"  # 极端兜底

    if res["idhash"] or res["auth"]:
        res["needed"] = True

    if res["idhash"]:
        sh = re.search(r'name="seccodehash"\s+value="([A-Za-z0-9]+)"', html)
        res["seccodehash"] = sh.group(1) if sh else res["idhash"]

    return res


def extract_login_fields(html):
    """从登录/挑战页提取 formhash、loginhash、auth（用于二次提交）。"""
    fh = re.search(r'name="formhash"\s+value="([a-f0-9]+)"', html)
    lh = re.search(r"loginhash=([A-Za-z0-9]+)", html)
    auth = re.search(r'name="auth"\s+value="([A-Za-z0-9%_]+)"', html)
    return (
        fh.group(1) if fh else None,
        lh.group(1) if lh else None,
        auth.group(1) if auth else None,
    )


def solve_captcha(session, cap):
    """
    拉取验证码图片并用 ddddocr 识别。
    多次重试（每次换一张新图），提高识别率。
    返回识别出的验证码字符串，失败返回 None。
    """
    try:
        import ddddocr  # 仅在需要验证码时才 import
    except ImportError:
        print("  [验证码] 未安装 ddddocr，请先 pip install ddddocr")
        return None

    ocr = ddddocr.DdddOcr(show_ad=False)
    # 论坛/WAF 对 seccode 图片接口要求同源 Referer，否则返回 Access Denied
    headers = {"Referer": LOGIN_PAGE_URL}
    for attempt in range(1, 4):
        try:
            update = str(int(time.time() * 1000))
            img_url = (
                f"{BASE_URL}/misc.php?mod=seccode"
                f"&update={update}&idhash={cap['idhash']}"
            )
            r = session.get(img_url, timeout=TIMEOUT, headers=headers)
            # 校验确实是图片（PNG/JPEG 魔数），避免把 Access Denied 当图片识别
            if r.status_code != 200 or len(r.content) < 100:
                print(f"  [验证码] 第 {attempt} 次拉取图片失败(status={r.status_code}, len={len(r.content)})")
                continue
            if r.content[:4] not in (b"\x89PNG", b"\xff\xd8\xff\xe0", b"\xff\xd8\xff\xe1"):
                print(f"  [验证码] 第 {attempt} 次返回非图片(可能被拦截), len={len(r.content)}")
                continue
            code = ocr.classification(r.content).strip()
            if code:
                return code
        except Exception as e:
            print(f"  [验证码] 第 {attempt} 次识别异常: {e}")
    return None


def verify_captcha_code(session, cap, code):
    """
    调用 Discuz 验证码校验接口（action=check）。
    浏览器在最终提交登录前会先发这个请求：它会校验验证码是否正确，
    并在 cookie 里写入 seccode<idhash> 标记（登录提交时服务端据此判定验证码已通过）。
    返回 True/False，仅作提示用，不阻断后续登录提交。
    """
    url = (
        f"{BASE_URL}/misc.php?mod=seccode&action=check&inajax=1"
        f"&modid=member::logging&idhash={cap['idhash']}&secverify={code}"
    )
    try:
        r = session.get(
            url,
            timeout=TIMEOUT,
            headers={
                "Referer": LOGIN_PAGE_URL,
                "X-Requested-With": "XMLHttpRequest",
            },
        )
        txt = get_page_text(r)
        # 论坛返回 <root><![CDATA[succeed]]></root> 表示验证码正确
        ok = "succeed" in txt
        return ok
    except Exception as e:
        print(f"  [验证码] 校验接口异常: {e}")
        return False


def _submit_login(session, formhash, loginhash, username="", password="",
                  seccode="", auth="", seccodehash="", challenge=False):
    """
    执行一次登录 POST。返回 (ok, msg, resp_html)。

    challenge=True 时按 Discuz「二次验证码挑战」提交：
      - 凭据已由第一次提交的 auth 令牌在服务端关联，无需重复发送账号密码
      - 字段集对齐浏览器真实成功包：formhash/referer/auth/questionid/answer/
        seccodehash/seccodemodid=member::logging/seccodeverify
      - 登录 URL 带 &inajax=1
      - auth 调用方需先用 urllib.parse.unquote 还原，再由本函数统一按
        form-urlencoded 编码（与浏览器/真实 curl 字节级一致）
    """
    if challenge:
        data = {
            "formhash": formhash,
            "referer": BASE_URL + "/",
            "auth": auth,
            "questionid": "0",
            "answer": "",
            "seccodehash": seccodehash or "",
            "seccodemodid": "member::logging",
            "seccodeverify": seccode,
        }
    else:
        data = {
            "formhash": formhash,
            "referer": BASE_URL + "/",
            "loginfield": "username",
            "username": username,
            "password": password,
            "questionid": "0",
            "answer": "",
            "cookietime": "2592000",
        }
        if seccode:
            data["seccodeverify"] = seccode
            if seccodehash:
                data["seccodehash"] = seccodehash

    login_url = (
        f"{BASE_URL}/member.php?mod=logging&action=login"
        f"&loginsubmit=yes&loginhash={loginhash}"
    )
    if challenge:
        login_url += "&inajax=1"
    try:
        r = session.post(
            login_url,
            data=data,
            timeout=TIMEOUT,
            allow_redirects=True,
            headers={"Referer": LOGIN_PAGE_URL},
        )
    except requests.RequestException as e:
        return False, f"登录请求异常: {e}", ""

    txt = get_page_text(r)
    msg = extract_message(txt)
    if not msg:
        # inajax=1 时消息在 CDATA 里
        cdata = re.search(r"<!\[CDATA\[(.*?)\]\]>", txt, re.DOTALL)
        if cdata:
            msg = re.sub(r"<[^>]+>", "", cdata.group(1)).strip()

    # 仍被要求验证码（验证码错误等）
    if "请输入验证码" in txt and "auth=" in txt:
        return False, (msg or "验证码不正确，请重试"), txt

    # 权威校验：访问首页看 discuz_uid 是否为真实 UID
    logged, _ = verify_login(session)
    if logged:
        # 成功页提示区含内联 JS，extract_message 会抽到脏文本，统一用干净文案
        return True, "登录成功", txt
    # 密码/用户名错误等明确失败
    if msg and ("密码" in msg or "用户名" in msg):
        return False, f"登录失败: {msg}", txt
    return False, (msg or "登录失败（未进入登录态）"), txt


def do_login(session, username, password):
    """
    账号密码登录（含新 IP 二次验证码挑战）。
    成功返回 (True, 消息)，失败返回 (False, 消息)。
    """
    print("  [登录] 获取登录页 ...")
    html, formhash, loginhash = fetch_login_page(session)
    if not formhash or not loginhash:
        return False, "无法解析登录页(formhash/loginhash 缺失)"

    # 首次尝试：无验证码直接提交（老 IP / 已信任环境通常直接成功）
    ok, msg, resp_html = _submit_login(
        session, formhash, loginhash, username, password, None, None
    )
    if ok:
        return True, "登录成功"

    # 被验证码挑战：从第一次提交的响应里直接解析挑战页（auth 由本次响应给出）。
    # 关键：auth 在 HTML 里可能以 %2F 形式存在（含 /），需先 unquote 还原，
    # 再交给 _submit_login 统一按 form-urlencoded 编码，否则会被二次编码导致
    # 服务端认不出令牌、报“密码空或包含非法字符”。
    chtml = resp_html or ""
    c_fh, c_lh, auth = extract_login_fields(chtml)
    auth = urllib.parse.unquote(auth) if auth else None
    if not auth:
        am = re.search(r"auth=([A-Za-z0-9%_./=+]+)", chtml)
        auth = urllib.parse.unquote(am.group(1)) if am else None
    if not auth:
        return False, msg  # 真失败（密码错等），msg 已是原因

    # 若挑战页字段没解析全，带 auth 重新拉一次，并从页面里取最新的 auth（同样 unquote）
    cap = detect_captcha(chtml)
    if not (c_fh and c_lh and cap["needed"] and cap["idhash"]):
        print("  [登录] 触发验证码挑战，重新获取挑战页 ...")
        try:
            r = session.get(
                f"{BASE_URL}/member.php",
                params={"mod": "logging", "action": "login", "auth": auth},
                timeout=TIMEOUT,
                headers={"Referer": LOGIN_PAGE_URL},
            )
            chtml = get_page_text(r)
            c_fh, c_lh, c_auth = extract_login_fields(chtml)
            auth = urllib.parse.unquote(c_auth) if c_auth else auth
            cap = detect_captcha(chtml)
        except requests.RequestException as e:
            return False, f"获取挑战页异常: {e}"

    if not (c_fh and c_lh):
        return False, "验证码挑战页未解析出 formhash/loginhash"
    if not cap["needed"] or not cap["idhash"]:
        return False, f"验证码挑战页未解析出验证码(idhash 缺失): {msg}"

    # 二次挑战提交（不带账号密码，凭据由 auth 关联）。最多 3 次换图重试；
    # 若仍报凭据缺失（auth 失效 / 编码问题），则重拉挑战页拿新 auth 再试。
    last_msg = "验证码识别失败"
    for attempt in range(1, 4):
        print(f"  [登录] 验证码 idhash={cap['idhash']}，ddddocr 识别中(第{attempt}次) ...")
        code = solve_captcha(session, cap)
        if not code:
            return False, "验证码识别失败，请检查 ddddocr 或手动处理"

        # 调用验证码校验接口（设置 seccode cookie，确认识别是否正确）
        if not verify_captcha_code(session, cap, code):
            print(f"  [登录] 第 {attempt} 次验证码校验未通过，换新图重试 ...")
            continue

        ok2, last_msg, _ = _submit_login(
            session, c_fh, c_lh, username, password, code, auth,
            cap["seccodehash"], challenge=True,
        )
        if ok2:
            return True, "登录成功(已通过验证码)"
        # 验证码不正确则换新图重试
        if "验证码" in last_msg and ("不正确" in last_msg or "错误" in last_msg):
            print(f"  [登录] 第 {attempt} 次验证码不正确，换新图重试 ...")
            continue
        # 凭据缺失（auth 失效）：重拉挑战页获取新 auth 重试一次
        if "密码" in last_msg or "用户名" in last_msg:
            print(f"  [登录] 第 {attempt} 次疑似凭据缺失，重拉挑战页重试 ...")
            try:
                r = session.get(
                    f"{BASE_URL}/member.php",
                    params={"mod": "logging", "action": "login", "auth": auth},
                    timeout=TIMEOUT, headers={"Referer": LOGIN_PAGE_URL},
                )
                chtml = get_page_text(r)
                c_fh, c_lh, c_auth = extract_login_fields(chtml)
                auth = urllib.parse.unquote(c_auth) if c_auth else auth
                cap = detect_captcha(chtml)
            except requests.RequestException:
                pass
            continue
        # 其他失败不再重试
        return False, last_msg
    return False, last_msg


# ========================= 签到 =========================

def check_already_signed(html):
    m = re.search(r"fx_chk_menu\s*=\s*(true|false)", html)
    if m:
        return m.group(1) == "true"
    return False


def extract_formhash(html):
    m = re.search(r"fx_checkin:checkin&formhash=([a-f0-9]+)&([a-f0-9]+)", html)
    if m:
        return m.group(1), m.group(2)
    return None, None


def do_checkin(session, formhash, fx_formhash):
    url = (
        f"{BASE_URL}/plugin.php?id=fx_checkin:checkin"
        f"&formhash={formhash}&{fx_formhash}&inajax=1"
    )
    headers = {
        "Referer": FORUM_URL,
        "X-Requested-With": "XMLHttpRequest",
    }
    for attempt in range(1, MAX_RETRIES + 1):
        try:
            resp = session.get(url, timeout=TIMEOUT, headers=headers)
            return resp.text
        except requests.RequestException as e:
            print(f"  [签到] 第 {attempt}/{MAX_RETRIES} 次请求失败: {e}")
            if attempt < MAX_RETRIES:
                time.sleep(RETRY_DELAY)
    return None


def parse_result(text):
    if text is None:
        return False, "网络请求失败"
    cdata = re.search(r"<!\[CDATA\[(.*?)\]\]>", text, re.DOTALL)
    content = cdata.group(1) if cdata else text
    clean = re.sub(r"<[^>]+>", " ", content).strip()
    clean = re.sub(r"\s+", " ", clean)

    if "签到成功" in clean:
        rank = re.search(r"第\s*(\d+)\s*个", clean)
        if rank:
            return True, f"签到成功！今日第 {rank.group(1)} 个签到"
        return True, "签到成功！"
    if "已经签到" in clean or "已签到" in clean:
        return True, "今日已签到（重复签到）"
    if "先登录" in clean or "请登录" in clean:
        return False, "Cookie 已过期，请重新获取"
    if "补签" in clean and "成功" in clean:
        return True, "补签成功"
    return False, f"未知响应: {clean[:200]}"


# ========================= 通知 =========================

def send_notification(title, content):
    token = os.environ.get("PUSHPLUS_TOKEN", "")
    if token:
        try:
            resp = requests.post(
                "http://www.pushplus.plus/send",
                json={"token": token, "title": title, "content": content, "template": "txt"},
                timeout=10,
            )
            print(f"  [PushPlus] {resp.json().get('msg', 'unknown')}")
        except Exception as e:
            print(f"  [PushPlus] 发送失败: {e}")

    key = os.environ.get("SERVERCHAN_KEY", "")
    if key:
        try:
            resp = requests.post(
                f"https://sctapi.ftqq.com/{key}.send",
                data={"title": title, "desp": content},
                timeout=10,
            )
            print(f"  [Server酱] {resp.json().get('message', 'unknown')}")
        except Exception as e:
            print(f"  [Server酱] 发送失败: {e}")

    if not token and not key:
        print("  (未配置推送通知)")


# ========================= 调试：解析登录页 =========================

def inspect_login():
    """仅拉取登录页并打印解析结果，用于排查（无需账号密码）。"""
    session = requests.Session()
    session.headers.update(HEADERS)
    html, formhash, loginhash = fetch_login_page(session)
    cap = detect_captcha(html)
    print("=== 登录页解析结果 ===")
    print(f"  可访问: {bool(html)}")
    print(f"  formhash : {formhash}")
    print(f"  loginhash: {loginhash}")
    print(f"  需要验证码: {cap['needed']}")
    if cap["needed"]:
        print(f"  idhash   : {cap['idhash']}")
        print(f"  seccodehash: {cap['seccodehash']}")
    print("======================")


def mark_refreshed():
    """重新登录成功并已写盘时，向 GitHub Actions 输出 refreshed=true，
    通知 workflow 删除旧缓存并保存新 Cookie（避免缓存累积 / 过期不更新）。"""
    p = os.environ.get("GITHUB_OUTPUT")
    if p:
        try:
            with open(p, "a") as f:
                f.write("refreshed=true\n")
        except OSError:
            pass


# ========================= 主流程 =========================

def main():
    parser = argparse.ArgumentParser(description="福利吧论坛自动签到")
    parser.add_argument("--username", default=os.environ.get("FORUM_USERNAME", ""))
    parser.add_argument("--password", default=os.environ.get("FORUM_PASSWORD", ""))
    parser.add_argument("--cookie", default=os.environ.get("FORUM_COOKIE", ""),
                        help="直接传入 Cookie 字符串")
    parser.add_argument("--cookie-file", default=os.environ.get("COOKIE_FILE", "cookies.json"),
                        help="Cookie 缓存文件路径（默认 cookies.json）")
    parser.add_argument("--no-save", action="store_true", help="不保存 Cookie")
    parser.add_argument("--mode", default="万能福利吧签到", help="签到模式标签(万能福利吧签到/万能福利吧补签)")
    parser.add_argument("--inspect", action="store_true",
                        help="仅解析登录页并打印结果后退出")
    args = parser.parse_args()

    if args.inspect:
        inspect_login()
        return

    now = get_cst_time()
    print("=" * 50)
    print("  福利吧论坛自动签到")
    print(f"  模式: {args.mode}   时间: {now}")
    print("=" * 50)

    session = requests.Session()
    session.headers.update(HEADERS)

    html = None
    # 1) 尝试用缓存/Cookie 直接登录
    if load_cookies(session, args.cookie, args.cookie_file):
        print("[1] 已载入 Cookie，校验登录态 ...")
        logged, html = verify_login(session)
        if logged:
            print("  -> Cookie 有效，直接签到")
        else:
            print("  -> Cookie 已过期")
            html = None
    else:
        print("[1] 未找到可用 Cookie")

    # 2) Cookie 不可用 -> 账号密码登录
    if html is None:
        if not (args.username and args.password):
            msg = "Cookie 无效，且未提供账号密码(FORUM_USERNAME/FORUM_PASSWORD)"
            print(f"[FAIL] {msg}")
            send_notification("[签到失败] 需登录", f"时间:{now}\n错误:{msg}")
            sys.exit(1)
        print("[2] 使用账号密码登录 ...")
        ok, msg = do_login(session, args.username, args.password)
        if not ok:
            print(f"[FAIL] 登录失败: {msg}")
            send_notification("[签到失败] 登录失败", f"时间:{now}\n错误:{msg}")
            sys.exit(1)
        print(f"  -> {msg}")
        if not args.no_save:
            save_cookies(session, args.cookie_file)
            mark_refreshed()
        logged, html = verify_login(session)
        if not logged:
            print("[FAIL] 登录后首页校验未通过")
            sys.exit(1)

    # 3) 签到
    print("[3] 检查签到状态 ...")
    if check_already_signed(html):
        print("[OK] 今日已签到，无需重复操作")
        send_notification(f"[签到成功] {args.mode}",
                          f"时间:{now}\n状态:今日已签到")
        sys.exit(0)
    print("  -> 今日尚未签到，执行签到 ...")

    formhash, fx_formhash = extract_formhash(html)
    if not formhash:
        msg = "无法提取 formhash，页面结构可能已变化"
        print(f"[FAIL] {msg}")
        send_notification(f"[签到失败] {args.mode}", f"时间:{now}\n错误:{msg}")
        sys.exit(1)

    text = do_checkin(session, formhash, fx_formhash)
    success, message = parse_result(text)
    if success:
        print(f"[OK] {message}")
        send_notification(f"[签到成功] {args.mode}",
                          f"时间:{now}\n结果:{message}")
    else:
        print(f"[FAIL] {message}")
        send_notification(f"[签到失败] {args.mode}",
                          f"时间:{now}\n结果:{message}")
        sys.exit(1)


if __name__ == "__main__":
    main()
