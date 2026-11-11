import sqlite3

import whatsapp
from tests.test_messages_db_connection import _make_db


def test_list_messages_filters_by_media_type(tmp_path, monkeypatch):
    db = _make_db(tmp_path / "messages.db")
    conn = sqlite3.connect(db)
    conn.execute("INSERT INTO chats (jid, name) VALUES ('1@s.whatsapp.net', 'A')")
    rows = [
        ("t1", "hello", None, "2026-01-01 10:00:00"),
        ("v1", "[transcript (elevenlabs scribe_v2)] hi", "audio", "2026-01-01 10:01:00"),
        ("i1", "", "image", "2026-01-01 10:02:00"),
    ]
    for message_id, content, media_type, ts in rows:
        conn.execute(
            "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type) "
            "VALUES (?, '1@s.whatsapp.net', '1', ?, ?, 0, ?)",
            (message_id, content, ts, media_type),
        )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(db))

    audio = whatsapp.list_messages(media_type="Audio", include_context=False)
    assert [m["id"] for m in audio] == ["v1"]
    assert len(whatsapp.list_messages(include_context=False)) == 3
