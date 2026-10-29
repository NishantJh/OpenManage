#!/usr/bin/env python3
"""Stand-in for the ASU build server and the OpenWrt download server (VM flash test).

Serves the JSON shapes OpenManage reads for an upgrade plan and the build API, and one
real image from disk. The first build poll says "started"; the second returns the
finished build. Test use only.
"""
import hashlib
import json
import os
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

port, image_path, to_version, from_version, upstream = int(sys.argv[1]), sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
image_name = os.path.basename(image_path)
bin_dir = "0123456789abcdef"
sha = hashlib.sha256(open(image_path, "rb").read()).hexdigest()
polls = {}

overview = {"latest": [to_version], "upstream_url": upstream, "branches": {
    to_version.rsplit(".", 1)[0]: {"enabled": True, "path": "releases/{version}", "versions": sorted({to_version, from_version}, reverse=True),
                                   "targets": {"x86/64": "x86_64"}, "package_changes": []}}}
profiles = {"version_code": "r0-vmtest", "arch_packages": "x86_64", "default_packages": ["busybox"], "linux_kernel": {"version": "6.6.0"},
            "profiles": {"generic": {"device_packages": [], "supported_devices": [], "titles": [{"vendor": "", "model": "Generic x86/64"}]}}}
static = {
    "/json/v1/overview.json": overview,
    f"/releases/{to_version}/targets/x86/64/profiles.json": profiles,
    f"/json/v1/releases/{to_version}/targets/x86/64/index.json": {"packages": {"kmod-e1000": "6.6"}},
    f"/json/v1/releases/{to_version}/packages/x86_64-index.json": {"tcpdump": "4.99"},
}


class Handler(BaseHTTPRequestHandler):
    def send_json(self, code, obj):
        data = json.dumps(obj).encode()
        self.send_response(code)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length") or 0))
        if self.path.rstrip("/") == "/api/v1/build":
            polls.clear()
            return self.send_json(202, {"status": 202, "detail": "queued", "request_hash": bin_dir})
        self.send_json(404, {"detail": "not found"})

    def do_GET(self):
        path = self.path.split("?")[0]
        if path in static:
            return self.send_json(200, static[path])
        if path.startswith("/api/v1/build/"):
            polls[bin_dir] = polls.get(bin_dir, 0) + 1
            if polls[bin_dir] < 2:
                return self.send_json(202, {"status": 202, "detail": "started", "request_hash": bin_dir})
            return self.send_json(200, {"status": 200, "detail": "done", "bin_dir": bin_dir,
                                        "images": [{"name": image_name, "type": "sysupgrade", "filesystem": "squashfs", "sha256": sha}]})
        if path == f"/store/{bin_dir}/{image_name}":
            self.send_response(200)
            self.send_header("Content-Type", "application/octet-stream")
            self.send_header("Content-Length", str(os.path.getsize(image_path)))
            self.end_headers()
            with open(image_path, "rb") as f:
                while chunk := f.read(1 << 20):
                    self.wfile.write(chunk)
            return
        self.send_json(404, {"detail": "not found"})

    def log_message(self, fmt, *args):
        sys.stderr.write("asu: " + fmt % args + "\n")


ThreadingHTTPServer(("0.0.0.0", port), Handler).serve_forever()
