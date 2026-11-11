import sqlite3

import pytest

import whatsapp
from tests.test_messages_db_connection import _make_db

CHAT = "120363000000000000@g.us"


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = _make_db(tmp_path / "messages.db")
    conn = sqlite3.connect(path)
    conn.execute("ALTER TABLE messages ADD COLUMN deleted_at TIMESTAMP")
    conn.execute("INSERT INTO chats (jid, name) VALUES (?, 'G')", (CHAT,))
    rows = [
        ("img-old", "image", "2026-10-01 10:00:00+04:00", None),
        ("img-1", "image", "2026-11-07 10:00:00+04:00", None),
        ("img-2", "image", "2026-11-07 11:00:00+04:00", None),
        ("img-deleted", "image", "2026-11-07 12:00:00+04:00", "2026-11-07 12:05:00+04:00"),
        ("vid-1", "video", "2026-11-07 13:00:00+04:00", None),
        ("text-1", None, "2026-11-07 14:00:00+04:00", None),
        ("img-other-chat", "image", "2026-11-07 15:00:00+04:00", None),
    ]
    for message_id, media_type, ts, deleted in rows:
        chat = "other@g.us" if message_id == "img-other-chat" else CHAT
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, deleted_at) "
            "VALUES (?, ?, '971500000000', '', ?, 0, ?, ?)",
            (message_id, chat, ts, media_type, deleted),
        )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    return path


@pytest.fixture
def bridge(monkeypatch):
    calls = []

    def detail(message_id, chat_jid, recover=True):
        calls.append((message_id, recover))
        if message_id == "img-1":
            return None, "This media has expired on WhatsApp's servers."
        return f"/store/{message_id}.jpg", "ok"

    monkeypatch.setattr(whatsapp, "download_media_detail", detail)
    return calls


def test_downloads_only_matching_media_in_range(db, bridge):
    result = whatsapp.download_chat_media(CHAT, after="2026-11-07", before="2026-11-08")
    assert [d["message_id"] for d in result["downloaded"]] == ["img-2"]
    assert result["downloaded"][0]["file_path"] == "/store/img-2.jpg"
    assert [(f["message_id"], f["reason"]) for f in result["failed"]] == [
        ("img-1", "This media has expired on WhatsApp's servers.")
    ]
    assert result["total_matching"] == 2
    assert bridge == [("img-2", False), ("img-1", False)]


def test_any_type_limit_and_recover_flag(db, bridge):
    result = whatsapp.download_chat_media(CHAT, media_type="any", limit=2, recover_expired=True)
    assert [d["message_id"] for d in result["downloaded"]] == ["vid-1", "img-2"]
    assert result["total_matching"] == 4 and "2 older matches not attempted" in result["message"]
    assert all(recover for _, recover in bridge)


def test_rejects_bad_arguments(db, bridge):
    with pytest.raises(ValueError, match="media_type"):
        whatsapp.download_chat_media(CHAT, media_type="gif")
    with pytest.raises(ValueError, match="after"):
        whatsapp.download_chat_media(CHAT, after="last week")
    assert bridge == []


def test_recover_false_is_sent_to_the_bridge(monkeypatch):
    seen = []

    class Response:
        status_code = 200

        def json(self):
            return {"success": False, "message": "expired", "status": "expired"}

    monkeypatch.setattr(whatsapp.requests, "post", lambda url, json, headers=None: seen.append(json) or Response())
    assert whatsapp.download_media_detail("m", CHAT, recover=False) == (None, "expired")
    whatsapp.download_media_detail("m", CHAT)
    assert seen == [{"message_id": "m", "chat_jid": CHAT, "recover": False}, {"message_id": "m", "chat_jid": CHAT}]
