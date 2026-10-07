"""``evalsi annotate``: human annotation queues on a server.

A queue holds records (a run's, filtered by CEL, or from a file) and a rubric
of questions. ``start`` walks an annotator through the items the server hands
them; ``stats`` prints each question's summary, the agreement between
annotators and with the run's metric; ``export`` writes every answer as JSONL.
"""

from __future__ import annotations

import argparse
import json
import sys
from collections.abc import Callable
from pathlib import Path
from typing import Any

from evalsi.cli import add_credential_args, server_client

KINDS = ("pass_fail", "score", "label", "text")


class _Quit(Exception):
    pass


class _Skip(Exception):
    pass


def add_annotate_commands(sub: Any) -> None:
    an = sub.add_parser("annotate", help="human annotation queues on a server")
    an_sub = an.add_subparsers(dest="action", required=True)

    def common(p: argparse.ArgumentParser, queue: bool = True) -> None:
        if queue:
            p.add_argument("queue", help="the queue's name")
            p.add_argument("--project", required=True)
        p.add_argument("--server", required=True, help="evalsid URL")
        add_credential_args(p)

    ls = an_sub.add_parser("queues", help="list queues")
    ls.add_argument("--project", default="")
    common(ls, queue=False)
    ls.set_defaults(func=_cmd_queues)

    cr = an_sub.add_parser("create", help="create a queue from a YAML or JSON file")
    cr.add_argument("-f", "--file", required=True)
    common(cr, queue=False)
    cr.set_defaults(func=_cmd_create)

    rm = an_sub.add_parser("delete", help="delete a queue with its items and answers")
    common(rm)
    rm.set_defaults(func=_cmd_delete)

    ad = an_sub.add_parser("add", help="add a run's records, or records from a file")
    src = ad.add_mutually_exclusive_group(required=True)
    src.add_argument("--run", help="a run id in the queue's project")
    src.add_argument("--records", help="a dataset file (JSONL, JSON, CSV)")
    ad.add_argument(
        "--when", default="", help="with --run: CEL over scores, errored, trial and record"
    )
    ad.add_argument("--all-trials", action="store_true", help="with --run: every matching trial")
    ad.add_argument("--limit", type=int, default=0)
    common(ad)
    ad.set_defaults(func=_cmd_add)

    st = an_sub.add_parser("start", help="annotate items interactively")
    st.add_argument(
        "--show-scores", action="store_true", help="show the run's scores (they can bias answers)"
    )
    st.add_argument("--lease", type=int, default=0, help="seconds an item is held (default 1800)")
    common(st)
    st.set_defaults(func=_cmd_start)

    ss = an_sub.add_parser("stats", help="progress, summaries and agreement")
    ss.add_argument("--format", choices=["table", "json"], default="table")
    common(ss)
    ss.set_defaults(func=_cmd_stats)

    ex = an_sub.add_parser("export", help="write every annotation, with its item, as JSONL")
    ex.add_argument("-o", "--out", required=True)
    common(ex)
    ex.set_defaults(func=_cmd_export)


def queue_from_doc(doc: dict[str, Any]) -> dict[str, Any]:
    """A queue file (snake_case, short question kinds) as AnnotationService JSON."""
    from google.protobuf import json_format

    from evalsi.v1alpha1 import annotation_service_pb2 as pb

    doc = dict(doc)
    questions = []
    for given in doc.get("questions") or []:
        q = dict(given)
        kind = str(q.get("kind", "")).lower()
        if kind in KINDS:
            q["kind"] = "QUESTION_KIND_" + kind.upper()
        questions.append(q)
    doc["questions"] = questions
    msg = json_format.ParseDict(doc, pb.AnnotationQueue())
    out: dict[str, Any] = json_format.MessageToDict(msg)
    return out


