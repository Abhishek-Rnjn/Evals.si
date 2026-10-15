"""An agent's own setup (AGENTS.md, skills, MCP config) as part of the task's environment (H3)."""

from __future__ import annotations

from pathlib import Path

from evalsi_harness.task import EnvironmentConfig
from evalsi_harness.testing import LocalSandboxClient
from tests.harness_support import record, run, spec_of

FIXTURES = Path(__file__).parent / "fixtures"

SETUP = {
    "AGENTS.md": "Run the tests before you finish.",
    ".claude/skills/review/SKILL.md": "---\nname: review\n---\nReview the diff.",
    ".mcp.json": '{"mcpServers": {}}',
}


def test_files_in_the_environment_are_there_for_a_cli_agent_and_are_its_setup(
    sandboxes: LocalSandboxClient,
) -> None:
    # The agent here is a script that reports what it found, in Claude's format.
    script = (
        "printf '%s\\n' "
        '\'{"type":"result","subtype":"success","result":"seen:\'"$(cat AGENTS.md)"\'"}\''
    )
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "cli": {"command": ["sh", "-c", script], "output_format": "claude-stream-json"}
                }
            },
            "environment": {"files": SETUP, "checker": {"command": ["test", "-f", ".mcp.json"]}},
        }
    )
    rec = run(spec, record("go"), sandboxes).record
    assert rec is not None
    assert rec.output is not None
    assert rec.output.as_text() == "seen:Run the tests before you finish."
    assert rec.check is not None
    assert rec.check.passed  # nested and dot files were written, and the checker sees them


def test_a_different_agent_setup_is_a_different_environment() -> None:
    """The snapshot key covers the files, so two setups never share one, and a run
    that changes only AGENTS.md compares as a different environment."""
    a = EnvironmentConfig(files=dict(SETUP))
    b = EnvironmentConfig(files={**SETUP, "AGENTS.md": "Do not run the tests."})
    assert a.key() != b.key()
    assert a.key() == EnvironmentConfig(files=dict(SETUP)).key()


def test_a_wrong_output_format_fails_the_trial_and_the_checker_never_grades_it(
    sandboxes: LocalSandboxClient,
) -> None:
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "cli": {
                        "command": ["sh", "-c", "echo plain text"],
                        "output_format": "codex-json",
                    }
                }
            },
            "environment": {"checker": {"command": ["true"]}},  # would pass if it ran
        }
    )
    out = run(spec, record("go"), sandboxes)
    assert "no codex-json events" in out.error
    assert out.record is None or out.record.check is None or not out.record.check.passed


def test_the_cost_a_cli_reports_is_kept_in_the_runs_usage(sandboxes: LocalSandboxClient) -> None:
    fixture = FIXTURES / "claude-code-0.3.300-fix-calc.jsonl"
    spec = spec_of(
        {
            "target": {
                "agent": {
                    "cli": {
                        "command": ["sh", "-c", f"cat {fixture}"],
                        "output_format": "claude-stream-json",
                    }
                }
            },
            "environment": {"checker": {"command": ["true"]}},
        }
    )
    rec = run(spec, record("go"), sandboxes).record
    assert rec is not None
    assert rec.usage is not None
    assert rec.usage.cost_usd == 0.0123
