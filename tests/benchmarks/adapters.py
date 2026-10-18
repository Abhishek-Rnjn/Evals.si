"""Public SDK entry points for the deterministic evaluation benchmark.

Imports stay inside factories so each tool can use its own locked environment.
No model calls or hosted uploads are part of these local measurements.
"""

from __future__ import annotations


def batch_runner(tool: str, concurrency: int):
    if tool == "evalsi":
        import evalsi

        def run(rows):
            result = evalsi.evaluate(
                rows,
                ["exact-match"],
                params={"exact-match": {"normalize": False}},
                concurrency=concurrency,
                cache=False,
            )
            if result.errors() or any(
                r.outcome != evalsi.Outcome.SCORED for r in result.results
            ):
                raise ValueError("Evals.si returned errors/skips")
            return [(r.record_id, float(r.scores[0].passed)) for r in result.results]

    elif tool == "deepeval":
        from deepeval import evaluate
        from deepeval.evaluate import (
            AsyncConfig,
            CacheConfig,
            DisplayConfig,
            ErrorConfig,
        )
        from deepeval.metrics import ExactMatchMetric
        from deepeval.test_case import LLMTestCase

        def run(rows):
            cases = [
                LLMTestCase(
                    input=r["id"],
                    actual_output=r["output"],
                    expected_output=r["reference"],
                )
                for r in rows
            ]
            result = evaluate(
                cases,
                [ExactMatchMetric()],
                async_config=AsyncConfig(run_async=False, max_concurrent=concurrency),
                cache_config=CacheConfig(write_cache=False, use_cache=False),
                error_config=ErrorConfig(
                    ignore_errors=False, skip_on_missing_params=False
                ),
                display_config=DisplayConfig(
                    show_indicator=False, print_results=False, inspect_after_run=False
                ),
            )
            scores = []
            for r in result.test_results:
                if len(r.metrics_data or []) != 1 or r.metrics_data[0].error:
                    raise ValueError("DeepEval returned missing/error metrics")
                scores.append((r.input, float(r.metrics_data[0].score)))
            return scores

    elif tool == "ragas":
        from ragas import EvaluationDataset, evaluate
        from ragas.metrics import ExactMatch
        from ragas.run_config import RunConfig

        def run(rows):
            dataset = EvaluationDataset.from_list(
                [
                    {
                        "user_input": r["id"],
                        "response": r["output"],
                        "reference": r["reference"],
                    }
                    for r in rows
                ]
            )
            result = evaluate(
                dataset,
                metrics=[ExactMatch()],
                show_progress=False,
                raise_exceptions=True,
                run_config=RunConfig(max_workers=concurrency),
                allow_nest_asyncio=False,
            )
            return [
                (row["id"], float(score["exact_match"]))
                for row, score in zip(rows, result.scores, strict=True)
            ]

    elif tool == "langsmith":
        from uuid import NAMESPACE_URL, uuid5

        from langsmith import Client
        from langsmith.evaluation import evaluate
        from langsmith.schemas import Example

        # Offline capability metadata avoids the otherwise automatic /info
        # discovery request. Actual evaluation still uses the public runner.
        client = Client(auto_batch_tracing=False, info={})

        def exact_match(outputs, reference_outputs):
            return {
                "key": "exact_match",
                "score": float(outputs["output"] == reference_outputs["output"]),
            }

        def run(rows):
            dataset = [
                Example(
                    id=uuid5(NAMESPACE_URL, r["id"]),
                    inputs={"id": r["id"], "output": r["output"]},
                    outputs={"output": r["reference"]},
                )
                for r in rows
            ]
            result = evaluate(
                lambda inputs: {"output": inputs["output"]},
                data=dataset,
                evaluators=[exact_match],
                max_concurrency=0 if concurrency == 1 else concurrency,
                upload_results=False,
                client=client,
            )
            return [
                (
                    r["example"].inputs["id"],
                    float(r["evaluation_results"]["results"][0].score),
                )
                for r in result
            ]

    elif tool == "braintrust":
        from braintrust import Eval

        def exact_match(output, expected):
            return float(output == expected)

        def run(rows):
            data = [
                {
                    "input": {"id": r["id"], "output": r["output"]},
                    "expected": r["reference"],
                }
                for r in rows
            ]
            result = Eval(
                "evalsi-local-benchmark",
                data=data,
                task=lambda input: input["output"],
                scores=[exact_match],
                max_concurrency=concurrency,
                no_send_logs=True,
                enable_cache=False,
            )
            if any(r.error for r in result.results):
                raise ValueError("Braintrust returned evaluation errors")
            return [
                (r.input["id"], float(r.scores["exact_match"])) for r in result.results
            ]

    else:
        raise ValueError(f"unsupported batch tool: {tool}")
    return run


def metric_runner(tool: str, rows: list[dict[str, str]]):
    """Prepare native inputs outside the timed, serial metric loop."""
    if tool == "evalsi":
        from evalsi import Content, Record
        from evalsi.packs.core import exact_match

        inputs = [
            Record(
                id=r["id"],
                output=Content(text=r["output"]),
                reference=Content(text=r["reference"]),
            )
            for r in rows
        ]

        def run():
            return [
                (r.id, float(exact_match.fn(r, normalize=False).passed)) for r in inputs
            ]

    elif tool == "deepeval":
        from deepeval.metrics import ExactMatchMetric
        from deepeval.test_case import LLMTestCase

        inputs = [
            LLMTestCase(
                input=r["id"], actual_output=r["output"], expected_output=r["reference"]
            )
            for r in rows
        ]
        metric = ExactMatchMetric()

        def run():
            return [
                (r.input, float(metric.measure(r, _show_indicator=False)))
                for r in inputs
            ]

    elif tool == "ragas":
        from ragas.metrics.collections import ExactMatch

        metric = ExactMatch()

        def run():
            return [
                (
                    r["id"],
                    float(
                        metric.score(
                            response=r["output"], reference=r["reference"]
                        ).value
                    ),
                )
                for r in rows
            ]

    else:
        raise ValueError(f"unsupported metric tool: {tool}")
    return run
