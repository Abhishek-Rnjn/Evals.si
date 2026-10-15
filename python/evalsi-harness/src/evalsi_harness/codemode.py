"""Code mode: the agent writes a script, the script calls the tools.

One round trip per tool call is slow and fills the context with every
intermediate result. In code mode the model sees a single tool, ``run_code``,
and writes a short Python script that calls the task's tools as functions,
loops over them, filters what they return and ``print``s only what it wants
back. The script's output is the tool result.

The script is **not** run by Python. It is parsed and interpreted by the small
evaluator below, which allows a safe subset: assignments, ``if``, ``for``,
``while``, ``try``, comprehensions, f-strings, small functions and lambdas,
the usual string, list and dict methods, ``json`` and ``re``. There is no
``open``, ``eval``, ``exec``, ``getattr``, no imports beyond ``json`` and
``re``, and no attribute starting with an underscore, so a script can reach
nothing but the tools it was given, and the model's code never runs on the
worker. Every tool call inside it goes through the same path as a direct one,
so each is a step on the trajectory (under the ``run_code`` step), hooks apply
to it, and it is recorded and replayed. Operations, tool calls and output are
capped.
"""

from __future__ import annotations

import ast
import json
import re
from collections.abc import Awaitable, Callable
from typing import Any

from evalsi_harness.models import ToolSpec
from evalsi_harness.tools import OUTPUT_LIMIT, ToolResult, _clip

MAX_OPERATIONS = 200_000
MAX_TOOL_CALLS = 100
MAX_SOURCE = 20_000
MAX_RANGE = 100_000
MAX_SEQUENCE = 1_000_000
MAX_DEPTH = 40

ToolFn = Callable[[dict[str, Any]], Awaitable[ToolResult]]


class CodeError(Exception):
    """The script is wrong or failed; it carries the line."""


class Limit(BaseException):
    """A resource limit. Not an Exception, so a script's try/except cannot hide it."""


class _Break(BaseException):
    pass


class _Continue(BaseException):
    pass


class _Return(BaseException):
    def __init__(self, value: Any) -> None:
        self.value = value


class _Json:
    """The ``json`` module, as far as scripts may use it."""

    loads = staticmethod(json.loads)

    @staticmethod
    def dumps(value: Any, indent: int | None = None) -> str:
        return json.dumps(value, indent=indent, default=str)


class _Re:
    search = staticmethod(re.search)
    match = staticmethod(re.match)
    findall = staticmethod(re.findall)
    sub = staticmethod(re.sub)
    split = staticmethod(re.split)
    IGNORECASE = re.IGNORECASE
    MULTILINE = re.MULTILINE


_STR = """lower upper strip lstrip rstrip split splitlines join startswith endswith replace find
count zfill title capitalize isdigit isalpha isalnum isspace ljust rjust partition rpartition
removeprefix removesuffix"""

METHODS: dict[type, frozenset[str]] = {
    str: frozenset(_STR.split()),
    list: frozenset({"append", "extend", "pop", "index", "count", "sort", "reverse", "copy"})
    | {"insert", "remove"},
    dict: frozenset({"get", "keys", "values", "items", "update", "setdefault", "pop", "copy"}),
    set: frozenset({"add", "update", "discard", "union", "intersection", "difference"}),
    tuple: frozenset({"index", "count"}),
    re.Match: frozenset({"group", "groups", "groupdict", "start", "end", "span"}),
    _Json: frozenset({"loads", "dumps"}),
    _Re: frozenset({"search", "match", "findall", "sub", "split", "IGNORECASE", "MULTILINE"}),
}

BUILTINS: dict[str, Any] = {
    "len": len, "range": range, "enumerate": enumerate, "zip": zip, "sorted": sorted,
    "reversed": reversed, "min": min, "max": max, "sum": sum, "any": any, "all": all,
    "abs": abs, "round": round, "str": str, "int": int, "float": float, "bool": bool,
    "list": list, "dict": dict, "set": set, "tuple": tuple, "isinstance": isinstance,
    "True": True, "False": False, "None": None, "json": _Json, "re": _Re,
    "Exception": Exception, "ValueError": ValueError, "KeyError": KeyError, "print": print,
}  # fmt: skip

