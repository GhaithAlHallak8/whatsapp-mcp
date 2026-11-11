import functools
import hashlib
import hmac
import html
import json
import os
import secrets
import sqlite3
import sys
import threading
import time
from collections.abc import Callable
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlsplit

from mcp.server.auth.middleware.auth_context import get_access_token
from mcp.server.auth.provider import (
    AccessToken,
    AuthorizationCode,
    AuthorizationParams,
    AuthorizeError,
    ProviderTokenVerifier,
    RefreshToken,
    construct_redirect_uri,
)
from mcp.server.auth.settings import AuthSettings, ClientRegistrationOptions, RevocationOptions
from mcp.server.transport_security import TransportSecuritySettings
from mcp.shared.auth import OAuthClientInformationFull, OAuthToken
from starlette.requests import Request
from starlette.responses import HTMLResponse, RedirectResponse, Response

SCOPE_READ = "read"
SCOPE_SEND = "send"
SCOPES = [SCOPE_READ, SCOPE_SEND]
MIN_SECRET_LENGTH = 32
STATIC_CLIENT_ID = "static-bearer"

ACCESS_TOKEN_TTL = 3600
REFRESH_TOKEN_TTL = 30 * 24 * 3600
AUTH_CODE_TTL = 300
PENDING_TTL = 600
MAX_CLIENTS = 100
MAX_FAILED_ATTEMPTS = 5
FAILED_ATTEMPT_WINDOW = 900
CONSENT_PATH = "/consent"


class OAuthConfigError(ValueError):
    pass


@dataclass(frozen=True)
class OAuthConfig:
    secret: str
    public_url: str
    db_path: str

    @property
    def resource_url(self) -> str:
        return f"{self.public_url}/mcp"


