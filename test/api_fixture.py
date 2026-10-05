"""Disposable DNScale API contract fixture for the pinned-controller test.

Response shapes follow the public v1 API. No production credentials are used.
"""
import base64
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, unquote, urlsplit

ZONE_ID = "11111111-1111-4111-8111-111111111111"
LOCK = threading.Lock()
RECORDS = []
WRITES = 0
READS = 0


def record_id(record):
    value = "|".join(record[k] for k in ("name", "type", "content"))
    return base64.urlsafe_b64encode(value.encode()).decode().rstrip("=")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, status, body=None):
        data = b"" if body is None else json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_GET(self):
        self.handle_request()

    def do_POST(self):
        self.handle_request()

    def do_PUT(self):
        self.handle_request()

    def do_DELETE(self):
        self.handle_request()

    def handle_request(self):
        global WRITES, READS
        url = urlsplit(self.path)
        path = url.path
        with LOCK:
            if path == "/healthz":
                return self.send(200, {})
            if self.headers.get("Authorization") != "Bearer fixture-token":
                return self.send(401, {"error": {"code": "UNAUTHORIZED", "message": "unauthorized"}})
            if path == "/test/state":
                return self.send(200, {"records": RECORDS, "writes": WRITES, "reads": READS})
            if self.command == "GET":
                READS += 1
                if path == "/v1/zones":
                    key, values = "zones", [{"id": ZONE_ID, "name": "example.org", "type": "NATIVE", "region": "eu", "status": "active"}]
                elif path == f"/v1/zones/{ZONE_ID}/records":
                    key, values = "records", RECORDS
                else:
                    return self.send(404, {})
                query = parse_qs(url.query)
                offset = int(query.get("offset", [0])[0])
                # Force pagination even with only a handful of records.
                limit = min(2, int(query.get("limit", [100])[0]))
                page = values[offset:offset + limit]
                return self.send(200, {"status": "success", "data": {key: page, "pagination": {"offset": offset, "limit": limit, "count": len(page), "total": len(values), "has_more": offset + len(page) < len(values)}}})
            base = f"/v1/zones/{ZONE_ID}/records"
            if not (path == base or path.startswith(base + "/")):
                return self.send(404, {})
            selected = unquote(path[len(base) + 1:])
            existing = next((r for r in RECORDS if r["id"] == selected), None)
            if self.command == "DELETE":
                if existing:
                    RECORDS.remove(existing)
                WRITES += 1
                return self.send(204)
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            record = {"name": body["name"].lower().rstrip("."), "type": body["type"], "content": body["content"], "ttl": body.get("ttl", 3600), "disabled": body.get("disabled", False), "comment": body.get("comment", existing.get("comment") if existing else None), "priority": None}
            if record["type"] == "CNAME":
                record["content"] = record["content"].lower().rstrip(".") + "."
            record["id"] = record_id(record)
            if self.command == "PUT" and not existing:
                return self.send(404, {"error": {"code": "NOT_FOUND", "message": "selected record absent"}})
            for old in RECORDS:
                if old is existing or old["name"] != record["name"]:
                    continue
                if old["type"] != record["type"] and "CNAME" in (old["type"], record["type"]):
                    return self.send(409, {"error": {"code": "CONFLICT", "message": "CNAME conflict"}})
            if existing:
                RECORDS.remove(existing)
            for old in RECORDS[:]:
                if old["name"] == record["name"] and old["type"] == record["type"]:
                    old["ttl"] = record["ttl"]
                    if old["id"] == record["id"] or record["type"] == "CNAME":
                        RECORDS.remove(old)
            RECORDS.append(record)
            WRITES += 1
            return self.send(201 if self.command == "POST" else 200, {"status": "success", "data": {"record": record}})


if __name__ == "__main__":
    ThreadingHTTPServer(("0.0.0.0", 8000), Handler).serve_forever()
