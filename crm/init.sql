-- CRM: таблица клиентов (источник CDC для Debezium, Задание 4)
CREATE TABLE IF NOT EXISTS clients (
    id       TEXT PRIMARY KEY,
    name     TEXT NOT NULL,
    email    TEXT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE clients REPLICA IDENTITY DEFAULT;

-- сиды: sub = subject из Keycloak (пользователи prothetic1..3)
INSERT INTO clients (id, name, email) VALUES
 ('sub-prothetic1', 'Prothetic One',   'prothetic1@example.com'),
 ('sub-prothetic2', 'Prothetic Two',   'prothetic2@example.com'),
 ('sub-prothetic3', 'Prothetic Three', 'prothetic3@example.com');

-- публикация для pgoutput-плагина
CREATE PUBLICATION crm_pub FOR TABLE clients;
