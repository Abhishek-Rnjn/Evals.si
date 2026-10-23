"""A database-backed tool environment (H4, decision 0032).

A task declares a SQLite schema and seed data, a set of named tools (each one
parameterized SQL statement, read or write, with a JSON Schema for its
parameters) and SQL assertions over the final state::

    environment:
      database:
        schema: "CREATE TABLE accounts (id TEXT PRIMARY KEY, balance INTEGER NOT NULL);"
        seed: "INSERT INTO accounts VALUES ('a', 100), ('b', 0);"
        tools:
          - name: get_balance
            description: Balance of one account.
            sql: "SELECT balance FROM accounts WHERE id = :id"
            parameters: {type: object, properties: {id: {type: string}}, required: [id]}
          - name: transfer_out
            mode: write
            sql: "UPDATE accounts SET balance = balance - :amount WHERE id = :id"
            parameters: {...}
            min_rows: 1
        assertions:
          - {name: moved, sql: "SELECT balance FROM accounts ORDER BY id", expect: [[70], [30]]}

The database is built in the harness process for each trial (an in-memory
SQLite database, no sandbox), so trials never share state and nothing the agent
does can reach the checker's queries except through the declared tools. A read
tool runs under an authorizer that allows only SELECT; a write tool may insert,
update and delete but never change the schema, attach another database or run a
pragma. A statement is one statement. Every write attempt is recorded, with its
arguments and the number of rows it changed, on its tool step (``evalsi.tool.db_*``)
and in ``metadata.database`` with a digest of the final state. A write that changes
fewer rows than ``min_rows`` or more than ``max_rows`` is rolled back and reported to
the agent as an error, so a task can refuse a transfer that matched no account.
"""

from __future__ import annotations

import hashlib
import json
import re
import sqlite3
from typing import Any

from evalsi.types import TaskCheck
from evalsi_harness.models import ToolSpec
from evalsi_harness.task import DatabaseConfig
from evalsi_harness.tools import ToolResult

MAX_ROWS = 100
MAX_RECORDED_WRITES = 200
MAX_STEPS = 20_000  # progress callbacks (1,000 VM steps each) a statement may take
_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_-]{0,63}$")

_READ_OK = {
    sqlite3.SQLITE_SELECT,
    sqlite3.SQLITE_READ,
    sqlite3.SQLITE_FUNCTION,
    sqlite3.SQLITE_RECURSIVE,
}
_WRITE_OK = _READ_OK | {sqlite3.SQLITE_INSERT, sqlite3.SQLITE_UPDATE, sqlite3.SQLITE_DELETE}


def _read_only(action: int, *_: Any) -> int:
    return sqlite3.SQLITE_OK if action in _READ_OK else sqlite3.SQLITE_DENY


def _writable(action: int, *_: Any) -> int:
    return sqlite3.SQLITE_OK if action in _WRITE_OK else sqlite3.SQLITE_DENY


# --- a small JSON Schema check, so the harness needs no schema library ---


def check_schema(value: Any, schema: dict[str, Any], path: str = "arguments") -> str:
    """The first way ``value`` fails ``schema`` (type, enum, required, properties,
    items, bounds, additionalProperties), or "" when it holds."""
    kind = schema.get("type")
    kinds = {
        "string": str,
        "boolean": bool,
        "array": list,
        "object": dict,
        "null": type(None),
    }
    if kind == "integer":
        if isinstance(value, bool) or not (
            isinstance(value, int) or (isinstance(value, float) and value.is_integer())
        ):
            return f"{path} must be an integer"
    elif kind == "number":
        if isinstance(value, bool) or not isinstance(value, int | float):
            return f"{path} must be a number"
    elif isinstance(kind, str) and kind in kinds and not isinstance(value, kinds[kind]):
        return f"{path} must be of type {kind}"
    if "enum" in schema and value not in schema["enum"]:
        return f"{path} must be one of {schema['enum']}"
    if isinstance(value, int | float) and not isinstance(value, bool):
        if "minimum" in schema and value < schema["minimum"]:
            return f"{path} must be at least {schema['minimum']}"
        if "maximum" in schema and value > schema["maximum"]:
            return f"{path} must be at most {schema['maximum']}"
    if isinstance(value, str):
        if "minLength" in schema and len(value) < schema["minLength"]:
            return f"{path} is shorter than {schema['minLength']}"
        if "maxLength" in schema and len(value) > schema["maxLength"]:
            return f"{path} is longer than {schema['maxLength']}"
        if "pattern" in schema and not re.search(schema["pattern"], value):
            return f"{path} does not match {schema['pattern']!r}"
    if isinstance(value, dict):
        props = schema.get("properties") or {}
        for name in schema.get("required") or []:
            if name not in value:
                return f"{path}.{name} is required"
        for name, item in value.items():
            if name in props:
                if problem := check_schema(item, props[name], f"{path}.{name}"):
                    return problem
            elif schema.get("additionalProperties") is False:
                return f"{path}.{name} is not a known parameter"
    if isinstance(value, list) and isinstance(schema.get("items"), dict):
        for i, item in enumerate(value):
            if problem := check_schema(item, schema["items"], f"{path}[{i}]"):
                return problem
    return ""


