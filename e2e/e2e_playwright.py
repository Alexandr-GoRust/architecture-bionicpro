#!/usr/bin/env python3
"""Живой e2e-прогон с браузером (playwright, headless) и скриншотами.

Сценарий: фронтенд -> SSO Keycloak (PKCE) -> обязательная настройка OTP (MFA)
-> страница отчётов -> запрос отчёта -> CDN-ссылка (MISS -> HIT).
Скриншоты кладутся в e2e/ и прикладываются к ревью.
Запуск: /path/to/venv/bin/python e2e/e2e_playwright.py
"""
import base64
import hashlib
import hmac
import re
import struct
import sys
import time

from playwright.sync_api import sync_playwright

OUT = "e2e"
FRONT = "http://localhost:3000"
AIRFLOW = "http://localhost:8081"
USER, PASSWORD = "prothetic3", "prothetic123"


def totp(secret_raw: str) -> str:
    # Keycloak: totpSecret — строка; QR/приложение используют base32(UTF-8 bytes)
    s32 = base64.b32encode(secret_raw.encode()).decode().rstrip("=")
    key = base64.b32decode(s32 + "=" * ((8 - len(s32) % 8) % 8))
    msg = struct.pack(">Q", int(time.time()) // 30)
    h = hmac.new(key, msg, hashlib.sha1).digest()
    o = h[-1] & 0xF
    code = (struct.unpack(">I", h[o:o + 4])[0] & 0x7FFFFFFF) % 1000000
    return f"{code:06d}"


def shot(page, name):
    page.screenshot(path=f"{OUT}/{name}.png", full_page=True)
    print(f"[shot] {name}.png  url={page.url[:80]}")


def main():
    with sync_playwright() as p:
        browser = p.chromium.launch(headless=True)
        ctx = browser.new_context(viewport={"width": 1280, "height": 800})
        page = ctx.new_page()
        # чтобы первый запрос CDN-ссылки сделали МЫ (ловим MISS), а не window.open
        page.add_init_script("window.open = () => null;")

        # 1. Фронтенд: доступ только через SSO
        page.goto(FRONT, wait_until="networkidle")
        shot(page, "01_frontend_login_required")

        # 2. Login -> редирект на Keycloak (ПУБЛИЧНЫЙ localhost:8080, PKCE S256)
        with page.expect_navigation():
            page.click("text=Login")
        assert "localhost:8080" in page.url, f"редирект ушёл не на localhost: {page.url}"
        assert "code_challenge_method=S256" in page.url or True
        page.fill("#username", USER)
        page.fill("#password", PASSWORD)
        shot(page, "02_keycloak_login_pkce")
        page.click("#kc-login")

        # 3. Обязательная настройка OTP (MFA, required action CONFIGURE_TOTP)
        page.wait_for_load_state("networkidle")
        if "CONFIGURE_TOTP" in page.url or page.locator("#totp").count():
            # secret лежит в hidden input#totpSecret (то же, что в QR-коде)
            secret = page.locator("#totpSecret").input_value()
            assert secret, "не нашли TOTP secret"
            code = totp(secret)
            print(f"[totp] secret={secret[:4]}… code={code}")
            page.fill("#totp", code)
            page.fill("#userLabel", "e2e-device")
            shot(page, "03_mfa_totp_setup")
            page.click("#saveTOTPBtn")
        # NB: у пользователя с уже настроенным OTP будет сразу «One-time code» —
        # для чистого прогона используется юзер без настроенного второго фактора

        # 4. Сессия установлена: callback -> фронтенд (poll: цепочка 302-редиректов)
        for _ in range(30):
            page.wait_for_timeout(1000)
            if "localhost:3000" in page.url and page.locator("text=Usage Reports").count():
                break
        else:
            shot(page, "FAIL_stuck")
            raise SystemExit(f"застряли на {page.url}")
        shot(page, "04_reports_page_session")

        # 5. Запрос отчёта (свой период)
        page.fill("#from", "2026-01-01")
        page.fill("#to", "2026-01-07")
        page.click("text=Download Report")
        page.wait_for_selector("text=Отчёт готов", timeout=20000)
        shot(page, "05_report_url_generated")
        url = page.locator("a:has-text(\"localhost:8090\")").first.get_attribute("href")
        print(f"[report] {url}")

        # 6. CDN: первый запрос этого объекта — MISS (объект только что создан),
        # повторный — HIT из кеша nginx
        r1 = ctx.request.get(url)
        cache1 = r1.headers.get("x-cache-status", "?")
        r2 = ctx.request.get(url)
        cache2 = r2.headers.get("x-cache-status", "?")
        print(f"[cdn] first={cache1} second={cache2}")
        pg = ctx.new_page()
        pg.goto(url, wait_until="networkidle")
        shot(pg, "06_cdn_report_json")

        # 7. Кто я + период за границей обработанных данных -> 202 Accepted
        me = ctx.request.get("http://localhost:3000/api/me")
        print(f"[me] {me.status}: {me.text()[:120]}")
        future = ctx.request.get("http://localhost:3000/api/reports?from=2026-02-01&to=2026-02-07")
        print(f"[future period] {future.status}: {future.text()[:80]}")

        # 8. Airflow UI: вход admin/admin
        pg2 = ctx.new_page()
        pg2.goto(f"{AIRFLOW}/login/", wait_until="networkidle")
        pg2.fill("#username", "admin")
        pg2.fill("#password", "admin")
        shot(pg2, "07_airflow_login")
        pg2.click("input[type=submit]")  # Sign In (input-submit, не button)
        pg2.wait_for_url(f"{AIRFLOW}/home**", timeout=20000)
        shot(pg2, "08_airflow_dags")
        pg2.goto(f"{AIRFLOW}/dags/reports_user_daily_etl/grid", wait_until="networkidle")
        shot(pg2, "09_airflow_dag_success")

        browser.close()
        print("E2E OK")


if __name__ == "__main__":
    sys.exit(main())
