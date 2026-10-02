# 福利吧论坛自动签到 sidecar
# 上游脚本：https://github.com/fmdxx1991/wnflb-checkin
# （二次开发自 https://github.com/appcctv/wnflb-checkin）
#
# 构建时如需换 pip 镜像源（国内服务器拉 PyPI 慢/失败时）：
# 在 .env 里加 PIP_INDEX_URL=https://pypi.tuna.tsinghua.edu.cn/simple
# 再 docker compose build wnflb-checkin

FROM python:3.12-slim

ARG PIP_INDEX_URL=https://pypi.org/simple

ENV TZ=Asia/Shanghai \
    PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1

RUN apt-get update \
    && apt-get install -y --no-install-recommends tzdata \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY requirements.txt ./
RUN pip install --index-url "$PIP_INDEX_URL" -r requirements.txt

COPY wnflb_checkin.py entrypoint.sh ./
RUN chmod +x entrypoint.sh

# Cookie 持久化目录（compose 里挂载 ./wnflb-data）
VOLUME ["/app/data"]

ENTRYPOINT ["/app/entrypoint.sh"]
