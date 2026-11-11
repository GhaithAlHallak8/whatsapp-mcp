import main
import whatsapp


class _Response:
    def __init__(self, status_code, payload):
        self.status_code = status_code
        self._payload = payload

    def json(self):
        if self._payload is None:
            raise ValueError("no json")
        return self._payload


def _bridge(monkeypatch, status_code, payload):
    monkeypatch.setattr(whatsapp.requests, "post", lambda *_a, **_k: _Response(status_code, payload))


def test_retry_requested_message_reaches_the_tool(monkeypatch):
    message = "A fresh copy was requested from your phone; try again shortly."
    _bridge(monkeypatch, 200, {"success": False, "message": message, "status": "retry_requested"})
    assert whatsapp.download_media_detail("m", "c@s.whatsapp.net") == (None, message)
    monkeypatch.setattr(main, "whatsapp_download_media_detail", whatsapp.download_media_detail)
    assert main.download_media("m", "c@s.whatsapp.net") == {"success": False, "message": message}


def test_success_returns_path(monkeypatch):
    _bridge(monkeypatch, 200, {"success": True, "message": "ok", "path": "/store/a.ogg"})
    assert whatsapp.download_media("m", "c@s.whatsapp.net") == "/store/a.ogg"


def test_error_status_keeps_bridge_message(monkeypatch):
    _bridge(monkeypatch, 500, {"success": False, "message": "Failed to download media: boom"})
    assert whatsapp.download_media_detail("m", "c")[1] == "Failed to download media: boom"
    _bridge(monkeypatch, 502, None)
    assert whatsapp.download_media_detail("m", "c") == (None, "Bridge returned HTTP 502")
