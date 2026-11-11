#!/usr/bin/env python3
"""Автоинициализация Garage S3 при первом старте (идемпотентно).

Заменяет ручные шаги из README: layout assign/apply, bucket create,
импорт фиксированных ключей reports-api (те же, что в docker-compose
для reports-api), выдача прав на бакет. Без этого чистый `docker compose up`
оставляет Garage без зоны/бакета/ключей и сервис отчётов не может работать.

Использует только Garage Admin API v1 (stdlib, без зависимостей):
  GET  /v1/status                 -> node id
  POST /v1/layout [roles]         -> staged changes
  POST /v1/layout/apply {version} -> применить
  POST /v1/bucket {name}          -> bucketId (409 если существует)
  GET  /v1/bucket?globalAlias=    -> bucketId существующего
  POST /v1/key/import             -> фиксированные access/secret
  POST /v1/bucket/allow           -> read/write
"""
import json
import os
import sys
import time
import urllib.error
import urllib.request

ADMIN_URL = os.environ.get("GARAGE_ADMIN_URL", "http://minio:3901").rstrip("/")
TOKEN = os.environ.get("GARAGE_ADMIN_TOKEN", "")
ACCESS_KEY = os.environ["GARAGE_S3_ACCESS_KEY"]
SECRET_KEY = os.environ["GARAGE_S3_SECRET_KEY"]
BUCKET = os.environ.get("GARAGE_BUCKET", "reports")
ZONE = os.environ.get("GARAGE_ZONE", "dc1")
CAPACITY = int(os.environ.get("GARAGE_CAPACITY", "1000000000"))


def req(method, path, body=None, ok=(200,)):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(ADMIN_URL + path, data=data, method=method)
    r.add_header("Content-Type", "application/json")
    if TOKEN:
        r.add_header("Authorization", "Bearer " + TOKEN)
    try:
        with urllib.request.urlopen(r, timeout=10) as resp:
            raw = resp.read()
            payload = json.loads(raw) if raw else {}
            return resp.status, payload
    except urllib.error.HTTPError as e:
        try:
            payload = json.loads(e.read() or b"{}")
        except Exception:
            payload = {}
        if e.code in ok:
            return e.code, payload
        raise RuntimeError(f"{method} {path} -> HTTP {e.code}: {payload}") from e


def wait_admin():
    for i in range(60):
        try:
            _, st = req("GET", "/v1/status")
            print(f"[garage-init] admin ok, garage {st.get('garageVersion')}", flush=True)
            return st
        except Exception as e:
            if i == 0:
                print("[garage-init] waiting for garage admin api...", flush=True)
            time.sleep(2)
    raise SystemExit("[garage-init] garage admin api unreachable")


def main():
    st = wait_admin()
    node_id = st["node"]
    layout_version = st.get("layoutVersion", 0)

    # 1. layout: назначаем узел в зону (пропускаем, если уже применён)
    if layout_version < 1:
        req("POST", "/v1/layout", [{"id": node_id, "capacity": CAPACITY,
                                    "zone": ZONE, "tags": ["init"]}])
        req("POST", "/v1/layout/apply", {"version": 1})
        print(f"[garage-init] layout applied: node {node_id[:8]}… zone={ZONE}", flush=True)
    else:
        print(f"[garage-init] layout already v{layout_version}, skip", flush=True)

    # 2. bucket (409 = уже существует -> берём по alias).
    # ВАЖНО: v1 API создаёт бакет с S3-именем через globalAlias
    # (поле name молча игнорируется -> бакет без имени).
    code, b = req("POST", "/v1/bucket", {"globalAlias": BUCKET}, ok=(200, 409))
    if code == 409:
        _, b = req("GET", f"/v1/bucket?globalAlias={BUCKET}")
    bucket_id = b["id"]
    print(f"[garage-init] bucket '{BUCKET}' id={bucket_id[:8]}…", flush=True)

    # 3. фиксированные ключи reports-api (import идемпотентен: конфликт -> ищем по id)
    code, k = req("POST", "/v1/key/import",
                  {"accessKeyId": ACCESS_KEY, "secretAccessKey": SECRET_KEY,
                   "name": "reports-api"}, ok=(200, 409))
    if code == 409:
        _, k = req("GET", f"/v1/key?id={ACCESS_KEY}")
    print(f"[garage-init] key '{k.get('name', 'reports-api')}' ready", flush=True)

    # 4. права на бакет
    req("POST", "/v1/bucket/allow",
        {"bucketId": bucket_id, "accessKeyId": ACCESS_KEY,
         "permissions": {"read": True, "write": True}})
    print("[garage-init] bucket allow read/write -> done", flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"[garage-init] ERROR: {e}", file=sys.stderr, flush=True)
        raise SystemExit(1)