def _cmd_queues(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        queues = client.list_queues(args.project)
    for q in queues:
        print(f"{q.get('project', '')}/{q['name']}\t{q.get('description', '')}")
    return 0


def _cmd_create(args: argparse.Namespace) -> int:
    import yaml

    doc = yaml.safe_load(Path(args.file).read_text())
    if not isinstance(doc, dict):
        print(f"{args.file}: expected a mapping", file=sys.stderr)
        return 2
    with server_client(args) as client:
        q = client.create_queue(queue_from_doc(doc))
    print(f"created queue {q.get('project', '')}/{q['name']}")
    return 0


def _cmd_delete(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        client.annotation_call("DeleteQueue", {"project": args.project, "name": args.queue})
    print(f"deleted queue {args.project}/{args.queue}")
    return 0


def _cmd_add(args: argparse.Namespace) -> int:
    records = None
    if args.records:
        from google.protobuf import json_format

        from evalsi.convert import record_to_proto
        from evalsi.datasets import load_records

        records = [
            json_format.MessageToDict(record_to_proto(r)) for r in load_records(args.records)
        ]
    with server_client(args) as client:
        added = client.add_items(
            args.project,
            args.queue,
            run_id=args.run or "",
            when=args.when,
            all_trials=args.all_trials,
            records=records,
            limit=args.limit,
        )
    print(f"added {added} item(s) to {args.project}/{args.queue}")
    return 0


def render_content(content: dict[str, Any] | None) -> str:
    if not content:
        return ""
    if "text" in content:
        return str(content["text"])
    if "messages" in content:
        lines = [
            f"[{m.get('role', '')}] {m.get('content', '')}"
            for m in content["messages"].get("messages", [])
        ]
        return "\n".join(lines)
    return json.dumps(next(iter(content.values())), indent=2)


def _show(item: dict[str, Any], show_scores: bool, out: Any) -> None:
    rec = item.get("record", {})
    print(f"\n--- item {item['id']} (record {rec.get('id', '')}) ---", file=out)
    for field in ("input", "output", "reference"):
        if rec.get(field):
            print(f"{field}:\n{render_content(rec[field])}\n", file=out)
    for i, ctx in enumerate(rec.get("context", [])):
        print(f"context[{i}]:\n{render_content(ctx)}\n", file=out)
    traj = rec.get("trajectory", {}).get("steps", [])
    if traj:
        print(f"trajectory: {len(traj)} step(s)", file=out)
        for step in traj:
            kind = str(step.get("kind", "")).removeprefix("STEP_KIND_").lower()
            print(f"  {kind}: {step.get('name', '')} {step.get('content', '')}"[:200], file=out)
    if show_scores and item.get("runScores"):
        scores = ", ".join(f"{k}={v:.3g}" for k, v in sorted(item["runScores"].items()))
        print(f"run scores: {scores}", file=out)


def ask(question: dict[str, Any], prompt: Callable[[str], str]) -> dict[str, Any] | None:
    """One answer from the annotator, or None for an optional question left blank."""
    name = question["name"]
    kind = str(question.get("kind", "")).removeprefix("QUESTION_KIND_").lower()
    text = question.get("prompt") or name
    lo, hi = float(question.get("min", 0)), float(question.get("max", 0))
    options = question.get("options", [])
    hint = {
        "pass_fail": "y/n",
        "score": f"{lo:g}-{hi:g}",
        "label": " / ".join(f"{i + 1}={o}" for i, o in enumerate(options)),
        "text": "text",
    }.get(kind, "")
    optional = question.get("optional", False)
    while True:
        raw = prompt(f"{text} [{hint}{', blank to leave' if optional else ''}]: ").strip()
        if raw in ("q", ":q"):
            raise _Quit
        if raw in ("s", ":s"):
            raise _Skip
        if not raw:
            if optional:
                return None
            continue
        if kind == "pass_fail" and raw.lower() in ("y", "yes", "n", "no", "pass", "fail"):
            return {"question": name, "passed": raw.lower() in ("y", "yes", "pass")}
        if kind == "score":
            try:
                v = float(raw)
            except ValueError:
                continue
            if lo <= v <= hi:
                return {"question": name, "number": v}
            continue
        if kind == "label":
            if raw.isdigit() and 1 <= int(raw) <= len(options):
                return {"question": name, "label": options[int(raw) - 1]}
            if raw in options:
                return {"question": name, "label": raw}
            continue
        if kind == "text":
            return {"question": name, "text": raw}


def annotate_loop(
    client: Any,
    project: str,
    queue: str,
    *,
    prompt: Callable[[str], str] = input,
    out: Any = None,
    show_scores: bool = False,
    lease: int = 0,
) -> int:
    """Answer items until none are left or the annotator quits; returns how many."""
    out = out or sys.stdout
    q = client.annotation_call("GetQueue", {"project": project, "name": queue})["queue"]
    if q.get("instructions"):
        print(q["instructions"], file=out)
    print("answer each question; 's' skips the item, 'q' quits", file=out)
    done = 0
    while True:
        nxt = client.next_item(project, queue, lease_seconds=lease)
        item = nxt.get("item")
        if not item:
            print(f"\nnothing left for you in {project}/{queue} ({done} answered)", file=out)
            return done
        print(f"\n{nxt.get('remaining', '?')} left", file=out)
        _show(item, show_scores, out)
        try:
            answers = [a for qu in q.get("questions", []) if (a := ask(qu, prompt)) is not None]
            comment = prompt("comment (optional): ").strip()
        except _Skip:
            client.submit_annotation(project, queue, item["id"], [], skip=True)
            continue
        except (_Quit, EOFError, KeyboardInterrupt):
            print(f"\nstopped ({done} answered); the item is held until its lease ends", file=out)
            return done
        client.submit_annotation(project, queue, item["id"], answers, comment=comment)
        done += 1


def _cmd_start(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        annotate_loop(
            client, args.project, args.queue, show_scores=args.show_scores, lease=args.lease
        )
    return 0


def _fmt(v: Any) -> str:
    return "-" if v is None else f"{float(v):.3f}"


def _cmd_stats(args: argparse.Namespace) -> int:
    with server_client(args) as client:
        st = client.summarize_queue(args.project, args.queue)
    if args.format == "json":
        print(json.dumps(st, indent=2))
        return 0
    print(
        f"{args.project}/{args.queue}: {st.get('done', 0)}/{st.get('items', 0)} items done, "
        f"{st.get('annotations', 0)} answers, {st.get('skipped', 0)} skipped"
    )
    for who, n in sorted(st.get("annotators", {}).items()):
        print(f"  {who}: {n}")
    print(f"\n{'question':<20} {'n':>5} {'mean':>7} {'95% CI':>17} {'alpha':>7}  vs metric")
    for q in st.get("questions", []):
        s = q.get("summary", {})
        ci = s.get("ci")
        ci_s = f"[{_fmt(ci.get('low'))}, {_fmt(ci.get('high'))}]" if ci else "-"
        ag = q.get("metricAgreement")
        vs = ""
        if ag:
            parts = [f"n={ag.get('n', 0)}"]
            for key in ("accuracy", "cohenKappa", "pearson", "mae"):
                if key in ag:
                    parts.append(f"{key}={float(ag[key]):.3f}")
            vs = " ".join(parts)
        print(
            f"{q['question']:<20} {s.get('n', '-')!s:>5} {_fmt(s.get('mean')):>7} {ci_s:>17} "
            f"{_fmt(q.get('interAnnotatorAlpha')):>7}  {vs}"
        )
        for label, n in sorted(q.get("labelCounts", {}).items()):
            print(f"{'':<20} {label}: {n}")
    return 0


def _cmd_export(args: argparse.Namespace) -> int:
    n = 0
    with server_client(args) as client, open(args.out, "w", encoding="utf-8") as f:
        token = ""
        while True:
            page = client.list_annotations(args.project, args.queue, page_token=token)
            items = {it["id"]: it for it in page.get("items", [])}
            for a in page.get("annotations", []):
                f.write(json.dumps({**a, "item": items.get(a["itemId"], {})}) + "\n")
                n += 1
            token = page.get("nextPageToken", "")
            if not token:
                break
    print(f"wrote {n} annotation(s) to {args.out}", file=sys.stderr)
    return 0
