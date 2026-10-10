"""Builds the small-repo-fixes suite: small SWE-bench-style tasks.

Each task is a tiny Python repository with one bug, an issue-style
instruction, and a hidden test the agent never sees. The suite is written as
``small-repo-fixes.jsonl`` (one record per task, with its environment and
checker in ``metadata.environment``, the form benchmark importers use) and the
mock model's ``solutions.json`` entries for them.

    python3 build_small_repo_fixes.py            # rewrite the two files
    python3 build_small_repo_fixes.py --check    # verify them, writing nothing

Verification runs every task twice in a temporary directory: the checker must
fail on the buggy repository and pass once the reference fix is applied. It is
what keeps the suite honest, and CI runs it.
"""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import textwrap
from pathlib import Path

HERE = Path(__file__).parent
SETUP = ["git init -q && git add -A && git -c user.email=e@x -c user.name=e commit -qm base"]


def d(text: str) -> str:
    return textwrap.dedent(text).lstrip("\n")


# Each task: id, the buggy files, the issue, the hidden test, and a shell
# command that fixes it (what the mock model plays; a real agent finds its own).
TASKS: list[dict] = [
    {
        "id": "calc",
        "issue": "calc.py has a bug: add(2, 3) returns -1, and mul(2, 3) returns 5. Fix both.",
        "files": {"calc.py": d("""
            def add(a, b):
                return a - b


            def mul(a, b):
                return a + b
        """)},
        "test": "from calc import add, mul\nassert add(2, 3) == 5 and mul(2, 3) == 6\n",
        "fix": "sed -i.orig '/def mul/,$ s/return a + b/return a * b/' calc.py && sed -i.orig 's/return a - b/return a + b/' calc.py && rm -f *.orig",
    },
    {
        "id": "range-sum",
        "issue": "mathx.range_sum(1, 4) should be 10 (1+2+3+4, both ends included) but it returns 6.",
        "files": {"mathx.py": d('''
            def range_sum(lo, hi):
                """The sum of the integers from lo to hi, both included."""
                return sum(range(lo, hi))
        ''')},
        "test": "from mathx import range_sum\nassert range_sum(1, 4) == 10 and range_sum(5, 5) == 5\n",
        "fix": "sed -i.orig 's/range(lo, hi)/range(lo, hi + 1)/' mathx.py && rm -f *.orig",
    },
    {
        "id": "slugify",
        "issue": "textutil.slugify('  Hello, World!  ') returns 'hello-world-' but should return 'hello-world' (no leading or trailing hyphen).",
        "files": {"textutil.py": d('''
            import re


            def slugify(text):
                """Lowercase, with runs of non-alphanumerics turned into single hyphens."""
                return re.sub(r"[^a-z0-9]+", "-", text.lower())
        ''')},
        "test": "from textutil import slugify\nassert slugify('  Hello, World!  ') == 'hello-world'\nassert slugify('a--b') == 'a-b'\n",
        "fix": "sed -i.orig 's/return re.sub(r\"\\[^a-z0-9\\]+\", \"-\", text.lower())/return re.sub(r\"[^a-z0-9]+\", \"-\", text.lower()).strip(\"-\")/' textutil.py && rm -f *.orig",
    },
    {
        "id": "parse-duration",
        "issue": "timeparse.parse_duration('1h30m') should be 5400 seconds but it returns 3600: minutes are ignored when hours are present.",
        "files": {"timeparse.py": d('''
            import re

            UNITS = {"h": 3600, "m": 60, "s": 1}


            def parse_duration(text):
                """Seconds in a duration like '1h30m', '45s' or '2h'."""
                match = re.fullmatch(r"(?:(\\d+)h)?(?:(\\d+)m)?(?:(\\d+)s)?", text)
                if not match or not text:
                    raise ValueError(text)
                hours, minutes, seconds = (int(g) if g else 0 for g in match.groups())
                if hours:
                    return hours * UNITS["h"]
                return minutes * UNITS["m"] + seconds * UNITS["s"]
        ''')},
        "test": "from timeparse import parse_duration\nassert parse_duration('1h30m') == 5400 and parse_duration('45s') == 45 and parse_duration('2h') == 7200\n",
        "fix": "python3 - <<'PY'\nimport re\ns = open('timeparse.py').read()\ns = s.replace('    if hours:\\n        return hours * UNITS[\"h\"]\\n    return minutes', '    return hours * UNITS[\"h\"] + minutes')\nopen('timeparse.py', 'w').write(s)\nPY",
    },
    {
        "id": "dedupe",
        "issue": "listutil.dedupe([3, 1, 3, 2, 1]) should keep the first occurrence of each item in order, [3, 1, 2], but the order comes back scrambled.",
        "files": {"listutil.py": d('''
            def dedupe(items):
                """The items without repeats, in order of first appearance."""
                return list(set(items))
        ''')},
        "test": "from listutil import dedupe\nassert dedupe([3, 1, 3, 2, 1]) == [3, 1, 2] and dedupe([]) == []\n",
        "fix": "sed -i.orig 's/return list(set(items))/return list(dict.fromkeys(items))/' listutil.py && rm -f *.orig",
    },
    {
        "id": "stack-empty",
        "issue": "Popping from an empty stack.Stack should raise stack.EmptyStackError, but it raises IndexError. Define EmptyStackError (a subclass of Exception) and raise it.",
        "files": {"stack.py": d('''
            class Stack:
                def __init__(self):
                    self._items = []

                def push(self, item):
                    self._items.append(item)

                def pop(self):
                    return self._items.pop()

                def __len__(self):
                    return len(self._items)
        ''')},
        "test": "import stack\ns = stack.Stack()\ns.push(1)\nassert s.pop() == 1\ntry:\n    s.pop()\nexcept stack.EmptyStackError:\n    pass\nelse:\n    raise AssertionError('no EmptyStackError')\n",
        "fix": "python3 - <<'PY'\ns = open('stack.py').read()\ns = 'class EmptyStackError(Exception):\\n    pass\\n\\n\\n' + s\ns = s.replace('        return self._items.pop()', '        if not self._items:\\n            raise EmptyStackError()\\n        return self._items.pop()')\nopen('stack.py', 'w').write(s)\nPY",
    },
    {
        "id": "median-even",
        "issue": "stats.median([1, 2, 3, 4]) should be 2.5 (the mean of the two middle values) but returns 3.",
        "files": {"stats.py": d('''
            def median(values):
                """The middle value; the mean of the two middle values for an even count."""
                ordered = sorted(values)
                return ordered[len(ordered) // 2]
        ''')},
        "test": "from stats import median\nassert median([1, 2, 3, 4]) == 2.5 and median([3, 1, 2]) == 2\n",
        "fix": "python3 - <<'PY'\ns = open('stats.py').read()\ns = s.replace('    return ordered[len(ordered) // 2]', '    mid = len(ordered) // 2\\n    if len(ordered) % 2:\\n        return ordered[mid]\\n    return (ordered[mid - 1] + ordered[mid]) / 2')\nopen('stats.py', 'w').write(s)\nPY",
    },
    {
        "id": "word-count",
        "issue": "wordcount.count_words('The the THE cat') should return {'the': 3, 'cat': 1}: words are counted without regard to case, but the keys come back as written.",
        "files": {"wordcount.py": d('''
            def count_words(text):
                """How many times each word appears, ignoring case."""
                counts = {}
                for word in text.split():
                    counts[word] = counts.get(word, 0) + 1
                return counts
        ''')},
        "test": "from wordcount import count_words\nassert count_words('The the THE cat') == {'the': 3, 'cat': 1}\n",
        "fix": "sed -i.orig 's/for word in text.split():/for word in text.lower().split():/' wordcount.py && rm -f *.orig",
    },
    {
        "id": "palindrome",
        "issue": "text.is_palindrome('A man, a plan, a canal: Panama') should be True (punctuation, spaces and case are ignored) but returns False.",
        "files": {"text.py": d('''
            def is_palindrome(s):
                """True if s reads the same both ways, ignoring case and anything but letters and digits."""
                return s == s[::-1]
        ''')},
        "test": "from text import is_palindrome\nassert is_palindrome('A man, a plan, a canal: Panama') and not is_palindrome('abc') and is_palindrome('')\n",
        "fix": "python3 - <<'PY'\ns = open('text.py').read()\ns = s.replace('    return s == s[::-1]', '    kept = [c.lower() for c in s if c.isalnum()]\\n    return kept == kept[::-1]')\nopen('text.py', 'w').write(s)\nPY",
    },
    {
        "id": "chunk-tail",
        "issue": "batching.chunk([1, 2, 3, 4, 5], 2) should be [[1, 2], [3, 4], [5]] but the last, shorter chunk is dropped.",
        "files": {"batching.py": d('''
            def chunk(items, size):
                """Consecutive lists of at most `size` items."""
                return [items[i : i + size] for i in range(0, len(items) - size + 1, size)]
        ''')},
        "test": "from batching import chunk\nassert chunk([1, 2, 3, 4, 5], 2) == [[1, 2], [3, 4], [5]] and chunk([], 3) == [] and chunk([1, 2], 2) == [[1, 2]]\n",
        "fix": "sed -i.orig 's/range(0, len(items) - size + 1, size)/range(0, len(items), size)/' batching.py && rm -f *.orig",
    },
    {
        "id": "merge-nested",
        "issue": "config.merge({'a': {'x': 1, 'y': 2}}, {'a': {'y': 3}}) should be {'a': {'x': 1, 'y': 3}} (nested dicts are merged) but the whole 'a' entry is replaced.",
        "files": {"config.py": d('''
            def merge(base, override):
                """A new dict with override's entries on top of base's; nested dicts are merged."""
                out = dict(base)
                out.update(override)
                return out
        ''')},
        "test": "from config import merge\nassert merge({'a': {'x': 1, 'y': 2}}, {'a': {'y': 3}}) == {'a': {'x': 1, 'y': 3}}\nassert merge({'a': 1}, {'b': 2}) == {'a': 1, 'b': 2}\n",
        "fix": "python3 - <<'PY'\ns = open('config.py').read()\ns = s.replace('    out.update(override)', '    for key, value in override.items():\\n        if isinstance(value, dict) and isinstance(out.get(key), dict):\\n            out[key] = merge(out[key], value)\\n        else:\\n            out[key] = value')\nopen('config.py', 'w').write(s)\nPY",
    },
]


