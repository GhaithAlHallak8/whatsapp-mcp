import base64
import hashlib
import secrets
from urllib.parse import parse_qs, urlsplit

import pytest
from mcp.server.auth.middleware.auth_context import auth_context_var
from mcp.server.auth.middleware.bearer_auth import AuthenticatedUser
from mcp.server.auth.provider import AccessToken
from mcp.server.fastmcp import FastMCP
from starlette.testclient import TestClient

import oauth

SECRET = "s" * 40
PUBLIC = "https://wa.example.com"
REDIRECT = "https://client.example/callback"


def _config(tmp_path):
    env = {"WHATSAPP_MCP_SECRET": SECRET, "WHATSAPP_MCP_PUBLIC_URL": PUBLIC}
    return oauth.load_config(env.get, str(tmp_path), "127.0.0.1", 8000)


@pytest.fixture
def server(tmp_path, monkeypatch):
    monkeypatch.setattr(oauth, "_audit_store", None)
    mcp = FastMCP("test")
    provider = oauth.enable(mcp, _config(tmp_path))
    with TestClient(mcp.streamable_http_app(), base_url=PUBLIC, follow_redirects=False) as client:
        yield client, provider


def _pkce():
    verifier = secrets.token_urlsafe(48)
    challenge = base64.urlsafe_b64encode(hashlib.sha256(verifier.encode()).digest()).rstrip(b"=").decode()
    return verifier, challenge


def _register(client):
    response = client.post(
        "/register",
        json={
            "redirect_uris": [REDIRECT],
            "client_name": "Test Client",
            "grant_types": ["authorization_code", "refresh_token"],
            "response_types": ["code"],
            "token_endpoint_auth_method": "none",
        },
    )
    assert response.status_code == 201, response.text
    return response.json()["client_id"]


def _authorize(client, client_id, challenge, scope="read send"):
    response = client.get(
        "/authorize",
        params={
            "response_type": "code",
            "client_id": client_id,
            "redirect_uri": REDIRECT,
            "code_challenge": challenge,
            "code_challenge_method": "S256",
            "scope": scope,
            "state": "xyz",
        },
    )
    assert response.status_code == 302, response.text
    location = urlsplit(response.headers["location"])
    assert location.path == oauth.CONSENT_PATH
    return parse_qs(location.query)["rid"][0]


def _grant(client, send: bool):
    client_id = _register(client)
    verifier, challenge = _pkce()
    rid = _authorize(client, client_id, challenge)
    form = {"rid": rid, "secret": SECRET}
    if send:
        form["scope"] = "send"
    response = client.post(oauth.CONSENT_PATH, data=form)
    assert response.status_code == 302, response.text
    query = parse_qs(urlsplit(response.headers["location"]).query)
    assert query["state"] == ["xyz"]
    token = client.post(
        "/token",
        data={
            "grant_type": "authorization_code",
            "code": query["code"][0],
            "redirect_uri": REDIRECT,
            "client_id": client_id,
            "code_verifier": verifier,
        },
    )
    assert token.status_code == 200, token.text
    return client_id, token.json()


def test_metadata_and_unauthenticated_request(server):
    client, _ = server
    metadata = client.get("/.well-known/oauth-authorization-server").json()
    assert metadata["issuer"].rstrip("/") == PUBLIC
    assert metadata["registration_endpoint"] == f"{PUBLIC}/register"
    assert "S256" in metadata["code_challenge_methods_supported"]
    resource = client.get("/.well-known/oauth-protected-resource/mcp").json()
    assert resource["resource"] == f"{PUBLIC}/mcp"

    response = client.post("/mcp", json={"jsonrpc": "2.0", "id": 1, "method": "ping"})
    assert response.status_code == 401
    assert "resource_metadata=" in response.headers["www-authenticate"]


def test_consent_page_requires_the_secret(server):
    client, _ = server
    client_id = _register(client)
    _, challenge = _pkce()
    rid = _authorize(client, client_id, challenge)
    page = client.get(oauth.CONSENT_PATH, params={"rid": rid})
    assert page.status_code == 200 and "Test Client" in page.text and 'value="send"' in page.text
    wrong = client.post(oauth.CONSENT_PATH, data={"rid": rid, "secret": "nope"})
    assert wrong.status_code == 401 and "location" not in wrong.headers
    assert client.get(oauth.CONSENT_PATH, params={"rid": "unknown"}).status_code == 400


def test_consent_page_locks_out_after_repeated_failures(server):
    client, _ = server
    client_id = _register(client)
    _, challenge = _pkce()
    rid = _authorize(client, client_id, challenge)
    for _ in range(oauth.MAX_FAILED_ATTEMPTS):
        assert client.post(oauth.CONSENT_PATH, data={"rid": rid, "secret": "bad"}).status_code == 401
    locked = client.post(oauth.CONSENT_PATH, data={"rid": rid, "secret": SECRET})
    assert locked.status_code == 429


