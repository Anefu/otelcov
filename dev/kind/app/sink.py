"""Alertmanager webhook sink: logs one JSON line per alert it is notified of.

The receiver name is the URL path (see config/alertmanager.yaml), so the log
shows which receiver each alert was routed to:

  kubectl -n observability logs deploy/alert-sink
"""

import json
from http.server import BaseHTTPRequestHandler, HTTPServer


class Handler(BaseHTTPRequestHandler):
    def do_POST(self):
        length = int(self.headers.get("Content-Length", 0))
        body = json.loads(self.rfile.read(length) or b"{}")
        receiver = self.path.strip("/")
        for alert in body.get("alerts", []):
            print(json.dumps({
                "receiver": receiver,
                "status": alert.get("status"),
                "labels": alert.get("labels", {}),
            }, sort_keys=True), flush=True)
        self.send_response(200)
        self.end_headers()

    def log_message(self, *args):
        pass  # no access log; the alerts are the log


if __name__ == "__main__":
    HTTPServer(("", 8080), Handler).serve_forever()
