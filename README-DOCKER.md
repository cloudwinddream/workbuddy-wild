# WorkBuddy-Wild Web 版（Docker 部署说明）

桌面版（Windows 托盘 exe）的**无头 Web 版**：同一个 Go 后端（双平台账号池、
自动签到调度器、OpenAI 兼容 API），管理面板从托盘窗口换成**浏览器页面**，
可部署在服务器上 7×24 常驻，偶尔打开网页检查即可。

- 管理页：`http://<服务器IP>:7863/`
- OpenAI 兼容 API：`http://<服务器IP>:7863/v1`（`workbuddy/<模型>` / `traework/<模型>`）
- 健康检查：`GET /healthz` → `ok`

> 与仓库根目录旧的 `docker/` 目录的区别：旧目录是 v0.3.0 单平台时代的遗留
> （预编译二进制已过时，不支持 TraeWork）；本 Web 版基于当前 master
> （v0.6.9+）构建，双平台、签到逻辑都是最新的。

## 一、部署步骤

```bash
# 1. 克隆 fork 仓库
git clone https://github.com/cloudwinddream/workbuddy-wild.git
cd workbuddy-wild

# 2. 改配置
vim docker-compose.yml
# WB2A_API_KEY：换成强随机值；可信内网调试可留空（留空=不鉴权，不再弹登录框）
# WB2A_PUBLIC_URL：浏览器与服务器不在同一台机器时必填，
#                 填浏览器里打开管理页的那个地址，如 http://192.168.1.10:7863
#                 （SSH 端口转发场景可留空）

# 3. 构建并启动
docker compose up -d --build

# 4. 看日志确认
docker compose logs -f workbuddy-wild-web
# 应看到：listening on :7863 / 管理页： http://<本机IP>:7863/

# 5. 健康检查
curl http://127.0.0.1:7863/healthz   # → ok
```

浏览器打开 `http://<服务器IP>:7863/`。若 `WB2A_API_KEY` 设了值，首次会弹框
要求输入 API Key（记在浏览器 sessionStorage，关标签页后需重输）；
若留空则直接进入管理页，不鉴权。

## 二、添加账号（两种方式）

### 方式 A：网页直接登录（推荐）

管理页右上点「＋ WorkBuddy」或「＋ TraeWork」：

1. 后端生成授权链接，前端遮罩层显示倒计时（5 分钟有效）；
   点右上角复制按钮把授权链接复制出来；
2. 在**你自己的浏览器**新标签页打开该链接，完成对应平台登录；
3. 页面自动检测到登录成功 → 凭证写入 `/data/auths/` → 自动签到一次。

注意：

- **WorkBuddy**：授权链接在任何浏览器打开都行，服务端轮询即可确认。
- **TraeWork**：登录成功后，Trae 的授权页会**回调**本服务
  （`http://<回调基址>/api/login/traecb?sid=...`）。回调基址默认
  `http://127.0.0.1:7863`，要求**你的浏览器能直接访问服务端**。
  - 若做了 `ssh -L 7863:127.0.0.1:7863 user@server` 端口转发：浏览器访问
    `http://127.0.0.1:7863/`，回调天然可达，**不用改任何配置**；
  - 若浏览器直接访问服务器 IP（最常见）：**必须**在 `docker-compose.yml`
    里设置 `WB2A_PUBLIC_URL=http://<服务器IP>:7863` 后重建容器，
    否则回调走 `127.0.0.1` 会打到你自己的电脑上，登录永远等不到结果；
  - Trae 是否接受非 localhost 回调地址**尚未用真实账号验证**，
    若登录卡在"等待回调"，请改用方式 B。

### 方式 B：从桌面版复制账号文件

桌面版是最顺手的添加账号方式（无痕浏览器一键登录）。加完后把
`auths/` 目录下的 `workbuddy-*.json` / `trae-*.json` **明文文件**
复制到服务器的 `./data/auths/`，重启容器即加载：

```bash
docker compose restart workbuddy-wild-web
```

> ⚠️ 桌面版 v0.3.0+ 默认用 Windows DPAPI **加密** token 落盘，
> 加密文件（`dpapi:` 前缀）在 Linux 容器里**解不开**，复制过去也用不了。
> 若你的桌面版 auth 文件是加密的，请在桌面版所在机器上先转出明文，
> 或直接用方式 A 在网页上重新登录一遍（推荐）。

## 三、日常使用

| 操作 | 位置 |
| --- | --- |
| 看积分 / 签到状态 / 冷却 | 管理页账号列表（每 1.5s 自动刷新） |
| 手动签到（单个/全部） | 账号卡片 ✓ 按钮 / 顶部"全部签到" |
| 刷新积分 | 顶部"刷新积分" |
| 改签到时间 / API-Key / 监听地址 / 积分策略 | 右侧设置栏（即时生效） |
| 看日志 | 右侧"查看日志"（新标签页打开最近 500 行） |
| 定时签到 | 按签到时间自动执行，结果推送到页面 toast + 日志 |

客户端对接（OpenAI 兼容）：Base URL `http://<服务器IP>:7863/v1`，
API Key 填 `WB2A_API_KEY`，模型名带前缀如 `workbuddy/glm-5.2`、
`traework/kimi-k2.7-code`。