def record(task: dict) -> dict:
    return {
        "id": task["id"],
        "input": task["issue"],
        "metadata": {
            "environment": {
                "files": task["files"],
                "setup": SETUP,
                "checker": {"files": {"test_hidden.py": task["test"]}, "command": ["python3", "-B", "test_hidden.py"]},
            }
        },
    }


def solution(task: dict) -> dict:
    # The mock model recognises a task by the start of its issue.
    return {"match": task["issue"][:60], "command": task["fix"]}


def verify() -> list[str]:
    """Problems with the suite: a checker that passes on the bug, or fails on the fix."""
    problems = []
    for task in TASKS:
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            for name, content in task["files"].items():
                (root / name).write_text(content)
            (root / "test_hidden.py").write_text(task["test"])
            before = subprocess.run(["python3", "-B", "test_hidden.py"], cwd=root, capture_output=True, check=False)
            if before.returncode == 0:
                problems.append(f"{task['id']}: the checker passes on the buggy repository")
            fixed = subprocess.run(["sh", "-c", task["fix"]], cwd=root, capture_output=True, check=False)
            after = subprocess.run(["python3", "-B", "test_hidden.py"], cwd=root, capture_output=True, check=False)
            if fixed.returncode != 0 or after.returncode != 0:
                detail = (fixed.stderr + after.stderr).decode()[-300:]
                problems.append(f"{task['id']}: the reference fix does not make the checker pass: {detail}")
    return problems


