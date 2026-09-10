#!/usr/bin/env python3
"""A stub GitHub API for the end-to-end equivalence check in hack/e2e.sh.

The first issue-list request returns nothing, so adoption draws the waterline
at "now" and baselines nothing. Every later request returns the four issues
below, one per outcome the pipeline can reach.
"""
import json, sys, time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse

LOGIN = "testuser"
AGE = 0  # issues are created just after adoption, so every alert reads "just now"

ISSUES = [
    {"n": 1, "title": "Race in the <cache> reaper & retry", "body": "The reaper double-frees under contention. Steps to reproduce are in the attached trace, and it reproduces on every run.", "labels": ["bug"], "assignees": []},
    {"n": 2, "title": "Add a --json flag to status", "body": "Machine-readable status output would let me graph the queue depth over time without scraping the table.", "labels": ["enhancement"], "assignees": []},
    {"n": 3, "title": "typo", "body": "typo in readme", "labels": [], "assignees": []},
    {"n": 4, "title": "Panic on empty config file", "body": "Starting with a zero-byte config panics instead of reporting the missing fields. Should be a clean error.", "labels": [], "assignees": ["someone"]},
]

LINKED_PR = {2}          # issue 2 has an open linked PR
CLAIMED = {4}            # issue 4 has a claim comment


def issue_json(spec, now):
    return {
        "number": spec["n"],
        "node_id": "NODE%d" % spec["n"],
        "title": spec["title"],
        "body": spec["body"],
        "html_url": "https://github.com/acme/widget/issues/%d" % spec["n"],
        "state": "open",
        "comments": 1 if spec["n"] in CLAIMED else 0,
        "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - AGE)),
        "updated_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - AGE)),
        "closed_at": None,
        "author_association": "NONE",
        "user": {"login": "reporter"},
        "assignee": None,
        "assignees": [{"login": a} for a in spec["assignees"]],
        "labels": [{"name": l} for l in spec["labels"]],
    }


class Handler(BaseHTTPRequestHandler):
    listed = False
    # Frozen the first time issues are served, so a re-poll sees the same
    # created_at and the waterline stops moving.
    born = None

    def log_message(self, *a):
        pass

    def send(self, obj):
        body = json.dumps(obj).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("X-RateLimit-Resource", "core")
        self.send_header("X-RateLimit-Limit", "5000")
        self.send_header("X-RateLimit-Remaining", "4999")
        self.send_header("X-RateLimit-Reset", str(int(time.time()) + 3600))
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_GET(self):
        path = urlparse(self.path).path
        now = int(time.time())
        parts = [p for p in path.split("/") if p]

        if path == "/user":
            return self.send({"login": LOGIN})
        if path == "/rate_limit":
            bucket = {"limit": 5000, "remaining": 4999, "reset": now + 3600}
            return self.send({"resources": {"core": bucket, "search": bucket}})

        # /repos/acme/widget/issues[/N[/comments|/timeline]]
        if len(parts) == 4 and parts[0] == "repos" and parts[3] == "issues":
            if not Handler.listed:
                Handler.listed = True
                return self.send([])
            if Handler.born is None:
                Handler.born = now
            return self.send([issue_json(s, Handler.born) for s in ISSUES])

        if len(parts) >= 5 and parts[0] == "repos" and parts[3] == "issues":
            number = int(parts[4])
            spec = next(s for s in ISSUES if s["n"] == number)
            tail = parts[5] if len(parts) > 5 else ""
            born = Handler.born or now
            if tail == "timeline":
                if number not in LINKED_PR:
                    return self.send([])
                return self.send([{
                    "event": "cross-referenced",
                    "source": {"issue": {
                        "state": "open",
                        "html_url": "https://github.com/acme/widget/pull/99",
                        "user": {"login": "someone"},
                        "pull_request": {"url": "https://api.github.com/repos/acme/widget/pulls/99"},
                    }},
                }])
            if tail == "comments":
                if number not in CLAIMED:
                    return self.send([])
                return self.send([{
                    "body": "I'm taking this one.",
                    "created_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(now - 60)),
                    "author_association": "NONE",
                    "user": {"login": "someone"},
                }])
            return self.send(issue_json(spec, born))

        self.send_response(404)
        self.end_headers()


if __name__ == "__main__":
    port = int(sys.argv[1])
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
