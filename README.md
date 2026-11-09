# BionicPRO — проектная работа спринта 9

Кейс: производитель бионических протезов. После взлома SSO компания усиливает
безопасность (PKCE, MFA, LDAP-федерация, Яндекс ID, изоляция токенов) и строит
сервис отчётов о работе протезов (ETL → ClickHouse → API → S3/CDN) с CDC-развязкой
нагрузки CRM.

## Структура

- `docs/Task1_security_C4.drawio` — Задание 1: архитектура безопасности (BFF-сервис
  bionicpro-auth, PKCE, MFA, LDAP-представительство, Яндекс ID).
- `docs/Task2_reports_C4.drawio` — Задания 2–4: сервис отчётов (Airflow, ClickHouse,
  Kafka/Debezium, S3+CDN).
- `bionicpro-auth/` — Go-сервис токенов и сессий: PKCE S256, сессионные cookie
  HttpOnly/Secure, ротация сессии, авто-refresh access-токена (TTL 2 мин).
- `reports-api/` — Go API `/reports`: отчёт только «по себе» (sub из RS256-токена),
  витрина ClickHouse без вычислений на лету, кеш отчётов в S3, отдача CDN-ссылкой.
- `frontend/` — React без keycloak-js: логин/сессия через bionicpro-auth (cookie),
  кнопка «Download Report».
- `keycloak/realm-export.json` — стартовый realm: PKCE-клиенты, refresh, access 120 c,
  OTP (CONFIGURE_TOTP defaultAction), LDAP User Federation (маппинг групп-ролей),
  Identity Brokering Яндекс ID.
- `keycloak/keycloak-results-export.json` — итоговый экспорт realm после настройки.
- `ldap/config.ldif` — пользователи и роли-группы представительства.
- `airflow/dags/reports_etl_dag.py` — ETL: CRM (PostgreSQL) + телеметрия → витрина
  `reports_user_daily` (расписание 0 3 * * *).
- `clickhouse/init/01-init.sql` — KafkaEngine-приёмники (CDC + телеметрия),
  MaterializedView, сырой слой и витрина.
- `debezium/crm-connector.json` + `register_connector.sh` — CDC из CRM (WAL → Kafka).
- `nginx/nginx-cdn.conf` — reverse-proxy CDN: кеш 10 мин, X-Cache-Status.
- `garage/garage.toml` — S3-хранилище Garage (MinIO удалён из Docker Hub в 09.2026).
- `telemetry-producer.py` — генератор телеметрии чипов в Kafka.
- `e2e_results.txt` — сквозной прогон (все проверки, включая 403/401/202 и кеш HIT).

## Запуск

```bash
docker compose up -d --build
# Keycloak :8080 (admin/admin), фронтенд :3000, bionicpro-auth :8000,
# reports-api :8001, Airflow :8081 (admin/admin), Kafka Connect :8083,
# ClickHouse :8123/:9000, Garage S3 :9010, CDN :8090

# инициализация Garage (после первого старта):
docker compose exec minio /garage layout assign -z dc1 -c 1G <NODE_ID из garage status>
docker compose exec minio /garage layout apply --version 1
docker compose exec minio /garage bucket create reports
docker compose exec minio /garage key create reports-api   # ключи -> docker-compose env
docker compose exec minio /garage bucket allow --read --write reports --key reports-api

# телеметрия (7 дней × 40 событий):
python3 telemetry-producer.py 7 40
```

## Важные инженерные решения

- **Токены не доходят до фронтенда**: bionicpro-auth (BFF) — единственный держатель
  access/refresh; refresh хранится в AES-GCM-шифрованном виде, фронт получает только
  сессионную cookie. Ротация session id — защита от session fixation.
- **Debezium envelope** (`before/after`) потребляется ClickHouse как `JSONAsString`
  c `JSONExtract*` в MV — плоский JSONEachRow не мапится на envelope (проверено).
- **JOIN в MV поверх Kafka-движка блокирует потребление** — витрина собирается
  батчем Airflow DAG + поток телеметрии идёт через простой MV (проверено).
- **Garage вместо MinIO**: официальный образ minio/minio удалён из Docker Hub
  (сентябрь 2026), Garage — S3-совместимая замена; анонимный доступ не поддерживается,
  поэтому CDN раздаёт через подписанный GET на стороне reports-api.
- **PKCE только S256, Direct Access Grants выключены** у всех клиентов;
  e2e-прогон выполнялся сервисным пользователем (удалён из realm после теста).
