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

Всё автоматизировано — ручных шагов инициализации больше нет:

```bash
docker compose up -d --build
```

При первом старте сами отрабатывают (см. `e2e_results.txt`):

- `garage-init` — layout/бакет/ключи Garage через Admin API (`garage/init_garage.py`);
- `debezium-register` — CDC-коннектор (с retry-ожиданием Kafka Connect);
- `telemetry-seed` — 280 демо-событий телеметрии (витрина не пустая);
- `clickhouse/init` — юзер `reports_api` + KafkaEngine/MV;
- Airflow — `db migrate` + `users create` (гарантированный `admin/admin`).

Порты: Keycloak :8080 (admin/admin), фронтенд :3000, bionicpro-auth :8000,
reports-api :8001, Airflow :8081 (admin/admin), Kafka Connect :8083,
ClickHouse :8123/:9000, Garage S3 :9010, CDN :8090.

Вход (MFA обязательна, required action CONFIGURE_TOTP): `prothetic1`/`prothetic123`
(и другие из `keycloak/realm-export.json`), после пароля — настройка OTP
(Google Authenticator/FreeOTP).

Живой сквозной прогон с чистого старта (вход PKCE + MFA, отчёт, CDN MISS→HIT,
Airflow, CDC) — скриншоты в `e2e/0*.png`, сценарий `e2e/e2e_playwright.py`
(playwright), текстовая сводка `e2e_results.txt`.


### Фиксы по итогам ревью (2026-11-11)

1. **502 при входе (Задание 1)**: `bionicpro-auth` отдавал браузеру ВНУТРЕННЕЕ
   docker-имя (`keycloak:8080`) в AuthCodeURL/logout. Разделены
   `KEYCLOAK_PUBLIC_URL` (браузер: авторизация/logout) и `KEYCLOAK_URL`
   (сервер-сервер: токены/JWKS); issuer id_token проверяется через JWKS
   (SkipIssuerCheck — подпись RS256 сверяется полноценно).
2. **Airflow admin/admin (Задание 2)**: `standalone` не гарантирует учётку —
   теперь `db migrate` + `airflow users create` при старте сервиса.
3. **ClickHouse-юзер `reports_api`** создался только в живой сессии — добавлен
   в `clickhouse/init/01-init.sql` (default из docker-сети не пускает).
4. **Garage** (S3) после prune оставался без layout/бакета/ключей —
   автоинициализация сервисом `garage-init` (Admin API v1, идемпотентно,
   фиксированные ключи = env reports-api).
5. **Debezium-коннектор** регистрировался раньше готовности Kafka Connect —
   добавлен retry-цикл (до 3 минут).
6. Фронтенд ходит в API через свой origin (nginx-прокси `/api/`) — убран
   кросс-портовый fetch, ломавшийся на CORS; cookie остаётся first-party.
7. `/api/reports` проксирует исходные `from/to` (ранее терялись).

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
