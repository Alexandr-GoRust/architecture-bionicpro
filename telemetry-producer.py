#!/usr/bin/env python3
"""Демо-генератор телеметрии: чипы протезов -> Kafka топик telemetry.prosthesis.
Эмулирует поток с 4G-модулей (Задание 2/4: данные для витрины отчётов)."""
import json, random, time, datetime, sys, os
from kafka import KafkaProducer

producer = KafkaProducer(
    # по умолчанию — EXTERNAL listener с хоста; внутри docker-сети задать kafka:9092
    bootstrap_servers=os.environ.get("KAFKA_BOOTSTRAP", "localhost:29092"),
    value_serializer=lambda v: json.dumps(v).encode(),
)
users = ["sub-prothetic1", "sub-prothetic2", "sub-prothetic3"]
prost = {users[0]: "prosthesis-001", users[1]: "prosthesis-002", users[2]: "prosthesis-003"}
days = int(sys.argv[1]) if len(sys.argv) > 1 else 7
events = int(sys.argv[2]) if len(sys.argv) > 2 else 50

n = 0
for day in range(1, days + 1):
    for _ in range(events):
        u = random.choice(users)
        ts = datetime.datetime(2026, 1, day, random.randint(8, 22), random.randint(0, 59))
        producer.send("telemetry.prosthesis", {
            "user_id": u,
            "prosthesis_id": prost[u],
            "event_time": ts.isoformat(),
            "response_ms": round(random.uniform(35, 110), 1),
        })
        n += 1
producer.flush()
print(f"sent {n} telemetry events over {days} days")