def _bound(value: Any) -> Any:
    if isinstance(value, bool) or (isinstance(value, float) and value.is_integer()):
        return int(value)
    if isinstance(value, dict | list):
        return json.dumps(value, sort_keys=True)
    return value


def _bind(args: dict[str, Any]) -> dict[str, Any]:
    return {key: _bound(value) for key, value in args.items()}


class Database:
    """One trial's database: built from the config, queried by tools, graded by assertions."""

    def __init__(self, config: DatabaseConfig) -> None:
        self.config = config
        self.conn = sqlite3.connect(":memory:", isolation_level=None, check_same_thread=False)
        self.conn.executescript(config.schema)
        if config.seed.strip():
            self.conn.executescript(config.seed)
        self.writes: list[dict[str, Any]] = []
        self.write_count = 0
        self.initial_digest = self.digest()

    def close(self) -> None:
        self.conn.close()

    def digest(self) -> str:
        """A hash of the whole database (schema and rows), for comparing end states."""
        h = hashlib.sha256()
        for line in self.conn.iterdump():
            h.update(line.encode())
            h.update(b"\n")
        return h.hexdigest()

    def _run(
        self, sql: str, params: dict[str, Any], *, write: bool
    ) -> tuple[list[dict[str, Any]], int, bool]:
        """(rows, rows changed, rows were cut). Runs under the authorizer and a step limit."""
        steps = 0

        def progress() -> int:
            nonlocal steps
            steps += 1
            return 1 if steps > MAX_STEPS else 0

        self.conn.set_authorizer(_writable if write else _read_only)
        self.conn.set_progress_handler(progress, 1000)
        try:
            cur = self.conn.execute(sql, _bind(params))
            names = [d[0] for d in cur.description] if cur.description else []
            fetched = cur.fetchmany(MAX_ROWS + 1)
            cut = len(fetched) > MAX_ROWS
            rows = [dict(zip(names, row, strict=True)) for row in fetched[:MAX_ROWS]]
            changed = cur.rowcount if write and cur.rowcount > 0 else 0
            return rows, changed, cut
        finally:
            self.conn.set_authorizer(None)
            self.conn.set_progress_handler(None, 0)

    def call(self, tool: Any, args: dict[str, Any]) -> ToolResult:
        write = tool.mode == "write"
        meta: dict[str, Any] = {}
        problem = check_schema(args, tool.parameters)
        if problem:
            return self._refused(tool, args, problem, write, meta)
        self.conn.execute("SAVEPOINT tool")
        try:
            rows, changed, cut = self._run(tool.sql, args, write=write)
            if write and (changed < tool.min_rows or (tool.max_rows and changed > tool.max_rows)):
                want = f"at least {tool.min_rows}" + (
                    f" and at most {tool.max_rows}" if tool.max_rows else ""
                )
                raise _RowCount(f"the statement changed {changed} rows; it must change {want}")
        except (sqlite3.Error, _RowCount) as exc:
            self.conn.execute("ROLLBACK TO tool")
            self.conn.execute("RELEASE tool")
            return self._refused(tool, args, str(exc), write, meta)
        self.conn.execute("RELEASE tool")
        if write:
            self._record(tool.name, args, changed, applied=True, error="")
            meta = {"db_write": self.write_count, "db_rows_changed": changed}
            body: dict[str, Any] = {"rows_changed": changed}
            if rows:
                body["rows"] = rows
            return ToolResult(json.dumps(body, default=str), metadata=meta)
        body = {"rows": rows, "row_count": len(rows)}
        if cut:
            body["truncated"] = f"only the first {MAX_ROWS} rows are shown"
        return ToolResult(json.dumps(body, default=str))

    def _refused(
        self, tool: Any, args: dict[str, Any], why: str, write: bool, meta: dict[str, Any]
    ) -> ToolResult:
        if write:
            self._record(tool.name, args, 0, applied=False, error=why)
            meta = {"db_write": self.write_count, "db_rows_changed": 0, "db_applied": False}
        return ToolResult(f"{tool.name}: {why}", is_error=True, metadata=meta)

    def _record(
        self, tool: str, args: dict[str, Any], changed: int, *, applied: bool, error: str
    ) -> None:
        self.write_count += 1
        if len(self.writes) < MAX_RECORDED_WRITES:
            self.writes.append(
                {
                    "seq": self.write_count,
                    "tool": tool,
                    "arguments": args,
                    "rows_changed": changed,
                    "applied": applied,
                    **({"error": error} if error else {}),
                }
            )

    def check(self) -> TaskCheck:
        """Run the assertions against the final state."""
        tests: dict[str, str] = {}
        failures: list[str] = []
        for a in self.config.assertions:
            try:
                rows, _, _ = self._run(a.sql, {}, write=False)
            except sqlite3.Error as exc:
                return TaskCheck(passed=False, error=f"assertion {a.name!r}: {exc}")
            got: Any = [list(r.values()) for r in rows]
            want = a.expect
            if not isinstance(want, list):  # one value: one row of one column
                got = got[0][0] if len(got) == 1 and len(got[0]) == 1 else got
            if got == want:
                tests[a.name] = "passed"
            else:
                tests[a.name] = "failed"
                failures.append(f"{a.name}: expected {json.dumps(want)}, got {json.dumps(got)}")
        passed = sum(v == "passed" for v in tests.values())
        return TaskCheck(
            passed=bool(tests) and passed == len(tests),
            score=passed / len(tests) if tests else None,
            tests=tests,
            details="; ".join(failures)[:2000] or f"{passed} of {len(tests)} assertions held",
        )

    def summary(self) -> dict[str, Any]:
        return {
            "writes": self.writes,
            "write_count": self.write_count,
            "writes_omitted": max(0, self.write_count - len(self.writes)),
            "initial_digest": self.initial_digest,
            "final_digest": self.digest(),
        }


