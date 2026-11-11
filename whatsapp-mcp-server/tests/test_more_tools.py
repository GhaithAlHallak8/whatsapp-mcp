import sqlite3

import pytest

import whatsapp
from tests.test_messages_db_connection import _make_db

GROUP = "120363000000000000@g.us"
DM = "971500000001@s.whatsapp.net"


def _run(path, statements):
    conn = sqlite3.connect(path)
    for sql, params in statements:
        conn.execute(sql, params)
    conn.commit()
    conn.close()


def _message(message_id, chat, ts, from_me=0, sender="971500000001", content="x", media=None, quoted=None):
    return (
        "INSERT INTO messages (id, chat_jid, sender, content, timestamp, is_from_me, media_type, quoted_message_id) "
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
        (message_id, chat, sender, content, ts, from_me, media, quoted),
    )


@pytest.fixture
def db(tmp_path, monkeypatch):
    path = _make_db(tmp_path / "messages.db")
    monkeypatch.setattr(whatsapp, "MESSAGES_DB_PATH", str(path))
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(tmp_path / "missing.db"))
    return path


@pytest.fixture
def contacts(tmp_path, monkeypatch):
    path = tmp_path / "whatsapp.db"
    conn = sqlite3.connect(path)
    conn.executescript(
        """
        CREATE TABLE whatsmeow_contacts (our_jid TEXT, their_jid TEXT, first_name TEXT, full_name TEXT,
            push_name TEXT, business_name TEXT);
        CREATE TABLE whatsmeow_lid_map (lid TEXT PRIMARY KEY, pn TEXT UNIQUE NOT NULL);
        INSERT INTO whatsmeow_lid_map VALUES ('111', '971500000001'), ('222', '971500000002');
        INSERT INTO whatsmeow_contacts VALUES ('me', '111@lid', '', '', 'Wessam', '');
        INSERT INTO whatsmeow_contacts VALUES ('me', '971500000002@s.whatsapp.net', '', 'Saved Name', '', '');
        INSERT INTO whatsmeow_contacts VALUES ('me', '222@lid', '', '', 'Push Two', '');
        """
    )
    conn.commit()
    conn.close()
    monkeypatch.setattr(whatsapp, "WHATSMEOW_DB_PATH", str(path))
    return path


def test_unsaved_number_resolves_to_display_name_stored_under_lid(contacts):
    assert whatsapp._resolve_name_from_whatsmeow("971500000001@s.whatsapp.net") == "Wessam"
    assert whatsapp._resolve_name_from_whatsmeow("971500000001") == "Wessam"
    assert whatsapp._resolve_name_from_whatsmeow("971500000002@s.whatsapp.net") == "Saved Name"
    assert whatsapp._resolve_name_from_whatsmeow("971599999999@s.whatsapp.net") is None


def test_list_unread_returns_only_inbound_messages_after_the_read_marker(db):
    _run(
        db,
        [
            (
                "INSERT INTO chats VALUES (?, 'Team', '2026-11-11 12:00:00+04:00', '2026-11-11 10:00:00+04:00')",
                (GROUP,),
            ),
            ("INSERT INTO chats VALUES (?, 'Read', '2026-11-11 09:00:00+04:00', '2026-11-11 09:30:00+04:00')", (DM,)),
            ("INSERT INTO chats VALUES ('nomarker@g.us', 'No marker', '2026-11-11 13:00:00+04:00', NULL)", ()),
            _message("old", GROUP, "2026-11-11 09:00:00+04:00"),
            _message("mine", GROUP, "2026-11-11 10:30:00+04:00", from_me=1),
            _message("new1", GROUP, "2026-11-11 11:00:00+04:00", content="first"),
            _message("new2", GROUP, "2026-11-11 12:00:00+04:00", content="second", quoted="new1"),
            _message("read", DM, "2026-11-11 09:00:00+04:00"),
            _message("nm", "nomarker@g.us", "2026-11-11 13:00:00+04:00"),
        ],
    )
    result = whatsapp.list_unread()
    assert [chat["chat_jid"] for chat in result] == [GROUP]
    chat = result[0]
    assert chat["unread_count"] == 2 and chat["chat_name"] == "Team"
    assert [m["id"] for m in chat["messages"]] == ["new1", "new2"]
    assert chat["messages"][1]["quoted_message_id"] == "new1"

    limited = whatsapp.list_unread(messages_per_chat=1)[0]
    assert limited["unread_count"] == 2 and [m["id"] for m in limited["messages"]] == ["new2"]


