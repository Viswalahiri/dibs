#!/usr/bin/env python3
"""Prints the rows hack/e2e.sh compares across a refactor."""
import sqlite3, sys

db = sqlite3.connect(sys.argv[1])
for n, state, reason in db.execute(
        "SELECT number, state, COALESCE(reject_reason,'') FROM issues ORDER BY number"):
    print(f"#{n} {state} {reason}")
print("=== outbox ===")
for kind, key, sent in db.execute(
        "SELECT kind, COALESCE(dedupe_key,''), sent_at IS NOT NULL FROM outbox ORDER BY id"):
    print(f"{kind} {key} sent={bool(sent)}")