_BINOPS: dict[type, Callable[[Any, Any], Any]] = {
    ast.Add: lambda a, b: a + b, ast.Sub: lambda a, b: a - b, ast.Mult: lambda a, b: a * b,
    ast.Div: lambda a, b: a / b, ast.FloorDiv: lambda a, b: a // b, ast.Mod: lambda a, b: a % b,
    ast.Pow: lambda a, b: a**b, ast.BitAnd: lambda a, b: a & b, ast.BitOr: lambda a, b: a | b,
    ast.BitXor: lambda a, b: a ^ b,
}  # fmt: skip
_CMPOPS: dict[type, Callable[[Any, Any], bool]] = {
    ast.Eq: lambda a, b: a == b, ast.NotEq: lambda a, b: a != b, ast.Lt: lambda a, b: a < b,
    ast.LtE: lambda a, b: a <= b, ast.Gt: lambda a, b: a > b, ast.GtE: lambda a, b: a >= b,
    ast.In: lambda a, b: a in b, ast.NotIn: lambda a, b: a not in b,
    ast.Is: lambda a, b: a is b, ast.IsNot: lambda a, b: a is not b,
}  # fmt: skip


class _ToolCall:
    """A tool as a function in a script."""

    def __init__(self, interp: Interpreter, name: str) -> None:
        self.interp, self.name = interp, name

    async def __call__(self, *args: Any, **kwargs: Any) -> str:
        interp, name = self.interp, self.name
        properties = list((interp.specs[name].input_schema.get("properties") or {}).keys())
        if len(args) > len(properties):
            raise TypeError(f"{name} takes at most {len(properties)} positional arguments")
        arguments = dict(zip(properties, args, strict=False))
        overlap = set(arguments) & set(kwargs)
        if overlap:
            raise TypeError(f"{name} got {sorted(overlap)} twice")
        arguments.update(kwargs)
        interp.calls += 1
        if interp.calls > MAX_TOOL_CALLS:
            raise Limit(f"the script made more than {MAX_TOOL_CALLS} tool calls")
        result = await interp.tools[name](arguments)
        return f"ERROR: {result.content}" if result.is_error else result.content


class _Function:
    """A function or lambda a script defined."""

    def __init__(self, name: str, args: ast.arguments, body: list[ast.stmt] | ast.expr) -> None:
        self.name, self.args, self.body = name, args, body


