-- Задание 4: приём данных из Kafka (Debezium CDC из CRM + телеметрия чипов)
-- через KafkaEngine и витрины на MaterializedView.
--
-- NB: Debezium шлёт envelope {"before":..,"after":{..},"op":"r|u|d"} —
-- поэтому CDC-топик потребляется как JSONAsString (сырой JSON), а поля
-- извлекаются JSONExtract* в MaterializedView (проверено живым прогоном).

-- Сервисный юзер для reports-api и Airflow ETL: доступ только к БД reports
-- (наименьшие привилегии). Юзер default из docker-сети не пускает
-- (Authentication failed) — поэтому сервисы подключаются всегда им.
CREATE USER IF NOT EXISTS reports_api IDENTIFIED BY 'reports_api_pass';
GRANT CREATE TABLE, INSERT, SELECT, TRUNCATE ON reports.* TO reports_api;

-- Приёмник CDC-топика crm.public.clients
CREATE TABLE IF NOT EXISTS reports.kafka_crm_clients (raw String)
ENGINE = Kafka
SETTINGS kafka_broker_list = 'kafka:9092',
         kafka_topic_list = 'crm.public.clients',
         kafka_group_name = 'ch_crm_clients_v6',
         kafka_format = 'JSONAsString',
         kafka_num_consumers = 1,
         kafka_skip_broken_messages = 10;

-- Реплика клиентов CRM в OLAP
CREATE TABLE IF NOT EXISTS reports.crm_clients
(
    user_id    String,
    name       String,
    email      String,
    updated_at DateTime DEFAULT now()
)
ENGINE = ReplacingMergeTree
ORDER BY user_id;

CREATE MATERIALIZED VIEW IF NOT EXISTS reports.mv_crm_clients TO reports.crm_clients AS
SELECT
    JSONExtractString(raw, 'after', 'id')    AS user_id,
    JSONExtractString(raw, 'after', 'name')  AS name,
    JSONExtractString(raw, 'after', 'email') AS email
FROM reports.kafka_crm_clients;

-- Приёмник телеметрии чипов (плоский JSONEachRow)
CREATE TABLE IF NOT EXISTS reports.kafka_telemetry
(
    user_id       String,
    prosthesis_id String,
    event_time    DateTime,
    response_ms   Float32
)
ENGINE = Kafka
SETTINGS kafka_broker_list = 'kafka:9092',
         kafka_topic_list = 'telemetry.prosthesis',
         kafka_group_name = 'ch_telemetry_v3',
         kafka_format = 'JSONEachRow',
         kafka_num_consumers = 1,
         kafka_skip_broken_messages = 10;

-- Сырая телеметрия (партиции по месяцу, ключ пользователя — быстрые выборки)
CREATE TABLE IF NOT EXISTS reports.telemetry_raw
(
    user_id       String,
    prosthesis_id String,
    event_time    DateTime,
    response_ms   Float32,
    event_date    Date DEFAULT toDate(event_time)
)
ENGINE = MergeTree
PARTITION BY toYYYYMM(event_date)
ORDER BY (user_id, prosthesis_id, event_time);

CREATE MATERIALIZED VIEW IF NOT EXISTS reports.mv_telemetry TO reports.telemetry_raw AS
SELECT user_id, prosthesis_id, event_time, response_ms
FROM reports.kafka_telemetry;

-- Итоговая витрина отчётности (Задание 2/4): телеметрия + клиенты CDC-реплики,
-- обновляется потоком через MaterializedView.
CREATE TABLE IF NOT EXISTS reports.reports_user_daily
(
    user_id         String,
    day             Date,
    prosthesis_id   String,
    events          UInt64,
    avg_response_ms Float32,
    client_name     String,
    client_email    String
)
ENGINE = SummingMergeTree
PARTITION BY toYYYYMM(day)
ORDER BY (user_id, day, prosthesis_id);

-- Витрина reports_user_daily наполняется двумя способами:
-- 1) батч: Airflow DAG reports_user_daily_etl (JOIN телеметрии с CDC-репликой
--    CRM по расписанию 0 3 * * *) — основной путь по архитектуре Задания 2;
-- 2) поток: при желании включается MV ниже (данные уже без JOIN собираются
--    запросом витрины через JOIN на лету из отчётного API).
-- В живом прогоне включён батч-путь: JOIN в MV на Kafka-движке блокирует
--    поток потребления (проверено), поэтому MV по потоку здесь не активируется.
