#!/bin/bash
# Регистрация Debezium-коннектора CDC из CRM (PostgreSQL) в Kafka.
# Задание 4: захват изменений clients -> топик crm.public.clients.
set -e
URL="${KAFKA_CONNECT_URL:-http://localhost:8083}"
echo "Registering debezium connector at $URL ..."
curl -s -X POST "$URL/connectors" \
  -H 'Content-Type: application/json' \
  -d @/debezium/crm-connector.json || true
echo
curl -s "$URL/connectors" ; echo
