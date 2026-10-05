# evalsi

The Python SDK and CLI for [Evals.si](https://github.com/abhishek-rnjn/evals.si): one entrypoint for evaluating ML models, LLMs and agents.

```bash
pip install evalsi                 # core
pip install 'evalsi[anthropic]'    # Claude as a judge
pip install 'evalsi[hf]'           # hf:// datasets
```

```bash
evalsi eval --data qa.jsonl --evaluators exact-match,numeric-match
evalsi catalog
```

```python
import evalsi

result = evalsi.evaluate(
    "qa.jsonl",
    ["exact-match", "llm-judge"],
    judge=evalsi.JudgeConfig(provider="anthropic", model="claude-opus-5-5"),
)
print(result.table())
```

See the repository README for the full guide.
