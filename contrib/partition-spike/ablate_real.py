#!/usr/bin/env python3
"""Where the time goes in a query proof_real.py compiled (spike): send the DQL as
it is, then with parts of its auth taken out by hand.

usage: ablate_real.py <arm> <query label> ["name=<regex>=><replacement>[;;<regex>=><replacement>]" ...]
       Each edit is applied to every block of the compiled DQL; blocks whose
       variables are then unused are dropped. Runs in partition 1 by header.
"""
import os
import re
import statistics
import sys
import time

import proof_real as P

def queries(text):
    return ["query {" + q for q in text.split("query {") if q.strip()]

def blocks(q):
    body = q[len("query {"):q.rindex("}")]
    out, cur = [], []
    for line in body.split("\n"):
        if re.match(r"^  \S", line) and not line.startswith("  }"):
            if cur: out.append("\n".join(cur))
            cur = [line]
        elif line.strip():
            cur.append(line)
    if cur: out.append("\n".join(cur))
    return out

def defined(b):
    return re.findall(r"(\w+) as ", b)

def prune(bs):
    """Drop var blocks none of whose variables is used elsewhere."""
    while True:
        text = "\n".join(bs)
        keep = []
        for b in bs:
            vs = defined(b)
            first = b.lstrip()
            is_var = first.startswith("var(") or re.match(r"\w+ as var\(", first)
            used = any(len(re.findall(r"\b%s\b" % re.escape(v), text)) > len(re.findall(r"\b%s as " % re.escape(v), text)) for v in vs)
            if is_var and vs and not used:
                continue
            keep.append(b)
        if len(keep) == len(bs):
            return bs
        bs = keep

def build(bs):
    return "query {\n" + "\n".join(prune(bs)) + "\n}"

def run(c, text, n=12):
    wall, last = [], None
    for i in range(n + 3):
        s = time.perf_counter()
        for q in text:
            last = c.post("/query", q, ns=1, ctype="application/dql")
            if last.get("errors"):
                return None, last["errors"][0]["message"][:300]
        if i >= 3:
            wall.append((time.perf_counter() - s) * 1000)
    return statistics.median(wall), None

if __name__ == "__main__":
    arm, label = sys.argv[1], sys.argv[2]
    text = open(os.path.join(P.OUT, arm, re.sub(r"\W+", "_", label) + ".dql")).read()
    qs = [blocks(q) for q in queries(text)]
    variants = {"as compiled": lambda b: b}
    for spec in sys.argv[3:]:
        name, edits = spec.split("=", 1)
        def f(b, edits=edits):
            for e in edits.split(";;"):
                pat, rep = e.split("=>")
                b = re.sub(pat, rep, b)
            return b
        variants[name] = f
    P.start_lambda_stub()
    with P.cluster(P.ON) as c:
        c.flags = P.HEADER_MODE
        P.restart(c)
        for name, f in variants.items():
            texts = [build([f(b) for b in bs]) for bs in qs]
            ms, err = run(c, texts)
            print("%-34s %s  blocks %d" % (name, "%8.1f ms" % ms if ms else "ERROR " + err,
                                           sum(len(re.findall(r"\(func:", t)) for t in texts)), flush=True)
