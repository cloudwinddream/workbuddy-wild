#!/bin/bash
# wnflb-checkin 定时调度入口
#
# 环境变量：
#   CHECKIN_TIMES   每天执行时刻（北京时间），逗号分隔，如 "01:00,22:00"
#   RUN_ON_STARTUP  启动后是否先跑一次（true/false），默认 true
#   COOKIE_FILE     Cookie 缓存路径，默认 /app/data/cookies.json
#
# 账号密码由上游脚本直接读 FORUM_USERNAME / FORUM_PASSWORD / FORUM_COOKIE。

set -u

TIMES="${CHECKIN_TIMES:-01:00,22:00}"
export COOKIE_FILE="${COOKIE_FILE:-/app/data/cookies.json}"
export TZ="${TZ:-Asia/Shanghai}"

SLEEP_PID=""

# 优雅退出：docker stop 发 SIGTERM 时能立即结束 sleep
trap 'echo "[wnflb] 收到停止信号，退出"; [ -n "$SLEEP_PID" ] && kill "$SLEEP_PID" 2>/dev/null; exit 0' TERM INT

run_once() {
  echo "[wnflb] $(date '+%F %T') 开始执行签到"
  if python3 /app/wnflb_checkin.py; then
    echo "[wnflb] $(date '+%F %T') 本次执行结束（成功）"
  else
    echo "[wnflb] $(date '+%F %T') 本次执行结束（退出码 $?，签到失败会走推送通知）"
  fi
}

# 计算下一次执行时间的 epoch 秒（今天/明天各时刻中最近的一个未来时刻）
next_epoch() {
  local now best cand t
  now=$(date +%s)
  best=""
  IFS=',' read -ra arr <<< "$TIMES"
  for t in "${arr[@]}"; do
    t="$(echo "$t" | xargs)"   # 去首尾空白
    [ -z "$t" ] && continue
    cand=$(date -d "$(date +%F) $t" +%s 2>/dev/null) || continue
    if [ "$cand" -le "$now" ]; then
      cand=$(date -d "$(date -d 'tomorrow' +%F) $t" +%s 2>/dev/null) || continue
    fi
    if [ -z "$best" ] || [ "$cand" -lt "$best" ]; then
      best=$cand
    fi
  done
  echo "$best"
}

mkdir -p "$(dirname "$COOKIE_FILE")"

if [ "${RUN_ON_STARTUP:-true}" = "true" ]; then
  run_once
fi

while true; do
  target=$(next_epoch)
  if [ -z "$target" ]; then
    echo "[wnflb] CHECKIN_TIMES=[$TIMES] 未解析出有效时刻，1 小时后重试"
    sleep 3600 & SLEEP_PID=$!
    wait $SLEEP_PID 2>/dev/null
    SLEEP_PID=""
    continue
  fi
  now=$(date +%s)
  wait_sec=$((target - now))
  [ "$wait_sec" -lt 0 ] && wait_sec=0
  echo "[wnflb] 下次签到：$(date -d "@$target" '+%F %T')（${wait_sec}s 后，时刻表 [$TIMES]）"
  sleep "$wait_sec" & SLEEP_PID=$!
  wait $SLEEP_PID 2>/dev/null
  SLEEP_PID=""
  run_once
done
