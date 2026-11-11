import sqlite3

import pytest

import whatsapp
from tests.test_messages_db_connection import _make_db

CHAT = "120363000000000000@g.us"


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = _make_db(tmp_path / "messages.db")
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "HISTORY_POLL_SECONDS", 0)
    monkeypatch.setattr(whatsapp, "HISTORY_WAIT_SECONDS", 0.05)
    _insert(path, [("m0", "2026-11-08 12:00:00+04:00")])
    return path


def _insert(path, rows):
    conn = sqlite3.connect(path)
    conn.execute("INSERT OR IGNORE INTO chats (jid, name) VALUES (?, 'G')", (CHAT,))
    for message_id, ts in rows:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me) VALUES (?, ?, '1', 'x', ?, 0)",
            (message_id, CHAT, ts),
        )
    conn.commit()
    conn.close()


def _phone(monkeypatch, db, batches):
    calls = []

    def request(chat_jid, count=whatsapp.HISTORY_REQUEST_COUNT):
        calls.append(chat_jid)
        if batches:
            _insert(db, batches.pop(0))
        return True, "requested"

    monkeypatch.setattr(whatsapp, "request_chat_history", request)
    return calls


def test_stops_once_the_requested_date_is_covered(db, monkeypatch):
    calls = _phone(
        monkeypatch,
        db,
        [
            [("m1", "2026-11-05 10:00:00+04:00")],
            [("m2", "2026-10-30 10:00:00+04:00")],
            [("m3", "2026-10-01 10:00:00+04:00")],
        ],
    )
    result = whatsapp.backfill_chat(CHAT, until="2026-10-31T00:00:00+04:00", max_rounds=10)
    assert result["success"] and len(calls) == 2 and result["rounds"] == 2
    assert result["messages_added"] == 2
    assert result["oldest_after"].startswith("2026-10-30")
    assert "covers" in result["message"]


def test_stops_when_the_phone_returns_nothing(db, monkeypatch):
    calls = _phone(monkeypatch, db, [[("m1", "2026-11-05 10:00:00+04:00")]])
    result = whatsapp.backfill_chat(CHAT, until="2026-01-01", max_rounds=10)
    assert result["success"] and len(calls) == 2 and result["messages_added"] == 1
    assert "no older messages" in result["message"]


def test_respects_max_rounds_and_skips_when_already_covered(db, monkeypatch):
    calls = _phone(monkeypatch, db, [[(f"m{i}", f"2026-11-0{8 - i} 10:00:00+04:00")] for i in range(1, 6)])
    result = whatsapp.backfill_chat(CHAT, max_rounds=2)
    assert len(calls) == 2 and "max_rounds" in result["message"]

    calls.clear()
    covered = whatsapp.backfill_chat(CHAT, until="2026-11-08")
    assert calls == [] and covered["rounds"] == 0 and "covers" in covered["message"]


def test_bridge_refusal_and_bad_date(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "request_chat_history", lambda *_a, **_k: (False, "not connected"))
    result = whatsapp.backfill_chat(CHAT)
    assert result == {
        "success": False,
        "message": "not connected",
        "rounds": 0,
        "messages_added": 0,
        "oldest_before": "2026-11-08T12:00:00+04:00",
        "oldest_after": "2026-11-08T12:00:00+04:00",
    }
    with pytest.raises(ValueError, match="until"):
        whatsapp.backfill_chat(CHAT, until="last week")


def test_request_chat_history_posts_to_the_bridge(monkeypatch):
    seen = {}

    class Response:
        status_code = 200

        def json(self):
            return {"success": True, "message": "Requested"}

    def post(url, json, headers=None):
        seen.update(url=url, json=json)
        return Response()

    monkeypatch.setattr(whatsapp.requests, "post", post)
    assert whatsapp.request_chat_history(CHAT, 50) == (True, "Requested")
    assert seen["url"].endswith("/history") and seen["json"] == {"chat_jid": CHAT, "count": 50}
