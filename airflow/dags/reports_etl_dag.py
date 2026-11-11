"""
Задание 2, задача 2 — ETL-процесс на Airflow.

Извлекает данные из CRM (PostgreSQL) и телеметрии протезов (ClickHouse,
приёмник KafkaEngine), объединяет в витрину reports_user_daily в OLAP.
Запуск — ежедневно по расписанию 0 3 * * *.

Примечание: стандартный провайдер apache-airflow-providers-clickhouse
недоступен на PyPI (имя занято пустым пакетом; сентябрь 2026 — официальный
провайдер вышел как apache-airflow-providers-clickhousedb). Поэтому SQL к
ClickHouse выполняется напрямую через clickhouse-driver в PythonOperator —
функционально эквивалентно ClickHouseOperator.
"""
from datetime import datetime, timedelta
import os

import clickhouse_driver
import psycopg2
from airflow import DAG
from airflow.operators.python import PythonOperator

CH_HOST = "clickhouse"
CH_DB = "reports"
CH_USER = os.environ.get("CH_USER", "reports_api")
CH_PASSWORD = os.environ.get("CH_PASSWORD", "reports_api_pass")
CRM_DSN = "host=crm_db port=5432 dbname=crm user=crm_user password=crm_password"

DDL = [
    # телеметрия: создаётся и потоком (KafkaEngine, Задание 4), и здесь — IF NOT EXISTS
    """
    CREATE TABLE IF NOT EXISTS telemetry_raw
    (
        user_id        String,
        prosthesis_id  String,
        event_time     DateTime,
        response_ms    Float32,
        event_date     Date DEFAULT toDate(event_time)
    )
    ENGINE = MergeTree
    PARTITION BY toYYYYMM(event_date)
    ORDER BY (user_id, prosthesis_id, event_time)
    """,
    # витрина: денормализованная широкая таблица (Кимбалл), ключ пользователя первым
    """
    CREATE TABLE IF NOT EXISTS reports_user_daily
    (
        user_id          String,
        day              Date,
        prosthesis_id    String,
        events           UInt64,
        avg_response_ms  Float32,
        client_name      String,
        client_email     String
    )
    ENGINE = SummingMergeTree
    PARTITION BY toYYYYMM(day)
    ORDER BY (user_id, day, prosthesis_id)
    """,
    # stage-таблица клиентов CRM для батч-пути ETL (CDC-реплика — Задание 4)
    """
    CREATE TABLE IF NOT EXISTS crm_clients_stage
    (
        user_id   String,
        name      String,
        email     String,
        loaded_at DateTime DEFAULT now()
    )
    ENGINE = ReplacingMergeTree
    ORDER BY user_id
    """,
]


def ch():
    # отдельный сервисный юзер reports_api (см. clickhouse/init/01-init.sql):
    # у default из docker-сети аутентификация не проходит
    return clickhouse_driver.Client(host=CH_HOST, database=CH_DB,
                                    user=CH_USER, password=CH_PASSWORD)


def create_tables(**_):
    client = ch()
    for ddl in DDL:
        client.execute(ddl)


def extract_crm(**_):
    """Extract: клиенты из CRM (PostgreSQL) -> stage ClickHouse."""
    pg = psycopg2.connect(CRM_DSN)
    with pg.cursor() as cur:
        cur.execute("SELECT id, name, email FROM clients")
        rows = cur.fetchall()
    ch().execute("INSERT INTO crm_clients_stage (user_id, name, email) VALUES", rows)


def load_telemetry(**_):
    """Load: телеметрия за вчерашний день из KafkaEngine-приёмника уже в CH;
    здесь проверяем её наличие (защита от пустого дня)."""
    n = ch().execute("SELECT count() FROM telemetry_raw")[0][0]
    print(f"telemetry_raw rows: {n}")


def build_datamart(**_):
    """Transform+Load: витрина = телеметрия JOIN клиенты.

    Полный пересбор (TRUNCATE + INSERT за весь период): демо-данные телеметрии
    приходят "задним числом" (генератор telemetry-producer.py), поэтому фильтр
    по yesterday() оставлял бы витрину пустой; при малом объёме полный рефреш
    проще и идемпотентен (нет дублей). В проде на растущем объёме шаг
    заменяется инкрементом WHERE toDate(event_time) = yesterday().
    """
    ch().execute("TRUNCATE TABLE reports_user_daily")
    ch().execute("""
        INSERT INTO reports_user_daily
        (user_id, day, prosthesis_id, events, avg_response_ms, client_name, client_email)
        SELECT
            t.user_id, toDate(t.event_time), t.prosthesis_id,
            count(), avg(t.response_ms),
            coalesce(any(c.name), ''), coalesce(any(c.email), '')
        FROM telemetry_raw AS t
        LEFT JOIN (SELECT user_id, argMax(name, loaded_at) AS name,
                          argMax(email, loaded_at) AS email
                   FROM crm_clients_stage GROUP BY user_id) AS c
          ON c.user_id = t.user_id
        GROUP BY t.user_id, toDate(t.event_time), t.prosthesis_id
    """)
    n = ch().execute("SELECT count() FROM reports_user_daily")[0][0]
    print(f"reports_user_daily rows: {n}")


with DAG(
    dag_id="reports_user_daily_etl",
    default_args={"owner": "bionicpro", "start_date": datetime(2026, 1, 1),
                  "retries": 1, "retry_delay": timedelta(minutes=5)},
    schedule_interval="0 3 * * *",
    catchup=False,
    tags=["bionicpro", "reports", "etl"],
) as dag:

    t_create = PythonOperator(task_id="create_tables", python_callable=create_tables)
    t_extract = PythonOperator(task_id="extract_crm", python_callable=extract_crm)
    t_telemetry = PythonOperator(task_id="load_telemetry", python_callable=load_telemetry)
    t_datamart = PythonOperator(task_id="build_datamart", python_callable=build_datamart)

    t_create >> t_extract >> t_telemetry >> t_datamart