class Interpreter:
    def __init__(self, tools: dict[str, ToolFn], specs: dict[str, ToolSpec]) -> None:
        self.tools = tools
        self.specs = specs
        self.out: list[str] = []
        self.out_chars = 0
        self.ops = 0
        self.calls = 0
        self.depth = 0
        self.line = 0

    # --- running ---

    async def run(self, source: str) -> str:
        if len(source) > MAX_SOURCE:
            raise CodeError(f"the script is longer than {MAX_SOURCE} characters")
        try:
            tree = ast.parse(source)
        except SyntaxError as exc:
            raise CodeError(f"line {exc.lineno}: syntax error: {exc.msg}") from exc
        env: dict[str, Any] = {}
        try:
            await self.block(tree.body, env)
        except (_Break, _Continue):
            raise CodeError("break or continue outside a loop") from None
        except _Return:
            pass
        except CodeError:
            raise
        except Limit:
            raise
        except Exception as exc:
            raise CodeError(f"line {self.line}: {type(exc).__name__}: {exc}") from exc
        return "".join(self.out)

    def tick(self, node: ast.AST) -> None:
        self.ops += 1
        self.line = getattr(node, "lineno", self.line)
        if self.ops > MAX_OPERATIONS:
            raise Limit(f"the script did too much work (more than {MAX_OPERATIONS} operations)")

    def say(self, text: str) -> None:
        self.out_chars += len(text)
        if self.out_chars > 4 * OUTPUT_LIMIT:
            raise Limit("the script printed too much")
        self.out.append(text)

    # --- statements ---

    async def block(self, body: list[ast.stmt], env: dict[str, Any]) -> None:
        for stmt in body:
            await self.stmt(stmt, env)

    async def stmt(self, node: ast.stmt, env: dict[str, Any]) -> None:
        self.tick(node)
        if isinstance(node, ast.Expr):
            await self.ev(node.value, env)
        elif isinstance(node, ast.Assign):
            value = await self.ev(node.value, env)
            for target in node.targets:
                await self.assign(target, value, env)
        elif isinstance(node, ast.AugAssign):
            current = await self.ev(node.target, env)
            value = self.binop(node.op, current, await self.ev(node.value, env))
            await self.assign(node.target, value, env)
        elif isinstance(node, ast.If):
            await self.block(node.body if await self.ev(node.test, env) else node.orelse, env)
        elif isinstance(node, ast.For):
            await self.loop_for(node, env)
        elif isinstance(node, ast.While):
            while await self.ev(node.test, env):
                self.tick(node)
                try:
                    await self.block(node.body, env)
                except _Break:
                    break
                except _Continue:
                    continue
        elif isinstance(node, ast.Try):
            await self.try_(node, env)
        elif isinstance(node, ast.FunctionDef):
            if node.args.vararg or node.args.kwarg or node.decorator_list:
                raise CodeError(f"line {node.lineno}: functions take plain arguments only")
            env[node.name] = _Function(node.name, node.args, node.body)
        elif isinstance(node, ast.Return):
            raise _Return(await self.ev(node.value, env) if node.value else None)
        elif isinstance(node, ast.Break):
            raise _Break
        elif isinstance(node, ast.Continue):
            raise _Continue
        elif isinstance(node, ast.Pass):
            return
        elif isinstance(node, ast.Import):
            for alias in node.names:
                if alias.name not in ("json", "re"):
                    raise CodeError(f"line {node.lineno}: only json and re can be imported")
        elif isinstance(node, ast.Raise):
            exc = await self.ev(node.exc, env) if node.exc else Exception("raised")
            raise exc if isinstance(exc, BaseException) else Exception(str(exc))
        elif isinstance(node, ast.Assert):
            if not await self.ev(node.test, env):
                msg = await self.ev(node.msg, env) if node.msg else "assertion failed"
                raise AssertionError(msg)
        else:
            raise CodeError(f"line {node.lineno}: {type(node).__name__} is not supported")

    async def loop_for(self, node: ast.For, env: dict[str, Any]) -> None:
        iterable = await self.ev(node.iter, env)
        for item in iterable:
            self.tick(node)
            await self.assign(node.target, item, env)
            try:
                await self.block(node.body, env)
            except _Break:
                break
            except _Continue:
                continue
        else:
            await self.block(node.orelse, env)

    async def try_(self, node: ast.Try, env: dict[str, Any]) -> None:
        try:
            await self.block(node.body, env)
        except Exception as exc:
            for handler in node.handlers:
                if handler.type is not None:
                    wanted = await self.ev(handler.type, env)
                    kinds = wanted if isinstance(wanted, tuple) else (wanted,)
                    if not any(isinstance(k, type) and isinstance(exc, k) for k in kinds):
                        continue
                if handler.name:
                    env[handler.name] = exc
                await self.block(handler.body, env)
                break
            else:
                raise
        else:
            await self.block(node.orelse, env)
        finally:
            await self.block(node.finalbody, env)

    async def assign(self, target: ast.expr, value: Any, env: dict[str, Any]) -> None:
        if isinstance(target, ast.Name):
            if target.id in self.tools or target.id in BUILTINS:
                raise CodeError(f"line {target.lineno}: {target.id} is a tool or builtin name")
            env[target.id] = value
        elif isinstance(target, ast.Tuple | ast.List):
            items = list(value)
            if len(items) != len(target.elts):
                raise ValueError(f"cannot unpack {len(items)} values into {len(target.elts)}")
            for t, v in zip(target.elts, items, strict=True):
                await self.assign(t, v, env)
        elif isinstance(target, ast.Subscript):
            container = await self.ev(target.value, env)
            container[await self.ev(target.slice, env)] = value
        else:
            raise CodeError(f"line {target.lineno}: cannot assign to that")

    # --- expressions ---

    def binop(self, op: ast.operator, a: Any, b: Any) -> Any:
        fn = _BINOPS.get(type(op))
        if fn is None:
            raise CodeError(f"line {self.line}: operator {type(op).__name__} is not supported")
        if isinstance(op, ast.Pow) and (not isinstance(b, int | float) or abs(b) > 64):
            raise Limit("an exponent above 64 is not allowed")
        repeats = isinstance(op, ast.Mult) and isinstance(a, str | list | tuple)
        if repeats and isinstance(b, int) and len(a) * max(b, 0) > MAX_SEQUENCE:
            raise Limit("that sequence would be too large")
        return fn(a, b)

    async def ev(self, node: ast.expr | None, env: dict[str, Any]) -> Any:
        if node is None:
            return None
        self.tick(node)
        method = getattr(self, f"ev_{type(node).__name__}", None)
        if method is None:
            raise CodeError(f"line {node.lineno}: {type(node).__name__} is not supported")
        return await method(node, env)

    async def ev_Constant(self, node: ast.Constant, env: dict[str, Any]) -> Any:
        return node.value

    async def ev_Name(self, node: ast.Name, env: dict[str, Any]) -> Any:
        if node.id.startswith("__"):
            raise CodeError(f"line {node.lineno}: names starting with __ are not allowed")
        if node.id in env:
            return env[node.id]
        if node.id in BUILTINS:
            return BUILTINS[node.id]
        if node.id in self.tools:
            return _ToolCall(self, node.id)
        raise NameError(f"name {node.id!r} is not defined")

    async def ev_List(self, node: ast.List, env: dict[str, Any]) -> Any:
        return [await self.ev(e, env) for e in node.elts]

    async def ev_Tuple(self, node: ast.Tuple, env: dict[str, Any]) -> Any:
        return tuple([await self.ev(e, env) for e in node.elts])

    async def ev_Set(self, node: ast.Set, env: dict[str, Any]) -> Any:
        return {await self.ev(e, env) for e in node.elts}

    async def ev_Dict(self, node: ast.Dict, env: dict[str, Any]) -> Any:
        out: dict[Any, Any] = {}
        for k, v in zip(node.keys, node.values, strict=True):
            if k is None:
                out.update(await self.ev(v, env))
            else:
                out[await self.ev(k, env)] = await self.ev(v, env)
        return out

    async def ev_JoinedStr(self, node: ast.JoinedStr, env: dict[str, Any]) -> Any:
        return "".join([str(await self.ev(v, env)) for v in node.values])

    async def ev_FormattedValue(self, node: ast.FormattedValue, env: dict[str, Any]) -> Any:
        value = await self.ev(node.value, env)
        if node.conversion == ord("r"):
            value = repr(value)
        elif node.conversion == ord("s"):
            value = str(value)
        spec = await self.ev(node.format_spec, env) if node.format_spec else ""
        return format(value, spec)

    async def ev_BinOp(self, node: ast.BinOp, env: dict[str, Any]) -> Any:
        return self.binop(node.op, await self.ev(node.left, env), await self.ev(node.right, env))

    async def ev_UnaryOp(self, node: ast.UnaryOp, env: dict[str, Any]) -> Any:
        v = await self.ev(node.operand, env)
        if isinstance(node.op, ast.Not):
            return not v
        if isinstance(node.op, ast.USub):
            return -v
        if isinstance(node.op, ast.UAdd):
            return +v
        raise CodeError(f"line {node.lineno}: unsupported operator")

    async def ev_BoolOp(self, node: ast.BoolOp, env: dict[str, Any]) -> Any:
        value: Any = None
        for operand in node.values:
            value = await self.ev(operand, env)
            if isinstance(node.op, ast.And) and not value:
                return value
            if isinstance(node.op, ast.Or) and value:
                return value
        return value

    async def ev_Compare(self, node: ast.Compare, env: dict[str, Any]) -> Any:
        left = await self.ev(node.left, env)
        for op, comparator in zip(node.ops, node.comparators, strict=True):
            right = await self.ev(comparator, env)
            fn = _CMPOPS.get(type(op))
            if fn is None or not fn(left, right):
                if fn is None:
                    raise CodeError(f"line {node.lineno}: unsupported comparison")
                return False
            left = right
        return True

    async def ev_IfExp(self, node: ast.IfExp, env: dict[str, Any]) -> Any:
        branch = node.body if await self.ev(node.test, env) else node.orelse
        return await self.ev(branch, env)

    async def ev_Subscript(self, node: ast.Subscript, env: dict[str, Any]) -> Any:
        return (await self.ev(node.value, env))[await self.ev(node.slice, env)]

    async def ev_Slice(self, node: ast.Slice, env: dict[str, Any]) -> Any:
        return slice(
            await self.ev(node.lower, env),
            await self.ev(node.upper, env),
            await self.ev(node.step, env),
        )

    async def ev_Attribute(self, node: ast.Attribute, env: dict[str, Any]) -> Any:
        if node.attr.startswith("_"):
            raise CodeError(f"line {node.lineno}: attributes starting with _ are not allowed")
        obj = await self.ev(node.value, env)
        for kind, allowed in METHODS.items():
            if (obj is kind or isinstance(obj, kind)) and node.attr in allowed:
                return getattr(obj, node.attr)
        raise AttributeError(f"{type(obj).__name__} has no allowed attribute {node.attr!r}")

    async def ev_Call(self, node: ast.Call, env: dict[str, Any]) -> Any:
        fn = await self.ev(node.func, env)
        args: list[Any] = []
        for a in node.args:
            if isinstance(a, ast.Starred):
                args.extend(await self.ev(a.value, env))
            else:
                args.append(await self.ev(a, env))
        kwargs: dict[str, Any] = {}
        for kw in node.keywords:
            if kw.arg is None:
                kwargs.update(await self.ev(kw.value, env))
            else:
                kwargs[kw.arg] = await self.ev(kw.value, env)
        if fn is range and sum(abs(int(x)) for x in args) > MAX_RANGE * 3:
            raise Limit(f"range is limited to {MAX_RANGE}")
        if fn is print:
            self.say(" ".join(str(a) for a in args) + "\n")
            return None
        if isinstance(fn, _Function):
            return await self.call_function(fn, args, kwargs, env)
        if isinstance(fn, _ToolCall):
            return await fn(*args, **kwargs)
        if isinstance(kwargs.get("key"), _Function):
            return await self.keyed(fn, args, kwargs)
        if not callable(fn):
            raise TypeError(f"{type(fn).__name__} is not callable")
        result = fn(*args, **kwargs)
        if isinstance(result, range) and len(result) > MAX_RANGE:
            raise Limit(f"range is limited to {MAX_RANGE}")
        return result

    async def keyed(self, fn: Any, args: list[Any], kwargs: dict[str, Any]) -> Any:
        """sorted, min, max and list.sort with a ``key`` the script defined: the
        keys are computed here (the function is not native) and used to order."""
        key = kwargs.pop("key")
        target = fn.__self__ if getattr(fn, "__name__", "") == "sort" else None
        items = list(target if target is not None else args[0])
        keys = [await self.call_function(key, [x], {}, {}) for x in items]
        order = sorted(range(len(items)), key=keys.__getitem__, reverse=bool(kwargs.get("reverse")))
        if fn is sorted:
            return [items[i] for i in order]
        if target is not None:
            target[:] = [items[i] for i in order]
            return None
        if fn in (min, max):
            if not items:
                raise ValueError(f"{fn.__name__}() of an empty sequence")
            pick = fn(range(len(items)), key=keys.__getitem__)
            return items[pick]
        raise TypeError("key= is for sorted, min, max and list.sort")

    async def call_function(
        self, fn: _Function, args: list[Any], kwargs: dict[str, Any], outer: dict[str, Any]
    ) -> Any:
        a = fn.args
        names = [p.arg for p in a.args]
        if len(args) > len(names):
            raise TypeError(f"{fn.name} takes {len(names)} arguments")
        local = dict(zip(names, args, strict=False))
        defaults = [None] * (len(names) - len(a.defaults)) + list(a.defaults)
        for name, default in zip(names, defaults, strict=True):
            if name in kwargs:
                local[name] = kwargs.pop(name)
            elif name not in local:
                if default is None:
                    raise TypeError(f"{fn.name} is missing {name!r}")
                local[name] = await self.ev(default, outer)
        if kwargs:
            raise TypeError(f"{fn.name} got unexpected {sorted(kwargs)}")
        self.depth += 1
        if self.depth > MAX_DEPTH:
            raise Limit("functions are nested too deeply")
        env = {**outer, **local}
        try:
            if isinstance(fn.body, list):
                try:
                    await self.block(fn.body, env)
                except _Return as r:
                    return r.value
                return None
            return await self.ev(fn.body, env)
        finally:
            self.depth -= 1

    async def ev_Lambda(self, node: ast.Lambda, env: dict[str, Any]) -> Any:
        return _Function("<lambda>", node.args, node.body)

    # --- comprehensions ---

    async def comprehension(
        self,
        generators: list[ast.comprehension],
        env: dict[str, Any],
        emit: Callable[[dict[str, Any]], Awaitable[None]],
    ) -> None:
        async def walk(i: int, scope: dict[str, Any]) -> None:
            if i == len(generators):
                await emit(scope)
                return
            gen = generators[i]
            for item in await self.ev(gen.iter, scope):
                self.tick(gen.iter)
                inner = dict(scope)
                await self.assign(gen.target, item, inner)
                if all([await self.ev(cond, inner) for cond in gen.ifs]):
                    await walk(i + 1, inner)

        await walk(0, dict(env))

    async def ev_ListComp(self, node: ast.ListComp, env: dict[str, Any]) -> Any:
        out: list[Any] = []

        async def emit(scope: dict[str, Any]) -> None:
            out.append(await self.ev(node.elt, scope))

        await self.comprehension(node.generators, env, emit)
        return out

    async def ev_GeneratorExp(self, node: ast.GeneratorExp, env: dict[str, Any]) -> Any:
        out: list[Any] = []

        async def emit(scope: dict[str, Any]) -> None:
            out.append(await self.ev(node.elt, scope))

        await self.comprehension(node.generators, env, emit)
        return out

    async def ev_SetComp(self, node: ast.SetComp, env: dict[str, Any]) -> Any:
        out: set[Any] = set()

        async def emit(scope: dict[str, Any]) -> None:
            out.add(await self.ev(node.elt, scope))

        await self.comprehension(node.generators, env, emit)
        return out

    async def ev_DictComp(self, node: ast.DictComp, env: dict[str, Any]) -> Any:
        out: dict[Any, Any] = {}

        async def emit(scope: dict[str, Any]) -> None:
            out[await self.ev(node.key, scope)] = await self.ev(node.value, scope)

        await self.comprehension(node.generators, env, emit)
        return out