def test_full_flow_issues_working_tokens(server):
    client, provider = server
    client_id, tokens = _grant(client, send=True)
    assert tokens["scope"] == "read send" and tokens["expires_in"] == oauth.ACCESS_TOKEN_TTL

    authed = client.post(
        "/mcp",
        json={
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": {
                "protocolVersion": "2025-06-18",
                "capabilities": {},
                "clientInfo": {"name": "t", "version": "0"},
            },
        },
        headers={
            "Authorization": f"Bearer {tokens['access_token']}",
            "Accept": "application/json, text/event-stream",
        },
    )
    assert authed.status_code == 200, authed.text

    refreshed = client.post(
        "/token",
        data={"grant_type": "refresh_token", "refresh_token": tokens["refresh_token"], "client_id": client_id},
    )
    assert refreshed.status_code == 200, refreshed.text
    reused = client.post(
        "/token",
        data={"grant_type": "refresh_token", "refresh_token": tokens["refresh_token"], "client_id": client_id},
    )
    assert reused.status_code == 400


def test_code_cannot_be_exchanged_with_wrong_verifier(server):
    client, _ = server
    client_id = _register(client)
    _, challenge = _pkce()
    rid = _authorize(client, client_id, challenge)
    response = client.post(oauth.CONSENT_PATH, data={"rid": rid, "secret": SECRET})
    code = parse_qs(urlsplit(response.headers["location"]).query)["code"][0]
    bad = client.post(
        "/token",
        data={
            "grant_type": "authorization_code",
            "code": code,
            "redirect_uri": REDIRECT,
            "client_id": client_id,
            "code_verifier": "wrong" * 12,
        },
    )
    assert bad.status_code == 400


@pytest.mark.anyio
async def test_tokens_survive_restart_are_hashed_and_can_be_revoked(tmp_path, server):
    client, provider = server
    _, tokens = _grant(client, send=False)
    assert tokens["scope"] == "read"

    restarted = oauth.SingleUserProvider(provider.config)
    loaded = await restarted.load_access_token(tokens["access_token"])
    assert loaded is not None and loaded.scopes == ["read"]

    with open(provider.config.db_path, "rb") as handle:
        assert tokens["access_token"].encode() not in handle.read()
    assert oct(__import__("os").stat(provider.config.db_path).st_mode & 0o777) == "0o600"

    assert restarted.store.revoke_all() == 2
    assert await restarted.load_access_token(tokens["access_token"]) is None


@pytest.mark.anyio
async def test_static_bearer_secret_is_accepted(server):
    _, provider = server
    token = await provider.load_access_token(SECRET)
    assert token is not None and token.scopes == ["read", "send"]
    assert await provider.load_access_token("x" * 40) is None


def _as(scopes):
    token = AccessToken(token="t", client_id="c1", scopes=scopes)
    return auth_context_var.set(AuthenticatedUser(token))


def test_read_only_grant_cannot_send(tmp_path, monkeypatch):
    store = oauth.AuthStore(str(tmp_path / "auth.db"))
    monkeypatch.setattr(oauth, "_audit_store", store)

    @oauth.guarded("send")
    def send_message(recipient: str) -> str:
        return f"sent to {recipient}"

    @oauth.guarded("read")
    def list_chats() -> str:
        return "chats"

    assert send_message("a") == "sent to a"

    reset = _as(["read"])
    try:
        assert list_chats() == "chats"
        with pytest.raises(PermissionError, match="read-only"):
            send_message("a")
    finally:
        auth_context_var.reset(reset)

    reset = _as(["read", "send"])
    try:
        assert send_message("b") == "sent to b"
    finally:
        auth_context_var.reset(reset)

    rows = store.execute("SELECT client_id, tool FROM audit ORDER BY rowid")
    assert [(row["client_id"], row["tool"]) for row in rows] == [
        ("c1", "list_chats"),
        ("c1", "send_message"),
        ("c1", "send_message"),
    ]


def test_send_tools_are_guarded_with_send_scope():
    import main

    reset = _as(["read"])
    try:
        for name in ("send_message", "send_file", "send_audio_message", "send_reaction", "mark_messages_read"):
            with pytest.raises(PermissionError):
                getattr(main, name)()
    finally:
        auth_context_var.reset(reset)


@pytest.mark.parametrize(
    "env",
    [
        {"WHATSAPP_MCP_SECRET": "short"},
        {"WHATSAPP_MCP_PUBLIC_URL": PUBLIC},
        {"WHATSAPP_MCP_SECRET": SECRET, "WHATSAPP_MCP_PUBLIC_URL": "http://wa.example.com"},
        {"WHATSAPP_MCP_SECRET": SECRET, "WHATSAPP_MCP_PUBLIC_URL": "https://wa.example.com/path"},
    ],
)
def test_bad_config_is_rejected(env, tmp_path):
    with pytest.raises(oauth.OAuthConfigError):
        oauth.load_config(env.get, str(tmp_path), "127.0.0.1", 8000)


def test_oauth_is_off_without_a_secret(tmp_path):
    assert oauth.load_config({}.get, str(tmp_path), "127.0.0.1", 8000) is None