def render() -> tuple[str, list[dict]]:
    jsonl = "".join(json.dumps(record(t), ensure_ascii=False) + "\n" for t in TASKS)
    return jsonl, [solution(t) for t in TASKS]


def main() -> int:
    problems = verify()
    for p in problems:
        print(p, file=sys.stderr)
    if problems:
        return 1
    jsonl, solutions = render()
    target = HERE / "small-repo-fixes.jsonl"
    sol_target = HERE.parent / "mock-model" / "solutions.json"
    if "--check" in sys.argv:
        stale = [p.name for p, want in ((target, jsonl),) if not p.exists() or p.read_text() != want]
        if stale:
            print(f"out of date: {', '.join(stale)}; run build_small_repo_fixes.py", file=sys.stderr)
            return 1
        return 0
    target.write_text(jsonl)
    # solutions.json keeps hand-written entries (the research question); ours are marked.
    existing = json.loads(sol_target.read_text()) if sol_target.exists() else []
    kept = [e for e in existing if not e.get("suite") == "small-repo-fixes"]
    ours = [{**s, "suite": "small-repo-fixes"} for s in solutions]
    sol_target.write_text(json.dumps(kept + ours, indent=2, ensure_ascii=False) + "\n")
    print(f"wrote {target.name} ({len(TASKS)} tasks) and {len(ours)} mock solutions")
    return 0


if __name__ == "__main__":
    sys.exit(main())