def load_config(getenv: Callable[[str], str | None], default_db_dir: str, host: str, port: int) -> OAuthConfig | None:
    secret = (getenv("WHATSAPP_MCP_SECRET") or "").strip()
    public_url = (getenv("WHATSAPP_MCP_PUBLIC_URL") or "").strip().rstrip("/")
    if not secret:
        if public_url:
            raise OAuthConfigError("WHATSAPP_MCP_PUBLIC_URL is set but WHATSAPP_MCP_SECRET is not")
        return None
    if len(secret) < MIN_SECRET_LENGTH:
        raise OAuthConfigError(f"WHATSAPP_MCP_SECRET must be at least {MIN_SECRET_LENGTH} characters")
    if not public_url:
        public_url = f"http://{host}:{port}"
    parsed = urlsplit(public_url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname or parsed.path not in {"", "/"}:
        raise OAuthConfigError("WHATSAPP_MCP_PUBLIC_URL must be an origin such as https://wa.example.com")
    if parsed.scheme == "http" and parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
        raise OAuthConfigError("WHATSAPP_MCP_PUBLIC_URL must use https unless it is a loopback address")
    db_path = (getenv("WHATSAPP_MCP_AUTH_DB") or "").strip() or os.path.join(default_db_dir, "mcp-auth.db")
    return OAuthConfig(secret=secret, public_url=public_url, db_path=os.path.expanduser(db_path))


def _hash(token: str) -> str:
    return hashlib.sha256(token.encode()).hexdigest()


class AuthStore:
    def __init__(self, path: str):
        self.path = path
        self._lock = threading.Lock()
        directory = os.path.dirname(path)
        if directory:
            os.makedirs(directory, exist_ok=True)
        if not os.path.exists(path):
            os.close(os.open(path, os.O_CREAT | os.O_WRONLY, 0o600))
        os.chmod(path, 0o600)
        with self._connect() as conn:
            conn.executescript(
                """
                CREATE TABLE IF NOT EXISTS clients (client_id TEXT PRIMARY KEY, data TEXT NOT NULL, created_at REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS pending (id TEXT PRIMARY KEY, client_id TEXT NOT NULL, params TEXT NOT NULL, expires_at REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS codes (code_hash TEXT PRIMARY KEY, data TEXT NOT NULL, expires_at REAL NOT NULL);
                CREATE TABLE IF NOT EXISTS tokens (
                    token_hash TEXT PRIMARY KEY, kind TEXT NOT NULL, grant_id TEXT NOT NULL, client_id TEXT NOT NULL,
                    scopes TEXT NOT NULL, resource TEXT, expires_at INTEGER NOT NULL
                );
                CREATE TABLE IF NOT EXISTS audit (ts REAL NOT NULL, client_id TEXT NOT NULL, tool TEXT NOT NULL);
                """
            )

    def _connect(self) -> sqlite3.Connection:
        conn = sqlite3.connect(self.path, timeout=10)
        conn.row_factory = sqlite3.Row
        return conn

    def execute(self, sql: str, params: tuple = ()) -> list[sqlite3.Row]:
        with self._lock, self._connect() as conn:
            return conn.execute(sql, params).fetchall()

    def prune(self) -> None:
        now = time.time()
        self.execute("DELETE FROM pending WHERE expires_at < ?", (now,))
        self.execute("DELETE FROM codes WHERE expires_at < ?", (now,))
        self.execute("DELETE FROM tokens WHERE expires_at < ?", (int(now),))

    def revoke_all(self) -> int:
        count = self.execute("SELECT COUNT(*) AS n FROM tokens")[0]["n"]
        for table in ("tokens", "codes", "pending", "clients"):
            self.execute(f"DELETE FROM {table}")
        return count

    def audit(self, client_id: str, tool: str) -> None:
        self.execute("INSERT INTO audit (ts, client_id, tool) VALUES (?, ?, ?)", (time.time(), client_id, tool))


class SingleUserProvider:
    def __init__(self, config: OAuthConfig, store: AuthStore | None = None):
        self.config = config
        self.store = store or AuthStore(config.db_path)
        self._failures: list[float] = []
        self._failures_lock = threading.Lock()

    async def get_client(self, client_id: str) -> OAuthClientInformationFull | None:
        rows = self.store.execute("SELECT data FROM clients WHERE client_id = ?", (client_id,))
        return OAuthClientInformationFull.model_validate_json(rows[0]["data"]) if rows else None

    async def register_client(self, client_info: OAuthClientInformationFull) -> None:
        self.store.execute(
            "DELETE FROM clients WHERE client_id NOT IN (SELECT client_id FROM tokens) AND client_id NOT IN "
            "(SELECT client_id FROM clients ORDER BY created_at DESC LIMIT ?)",
            (MAX_CLIENTS - 1,),
        )
        self.store.execute(
            "INSERT OR REPLACE INTO clients (client_id, data, created_at) VALUES (?, ?, ?)",
            (client_info.client_id, client_info.model_dump_json(), time.time()),
        )

    async def authorize(self, client: OAuthClientInformationFull, params: AuthorizationParams) -> str:
        self.store.prune()
        requested = [scope for scope in (params.scopes or SCOPES) if scope in SCOPES]
        if not requested:
            raise AuthorizeError("invalid_scope", "No supported scope requested")
        request_id = secrets.token_urlsafe(32)
        payload = params.model_dump(mode="json")
        payload["scopes"] = requested
        self.store.execute(
            "INSERT INTO pending (id, client_id, params, expires_at) VALUES (?, ?, ?, ?)",
            (request_id, client.client_id, json.dumps(payload), time.time() + PENDING_TTL),
        )
        return f"{self.config.public_url}{CONSENT_PATH}?rid={request_id}"

    def pending(self, request_id: str) -> tuple[str, AuthorizationParams] | None:
        rows = self.store.execute(
            "SELECT client_id, params FROM pending WHERE id = ? AND expires_at >= ?", (request_id, time.time())
        )
        if not rows:
            return None
        return rows[0]["client_id"], AuthorizationParams.model_validate(json.loads(rows[0]["params"]))

    def locked_out(self) -> bool:
        cutoff = time.time() - FAILED_ATTEMPT_WINDOW
        with self._failures_lock:
            self._failures = [stamp for stamp in self._failures if stamp > cutoff]
            return len(self._failures) >= MAX_FAILED_ATTEMPTS

    def check_secret(self, candidate: str) -> bool:
        if self.locked_out():
            return False
        if hmac.compare_digest(candidate.encode(), self.config.secret.encode()):
            return True
        with self._failures_lock:
            self._failures.append(time.time())
        return False

    def approve(self, request_id: str, granted: list[str]) -> str | None:
        found = self.pending(request_id)
        if found is None:
            return None
        client_id, params = found
        scopes = [scope for scope in (params.scopes or []) if scope in granted]
        if SCOPE_READ not in scopes:
            scopes.insert(0, SCOPE_READ)
        code = secrets.token_urlsafe(32)
        record = AuthorizationCode(
            code=code,
            scopes=scopes,
            expires_at=time.time() + AUTH_CODE_TTL,
            client_id=client_id,
            code_challenge=params.code_challenge,
            redirect_uri=params.redirect_uri,
            redirect_uri_provided_explicitly=params.redirect_uri_provided_explicitly,
            resource=params.resource,
        )
        self.store.execute("DELETE FROM pending WHERE id = ?", (request_id,))
        self.store.execute(
            "INSERT INTO codes (code_hash, data, expires_at) VALUES (?, ?, ?)",
            (_hash(code), record.model_dump_json(), record.expires_at),
        )
        return construct_redirect_uri(str(params.redirect_uri), code=code, state=params.state)

    async def load_authorization_code(
        self, client: OAuthClientInformationFull, authorization_code: str
    ) -> AuthorizationCode | None:
        rows = self.store.execute("SELECT data FROM codes WHERE code_hash = ?", (_hash(authorization_code),))
        if not rows:
            return None
        record = AuthorizationCode.model_validate_json(rows[0]["data"])
        return record if record.client_id == client.client_id else None

    def _issue(self, client_id: str, scopes: list[str], resource: str | None) -> OAuthToken:
        access, refresh, grant_id = secrets.token_urlsafe(32), secrets.token_urlsafe(32), secrets.token_urlsafe(12)
        now = int(time.time())
        for token, kind, ttl in ((access, "access", ACCESS_TOKEN_TTL), (refresh, "refresh", REFRESH_TOKEN_TTL)):
            self.store.execute(
                "INSERT INTO tokens (token_hash, kind, grant_id, client_id, scopes, resource, expires_at) "
                "VALUES (?, ?, ?, ?, ?, ?, ?)",
                (_hash(token), kind, grant_id, client_id, " ".join(scopes), resource, now + ttl),
            )
        return OAuthToken(
            access_token=access, expires_in=ACCESS_TOKEN_TTL, scope=" ".join(scopes), refresh_token=refresh
        )

    async def exchange_authorization_code(
        self, client: OAuthClientInformationFull, authorization_code: AuthorizationCode
    ) -> OAuthToken:
        self.store.execute("DELETE FROM codes WHERE code_hash = ?", (_hash(authorization_code.code),))
        return self._issue(client.client_id, authorization_code.scopes, authorization_code.resource)

    def _load_token(self, token: str, kind: str) -> sqlite3.Row | None:
        rows = self.store.execute(
            "SELECT * FROM tokens WHERE token_hash = ? AND kind = ? AND expires_at >= ?",
            (_hash(token), kind, int(time.time())),
        )
        return rows[0] if rows else None

    async def load_refresh_token(self, client: OAuthClientInformationFull, refresh_token: str) -> RefreshToken | None:
        row = self._load_token(refresh_token, "refresh")
        if row is None or row["client_id"] != client.client_id:
            return None
        return RefreshToken(
            token=refresh_token,
            client_id=row["client_id"],
            scopes=row["scopes"].split(),
            expires_at=row["expires_at"],
            resource=row["resource"],
        )

    async def exchange_refresh_token(
        self, client: OAuthClientInformationFull, refresh_token: RefreshToken, scopes: list[str]
    ) -> OAuthToken:
        await self.revoke_token(refresh_token)
        return self._issue(client.client_id, scopes or refresh_token.scopes, refresh_token.resource)

    async def load_access_token(self, token: str) -> AccessToken | None:
        if hmac.compare_digest(token.encode(), self.config.secret.encode()):
            return AccessToken(
                token=token, client_id=STATIC_CLIENT_ID, scopes=list(SCOPES), resource=self.config.resource_url
            )
        row = self._load_token(token, "access")
        if row is None:
            return None
        return AccessToken(
            token=token,
            client_id=row["client_id"],
            scopes=row["scopes"].split(),
            expires_at=row["expires_at"],
            resource=row["resource"],
        )

    async def revoke_token(self, token: AccessToken | RefreshToken) -> None:
        rows = self.store.execute("SELECT grant_id FROM tokens WHERE token_hash = ?", (_hash(token.token),))
        if rows:
            self.store.execute("DELETE FROM tokens WHERE grant_id = ?", (rows[0]["grant_id"],))


CONSENT_PAGE = """<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex"><title>WhatsApp MCP</title>
<style>
body{{font-family:system-ui,sans-serif;max-width:26rem;margin:10vh auto;padding:0 1rem;color:#111;background:#fafafa}}
form{{display:grid;gap:1rem}} input[type=password]{{padding:.6rem;font-size:1rem;width:100%;box-sizing:border-box}}
button{{padding:.7rem;font-size:1rem;cursor:pointer}} .error{{color:#b00020}} label{{display:block}}
@media (prefers-color-scheme:dark){{body{{color:#eee;background:#161616}}}}
</style></head><body>
<h1>Allow access to WhatsApp?</h1>
<p><strong>{client}</strong> wants to connect to your WhatsApp MCP server.</p>
{error}
<form method="post" action="{action}">
<input type="hidden" name="rid" value="{rid}">
<label><input type="checkbox" checked disabled> Read chats, messages and media</label>
{send}
<label>Server secret<input type="password" name="secret" autocomplete="off" required autofocus></label>
<button type="submit">Allow</button>
</form></body></html>"""

SEND_OPTION = (
    '<label><input type="checkbox" name="scope" value="send"> Send messages, reactions and read receipts</label>'
)


def _consent_response(provider: SingleUserProvider, request_id: str, error: str = "", status: int = 200) -> Response:
    found = provider.pending(request_id)
    if found is None:
        return HTMLResponse("<p>This sign-in request has expired. Start again from your MCP client.</p>", 400)
    client_id, params = found
    rows = provider.store.execute("SELECT data FROM clients WHERE client_id = ?", (client_id,))
    name = (json.loads(rows[0]["data"]).get("client_name") if rows else None) or client_id
    page = CONSENT_PAGE.format(
        client=html.escape(str(name)),
        error=f'<p class="error">{html.escape(error)}</p>' if error else "",
        action=CONSENT_PATH,
        rid=html.escape(request_id),
        send=SEND_OPTION if SCOPE_SEND in (params.scopes or []) else "",
    )
    return HTMLResponse(page, status, headers={"Cache-Control": "no-store", "X-Frame-Options": "DENY"})


def register_consent_routes(mcp: Any, provider: SingleUserProvider) -> None:
    @mcp.custom_route(CONSENT_PATH, methods=["GET", "POST"])
    async def consent(request: Request) -> Response:
        if request.method == "GET":
            return _consent_response(provider, request.query_params.get("rid", ""))
        form = await request.form()
        request_id = str(form.get("rid", ""))
        if provider.locked_out():
            return _consent_response(provider, request_id, "Too many failed attempts. Try again in 15 minutes.", 429)
        if not provider.check_secret(str(form.get("secret", ""))):
            return _consent_response(provider, request_id, "That secret is not correct.", 401)
        granted = [SCOPE_READ, *[str(scope) for scope in form.getlist("scope")]]
        redirect = provider.approve(request_id, granted)
        if redirect is None:
            return _consent_response(provider, request_id)
        return RedirectResponse(redirect, status_code=302, headers={"Cache-Control": "no-store"})


def fastmcp_kwargs(config: OAuthConfig | None, provider: SingleUserProvider | None) -> dict[str, Any]:
    if config is None or provider is None:
        return {}
    return {
        "auth_server_provider": provider,
        "auth": AuthSettings(
            issuer_url=config.public_url,
            resource_server_url=config.resource_url,
            validate_token_resource=False,
            required_scopes=[SCOPE_READ],
            client_registration_options=ClientRegistrationOptions(
                enabled=True, valid_scopes=list(SCOPES), default_scopes=list(SCOPES)
            ),
            revocation_options=RevocationOptions(enabled=True),
        ),
        "transport_security": TransportSecuritySettings(enable_dns_rebinding_protection=False),
    }


_audit_store: AuthStore | None = None


def enable(mcp: Any, config: OAuthConfig) -> SingleUserProvider:
    global _audit_store
    provider = SingleUserProvider(config)
    settings = fastmcp_kwargs(config, provider)
    mcp.settings.auth = settings["auth"]
    mcp.settings.transport_security = settings["transport_security"]
    mcp._auth_server_provider = provider
    mcp._token_verifier = ProviderTokenVerifier(provider)
    register_consent_routes(mcp, provider)
    _audit_store = provider.store
    return provider


def guarded(scope: str) -> Callable[[Callable[..., Any]], Callable[..., Any]]:
    def decorate(func: Callable[..., Any]) -> Callable[..., Any]:
        @functools.wraps(func)
        def wrapper(*args: Any, **kwargs: Any) -> Any:
            token = get_access_token()
            if token is not None:
                if _audit_store is not None:
                    _audit_store.audit(token.client_id, func.__name__)
                if scope not in token.scopes:
                    raise PermissionError(
                        f"This connection was granted read-only access; {func.__name__} needs the '{scope}' scope. "
                        "Reconnect and tick the send option to allow it."
                    )
            return func(*args, **kwargs)

        return wrapper

    return decorate


def main(argv: list[str]) -> int:
    if argv[1:] != ["revoke-all"]:
        print("usage: oauth.py revoke-all", file=sys.stderr)
        return 2
    default_dir = os.path.dirname(
        os.getenv("WHATSAPP_DB_PATH")
        or os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "whatsapp-bridge", "store", "messages.db")
    )
    path = os.path.expanduser(os.getenv("WHATSAPP_MCP_AUTH_DB", "").strip() or os.path.join(default_dir, "mcp-auth.db"))
    if not os.path.exists(path):
        print("No tokens have been issued.")
        return 0
    count = AuthStore(path).revoke_all()
    print(f"Revoked {count} tokens and removed all registered clients.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv))