class _RowCount(Exception):
    pass


class DatabaseTool:
    """A declared SQL statement as a tool."""

    reexecute = True  # a replayed run must rebuild the final state

    def __init__(self, db: Database, config: Any) -> None:
        self.db = db
        self.sql = config.sql
        self.mode = config.mode
        self.min_rows = config.min_rows
        self.max_rows = config.max_rows
        self.parameters = config.parameters or {"type": "object", "properties": {}}
        self.name = config.name
        self.spec = ToolSpec(
            name=config.name,
            description=config.description
            or ("Changes the database." if config.mode == "write" else "Reads the database."),
            input_schema=self.parameters,
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        return self.db.call(self, args)


def _unbound(sql: str) -> dict[str, None]:
    return {name: None for name in re.findall(r":([A-Za-z_]\w*)", sql)}


def validate(config: DatabaseConfig) -> None:
    """Raise ValueError if the schema, seed, statements or assertions do not hold together."""
    names = set()
    for t in config.tools:
        if not _NAME.match(t.name):
            raise ValueError(f"tool name {t.name!r} must be letters, digits, _ and -")
        if t.name in names:
            raise ValueError(f"duplicate database tool {t.name!r}")
        names.add(t.name)
        if t.mode not in ("read", "write"):
            raise ValueError(f"tool {t.name}: mode must be read or write")
        if not t.sql.strip():
            raise ValueError(f"tool {t.name}: sql is empty")
        if (t.parameters or {}).get("type", "object") != "object":
            raise ValueError(f"tool {t.name}: parameters must be an object schema")
        if t.max_rows and t.max_rows < t.min_rows:
            raise ValueError(f"tool {t.name}: max_rows is below min_rows")
    for a in config.assertions:
        if not a.name or not a.sql.strip():
            raise ValueError("every assertion needs a name and sql")
    try:
        db = Database(config)
    except sqlite3.Error as exc:
        raise ValueError(f"schema or seed: {exc}") from exc
    try:
        for t in config.tools:
            try:
                db.conn.set_authorizer(_writable if t.mode == "write" else _read_only)
                db.conn.execute("EXPLAIN " + t.sql, _unbound(t.sql))
            except sqlite3.Error as exc:
                raise ValueError(f"tool {t.name}: {exc}") from exc
            finally:
                db.conn.set_authorizer(None)
        for a in config.assertions:
            try:
                db.conn.set_authorizer(_read_only)
                db.conn.execute("EXPLAIN " + a.sql)
            except sqlite3.Error as exc:
                raise ValueError(f"assertion {a.name}: {exc}") from exc
            finally:
                db.conn.set_authorizer(None)
    finally:
        db.close()
