"""evalsi-harness: the light built-in agent harness for Evals.si (design §11).

It drives an agent through a task in an Evals.si sandbox: the built-in
tool-calling agent on any OpenAI-compatible or Anthropic model, or a
bring-your-own agent (A2A, MCP, OpenAI Responses, HTTP, or a CLI run in the
sandbox). It provides sandbox and MCP tools, budgets, a simulated user,
mocked and faulty tools, record and replay, sandbox policy events and OTel
spans, and grades the end state with the environment's checker.

Most users never import it: ``evalsi run`` and the server's worker call
:func:`run_task` for every record of an agent run.
"""

from evalsi_harness._version import __version__
from evalsi_harness.events import FinalEvent, PolicyEvent, StepEvent
from evalsi_harness.harness import BuiltinHarness, Harness, HarnessContext, HarnessManifest
from evalsi_harness.runner import TaskOutcome, load_harness, run_task
from evalsi_harness.task import EnvironmentConfig, Task, TaskError
from evalsi_harness.tools import FunctionTool, Tool, ToolResult

__all__ = [
    "BuiltinHarness",
    "EnvironmentConfig",
    "FinalEvent",
    "FunctionTool",
    "Harness",
    "HarnessContext",
    "HarnessManifest",
    "PolicyEvent",
    "StepEvent",
    "Task",
    "TaskError",
    "TaskOutcome",
    "Tool",
    "ToolResult",
    "__version__",
    "load_harness",
    "run_task",
]
