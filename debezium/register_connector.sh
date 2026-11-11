#!/bin/sh
# Регистрация Debezium-коннектора CDC из CRM (PostgreSQL) в Kafka.
# Задание 4: захват изменений clients -> топик crm.public.clients.
# Ждём готовности Kafka Connect REST (на чистом старте он поднимается
# дольше, чем стартует сам контейнер), затем регистрируем коннектор.
set -e
URL="${KAFKA_CONNECT_URL:-http://localhost:8083}"

echo "Waiting for Kafka Connect at $URL ..."
i=0
until curl -sf -m 3 "$URL/connectors" >/dev/null 2>&1; do
  i=$((i + 1))
  if [ "$i" -ge 60 ]; then
    echo "Kafka Connect unreachable after $i attempts, giving up" >&2
    exit 1
  fi
  sleep 3
done

echo "Registering debezium connector at $URL ..."
curl -s -X POST "$URL/connectors" \
  -H 'Content-Type: application/json' \
  -d @/debezium/crm-connector.json || true
echo
curl -s "$URL/connectors"; echo