def test_get_group_members_attaches_names_and_sorts_admins_first(db, contacts, monkeypatch):
    seen = {}

    def post(path, payload):
        seen.update(path=path, payload=payload)
        return (
            200,
            {
                "success": True,
                "name": "Cabinet",
                "members": [
                    {"jid": "333@lid", "lid": "333", "is_admin": False, "is_super_admin": False},
                    {"jid": "111@lid", "phone": "971500000001", "lid": "111", "is_admin": False},
                    {"jid": "222@lid", "phone": "971500000002", "lid": "222", "is_admin": True, "is_super_admin": True},
                ],
            },
            "",
        )

    monkeypatch.setattr(whatsapp, "_bridge_post", post)
    result = whatsapp.get_group_members(GROUP)
    assert seen == {"path": "group-members", "payload": {"chat_jid": GROUP}}
    assert result["success"] and result["name"] == "Cabinet" and result["member_count"] == 3
    assert [(m["name"], m["phone"], m["is_admin"]) for m in result["members"]] == [
        ("Saved Name", "971500000002", True),
        ("Wessam", "971500000001", False),
        (None, None, False),
    ]

    monkeypatch.setattr(whatsapp, "_bridge_post", lambda *_a: (503, {"success": False}, "Not connected to WhatsApp"))
    assert whatsapp.get_group_members(GROUP) == {
        "success": False,
        "message": "Not connected to WhatsApp",
        "members": [],
    }


def test_transcribe_chat_audio_sends_rfc3339_range(monkeypatch):
    seen = {}

    def post(path, payload):
        seen.update(path=path, payload=payload)
        return 200, {"success": True, "candidates": 12}, ""

    monkeypatch.setattr(whatsapp, "_bridge_post", post)
    result = whatsapp.transcribe_chat_audio(
        "2026-11-11T16:00:00+04:00", before="2026-11-11T18:00:00+04:00", chat_jid=GROUP
    )
    assert result["success"] and result["queued"] == 12
    assert seen == {
        "path": "transcribe/backfill",
        "payload": {"after": "2026-11-11T16:00:00+04:00", "before": "2026-11-11T18:00:00+04:00", "chat_jid": GROUP},
    }

    monkeypatch.setattr(whatsapp, "_bridge_post", lambda *_a: (409, {"success": False}, "Auto-transcription is off."))
    assert whatsapp.transcribe_chat_audio("2026-11-11") == {
        "success": False,
        "message": "Auto-transcription is off.",
        "queued": 0,
    }
    with pytest.raises(ValueError, match="after"):
        whatsapp.transcribe_chat_audio("yesterday")


def test_refresh_chat_history_walks_back_and_counts_new_reply_links(db, monkeypatch):
    monkeypatch.setattr(whatsapp, "REFRESH_WAIT_SECONDS", 0)
    monkeypatch.setattr(whatsapp, "REFRESH_STEP", 2)
    statements = [("INSERT INTO chats VALUES (?, 'Team', NULL, NULL)", (GROUP,))]
    for hour in range(8):
        statements.append(_message(f"m{hour}", GROUP, f"2026-11-10 {10 + hour:02d}:00:00+04:00"))
    _run(db, statements)
    anchors = []

    def request(chat_jid, count=500, before_message_id=None):
        anchors.append(before_message_id)
        _run(db, [("UPDATE messages SET quoted_message_id = 'q' WHERE id = 'm5'", ())] if len(anchors) == 1 else [])
        return True, "requested"

    monkeypatch.setattr(whatsapp, "request_chat_history", request)
    result = whatsapp.refresh_chat_history(GROUP, since="2026-11-10T13:30:00+04:00")
    assert anchors == ["m7", "m5"]
    assert result["success"] and result["rounds"] == 2 and result["reply_links_added"] == 1
    assert "back to the requested date" in result["message"]

    anchors.clear()
    capped = whatsapp.refresh_chat_history(GROUP, since="2026-11-01", max_rounds=1)
    assert anchors == ["m7"] and "max_rounds" in capped["message"]

    anchors.clear()
    windowed = whatsapp.refresh_chat_history(
        GROUP, since="2026-11-10T11:30:00+04:00", before="2026-11-10T14:30:00+04:00"
    )
    assert anchors == ["m4", "m2"] and windowed["rounds"] == 2

    monkeypatch.setattr(whatsapp, "request_chat_history", lambda *_a, **_k: (False, "Not connected to WhatsApp"))
    failed = whatsapp.refresh_chat_history(GROUP, since="2026-11-10")
    assert failed["success"] is False and failed["message"] == "Not connected to WhatsApp"
    assert whatsapp.refresh_chat_history("empty@g.us", since="2026-11-10")["success"] is False


def test_request_chat_history_passes_the_anchor(monkeypatch):
    seen = []

    class Response:
        status_code = 200

        def json(self):
            return {"success": True, "message": "ok"}

    monkeypatch.setattr(whatsapp.requests, "post", lambda url, json, headers=None: seen.append(json) or Response())
    whatsapp.request_chat_history(GROUP, 50, before_message_id="abc")
    assert seen == [{"chat_jid": GROUP, "count": 50, "before_message_id": "abc"}]