class RunCodeTool:
    """``run_code``: the one tool of code mode."""

    def __init__(
        self, callables: dict[str, ToolFn], specs: dict[str, ToolSpec], *, only: bool = True
    ) -> None:
        self.callables = callables
        self.specs = specs
        listing = "\n".join(
            f"  {_signature(s)}: {_first_line(s.description)}" for s in specs.values()
        )
        self.spec = ToolSpec(
            name="run_code",
            description=(
                "Do the work by writing a short Python script that calls these tools as functions, "
                "instead of calling them one at a time. Loop, filter and combine their results "
                "in the script and print only what you need: the script's printed output is what "
                "you get back, so you do not pay for every intermediate result. Each tool returns "
                "its output as a string (starting with 'ERROR: ' if it failed).\n"
                f"Tools:\n{listing}\n"
                "The language is a safe subset of Python: variables, if/for/while/try, "
                "comprehensions, f-strings, def and lambda, the usual str/list/dict methods, "
                "json and re. There are no files, imports (other than json and re), classes or "
                "attributes starting with an underscore. Use print() for output."
            ),
            input_schema={
                "type": "object",
                "properties": {"code": {"type": "string", "description": "The script."}},
                "required": ["code"],
            },
        )

    async def call(self, args: dict[str, Any], *, span: str = "") -> ToolResult:
        code = args.get("code")
        if not isinstance(code, str) or not code.strip():
            return ToolResult("run_code needs a script in 'code'", is_error=True)
        interp = Interpreter(self.callables, self.specs)
        try:
            output = await interp.run(code)
        except CodeError as exc:
            partial = "".join(interp.out)
            body = f"{partial}\n" if partial else ""
            return ToolResult(
                _clip(f"{body}Error: {exc}"),
                is_error=True,
                metadata={"tool_calls": interp.calls, "operations": interp.ops},
            )
        except Limit as exc:
            partial = "".join(interp.out)
            body = f"{partial}\n" if partial else ""
            return ToolResult(
                _clip(f"{body}Stopped: {exc}"),
                is_error=True,
                metadata={"tool_calls": interp.calls, "operations": interp.ops},
            )
        return ToolResult(
            _clip(output or "(the script printed nothing)"),
            metadata={"tool_calls": interp.calls, "operations": interp.ops},
        )


def _first_line(text: str) -> str:
    return text.strip().splitlines()[0][:140] if text.strip() else ""


def _signature(spec: ToolSpec) -> str:
    props = spec.input_schema.get("properties") or {}
    required = set(spec.input_schema.get("required") or [])
    params = ", ".join(f"{name}" if name in required else f"{name}=None" for name in props)
    return f"{spec.name}({params})"
