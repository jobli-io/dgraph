#!/usr/bin/env python3
"""Dump the DQL schema of a running alpha as text that /alter accepts.

Used to copy the shape of a real schema (predicates, indexes, types) into a
throwaway cluster without copying any data.

usage: dump_dql_schema.py [alpha http address, default localhost:8081] > schema.dql
"""

import json
import sys
import urllib.request

ALPHA = "http://" + (sys.argv[1] if len(sys.argv) > 1 else "localhost:8081")


def main():
    req = urllib.request.Request(ALPHA + "/query", data=b"schema {}", method="POST")
    req.add_header("Content-Type", "application/dql")
    with urllib.request.urlopen(req, timeout=120) as resp:
        data = json.loads(resp.read())["data"]

    lines = []
    for p in data.get("schema", []):
        name = p["predicate"]
        if name.startswith("dgraph."):
            continue
        typ = p["type"]
        if p.get("list"):
            typ = "[%s]" % typ
        parts = ["<%s>: %s" % (name, typ)]
        toks = p.get("tokenizer") or []
        if toks:
            parts.append("@index(%s)" % ", ".join(toks))
        for flag in ("reverse", "count", "upsert", "lang", "unique", "noconflict"):
            if p.get(flag):
                parts.append("@" + flag)
        lines.append(" ".join(parts) + " .")

    for t in data.get("types", []):
        if t["name"].startswith("dgraph."):
            continue
        fields = "\n".join("  <%s>" % f["name"] for f in t.get("fields", []))
        lines.append("type <%s> {\n%s\n}" % (t["name"], fields))

    sys.stdout.write("\n".join(lines) + "\n")


if __name__ == "__main__":
    main()