## 四、升级

```bash
git pull
docker compose up -d --build
```

`./data` 卷独立于镜像，账号、状态、日志不受影响。

## 五、配置项（环境变量）

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `WB2A_LISTEN` | `:7863` | 监听地址；容器内保持 `:7863` |
| `WB2A_API_KEY` | `WorkBuddy2API` | Bearer 密钥；**务必修改**；显式设为空字符串=不鉴权（直接进管理页）；变量完全不设置时才回退到默认值 |
| `WB2A_AUTH_DIR` | `/data/auths` | 账号目录 |
| `WB2A_STATE_FILE` | `/data/state.json` | 状态文件（实际落 `state-workbuddy.json` / `state-traework.json`） |
| `WB2A_PUBLIC_URL` | 空 | TraeWork 登录回调基址（见方式 A） |
| `WB2A_STRATEGY` | `credits` | 选号策略：`credits` / `expire` / `roundrobin` |
| `TZ` | `Asia/Shanghai` | 签到时间的时区 |

首次启动没有 `config.json` 时使用"默认配置 + 环境变量"，
**不会**把环境变量烤进文件；之后在页面上改的配置才会写回
`config.json`（容器内为 `/app/config.json`，随容器重建丢失——
持久化配置请继续用环境变量）。

## 六、排错

| 现象 | 排查 |
| --- | --- |
| 容器反复重启 | `docker compose logs` 看 panic/监听失败；确认 7863 未被占用 |
| 管理页 401 反复弹框 | 输入的 Key 与 `WB2A_API_KEY` 不一致；输对一次即可 |
| `/v1/models` 返回空 | `/data/auths` 里没有账号文件，或 region 不匹配；看 `/status` |
| 接口 401 | Bearer 与 `WB2A_API_KEY` 不一致 |
| 模型报 `no_healthy_account` | token 过期/被冷却，看日志 refresh 结果 |
| TraeWork 登录卡在等待回调 | 见"方式 A"注意：回调地址浏览器不可达，或 Trae 不接受该回调地址，改用方式 B |
| 点"复制链接"报错（clipboard 相关） | 旧版本的已知问题；`git pull` 后 `docker compose up -d --build` 重建即修复 |
| 终端中文注释乱码 | 终端未按 UTF-8 解码：`export LANG=C.UTF-8 LC_ALL=C.UTF-8` 后重看 |
| TraeWork 签到 9074 | 设备号校验：服务器上没有 Trae 客户端，只能用随机设备号重试；v0.6.9 已内置最多 8 次换号重试 |

## 七、已知限制（实测前请知悉）

1. **网页登录**：WorkBuddy 的网页登录（复制授权链接 → 浏览器完成登录 →
   服务端轮询确认）已用真实账号验证走通。TraeWork 的授权 URL 生成、
   回调记录、轮询交换链路已做通，但 Trae 是否接受非 localhost 的
   `auth_callback_url`、以及服务器出口 IP 是否被限流，仍需真实账号实测。
2. **TraeWork 自动签到在服务器上依赖随机设备号**：v0.6.9 的换号重试
   （9074 瞬时限流 → 最多换 8 个号重试）在桌面端已验证有效，
   服务端行为一致，但多账号大并发下的长期表现待观察。
3. **DPAPI 加密的旧 auth 文件不可直接迁移**（见方式 B）。
4. `max_rotate` 在页面修改后**需重启容器**才生效（handler 启动时快照）；
   其余配置（签到时间、API-Key、策略、监听地址）即时生效。
5. 管理页是"桌面版前端 + 兼容垫片"，首次加载需请求 `/shim.js`；
   离线内网使用无影响（全部资源同源，无外部 CDN）。

## 八、签到中心

主服务内置**签到中心**：所有第三方自动签到统一在一个页面管理——
`http://<服务器IP>:7863/checkin/`（主管理页右下角也有"签到中心"悬浮入口）。
每个签到项显示：每天自动签到是否成功、上次签到时间、下次签到时间、
**对应积分**，并支持手动"立即签到"。新的签到项以后直接加进来，
不用再开新页面。

当前已接入：**福利吧论坛**（`wnflb2023.com`，Discuz! X3.4 + `fx_checkin` 插件）。
协议逆向来自上游开源项目 https://github.com/fmdxx1991/wnflb-checkin，
已完整移植为 Go 原生实现（`internal/wnflb`）。

```bash
# 账号密码填在 .env 里（容器首次启动自动导入；也可在签到中心页面直接登录）
cp .env.example .env
vim .env   # 填 FORUM_USERNAME / FORUM_PASSWORD
docker compose up -d --build
```

说明：

- 默认每天 **01:00 / 22:00**（北京时间）自动签到，`WNFLB_CHECKIN_TIMES` 可改；
  启动后默认先跑一次（`WNFLB_RUN_ON_STARTUP=false` 可关）。
- 登录态（Cookie）持久化在 `./data/wnflb/`，过期自动用存档账号重登。
- 新 IP 首次登录若触发论坛验证码，页面会弹出验证码图片，手动输入即可。
- 账号密码明文存于 `./data/wnflb/account.json`（0600 权限），与本项目
  auth 文件策略一致；`.env` 不会被 git 提交。
