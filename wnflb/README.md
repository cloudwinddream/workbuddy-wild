# 福利吧论坛自动签到（Docker sidecar）

签到脚本来自上游开源项目（本目录的 `wnflb_checkin.py` /
`requirements.txt` 与上游保持**逐字节一致**，方便以后同步更新）：

- https://github.com/fmdxx1991/wnflb-checkin
  （二次开发自 https://github.com/appcctv/wnflb-checkin）

论坛：`https://www.wnflb2023.com/`（Discuz! X3.4），签到走 `fx_checkin` 插件。

## 工作原理

独立容器，与 workbuddy-wild-web 主服务互不干扰：

1. 容器启动后按 `CHECKIN_TIMES`（默认每天 01:00 / 22:00，北京时间）执行签到；
2. 首次用账号密码登录（含新 IP 验证码自动识别，ddddocr 已预装），
   Cookie 存到 `./wnflb-data/cookies.json`，之后优先复用；
3. Cookie 过期自动重新登录；签到成功/失败可走 PushPlus / Server酱推送。

## 配置（`.env`，见仓库根目录 `.env.example`）

```bash
FORUM_USERNAME=你的论坛账号
FORUM_PASSWORD=你的论坛密码
# 可选：
# FORUM_COOKIE="xxx=yyy; ..."     # 直接给 Cookie，优先级高于账号密码
# PUSHPLUS_TOKEN=...               # 微信推送
# SERVERCHAN_KEY=...               # 微信推送
# WNFLB_CHECKIN_TIMES=01:00,22:00  # 签到时刻表
```

> ⚠️ `.env` 已加入 `.gitignore`，**不要**把填了真实账号密码的文件
> push 到公开仓库。

## 常用命令

```bash
# 启动（含主服务一起）
docker compose up -d --build

# 只看签到日志
docker compose logs -f wnflb-checkin

# 手动触发一次签到（不等定时）
docker compose exec wnflb-checkin python3 /app/wnflb_checkin.py

# 只重建这个服务
docker compose up -d --build wnflb-checkin
```

## 同步上游更新

```bash
cd /tmp && git clone --depth 1 https://github.com/fmdxx1991/wnflb-checkin.git wnflb-up
cp wnflb-up/wnflb_checkin.py wnflb-up/requirements.txt <本仓库>/wnflb/
docker compose up -d --build wnflb-checkin
```
