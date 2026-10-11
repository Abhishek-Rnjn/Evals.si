# Literature study for the Evals.si paper

Status: first pass, 2026-10-11. Every entry was found by web search and checked against the search summary or abstract only. **No full text was read.** Before citing a claim, open the paper and confirm it. Entries marked (?) were not confirmed from a primary source.

## 1. Evaluation harnesses and frameworks (closest relatives)

| Work | What it is | Relation to Evals.si |
|------|-----------|----------------------|
| HELM, Liang et al., [arXiv 2211.09110](https://arxiv.org/abs/2211.09110) (TMLR 2023) | Taxonomy of scenarios and 7 metrics; 30 models on 42 scenarios; raw prompts and completions released | Offline, benchmark-centric. No online or training mode, no sandbox story |
| lm-evaluation-harness, Biderman et al., [arXiv 2405.14782](https://arxiv.org/abs/2405.14782) | Reproducibility lessons and the harness | Evals.si wraps it as an isolated adapter. Cite for the sensitivity-to-setup argument |
| Inspect AI, UK AISI (no paper found; [docs](https://inspect.aisi.org.uk)) | Solvers and scorers, agent evals, sandboxing in Docker, Kubernetes and others | **Closest competitor** on agent evals and sandboxes. Cite the docs and repo; check for a paper or tech report before submission |
| RAGAS, Es et al., [arXiv 2309.15217](https://arxiv.org/abs/2309.15217) | Reference-free RAG metrics | Adapter in Evals.si |
| OpenAI Evals, DeepEval, OpenCompass, MLflow Evaluate | Tool and registry projects | No academic papers found. Cite repos or docs, and compare features in a table |

## 2. Evaluation statistics

| Work | Point | Use in the paper |
|------|-------|------------------|
| Miller, [arXiv 2411.00640](https://arxiv.org/abs/2411.00640), "Adding Error Bars to Evals" | Treat questions as a sample from a super-population; clustered standard errors; paired comparison; power analysis | Basis for the CI and `--cluster-by` design. State plainly which formulas Evals.si implements |
| Bowyer, Aitchison, Ivanova, [arXiv 2503.01747](https://arxiv.org/abs/2503.01747) (ICML 2025) | CLT intervals understate uncertainty below a few hundred items | **A weakness to address.** Check which interval Evals.si uses at small n (the sample output shows a Wilson-like interval for exact-match; verify in code) |
| Skewed Score, [arXiv 2507.03772](https://arxiv.org/abs/2507.03772) (?) | Statistical framework for assessing autograders | Candidate for judge-error propagation. Read before citing |
| Measuring all the noises of LLM Evals, [arXiv 2512.21326](https://arxiv.org/abs/2512.21326) (?) | Noise decomposition | Candidate for the trials design. Read before citing |

Gap Evals.si can claim, once verified: the cited work gives formulas. None of it is a platform contract that makes skipped records and evaluator errors first-class (skip is not zero, error is not zero). Check this claim against Inspect and lm-eval behaviour first.

## 3. LLM-as-judge

- Zheng et al., [arXiv 2306.05685](https://arxiv.org/abs/2306.05685), MT-Bench and Chatbot Arena: over 80% agreement with humans, plus position, verbosity and self-enhancement bias. Motivates "no default judge" and the need to calibrate any judge.
- Online-eval practice (Langfuse, MLflow, LangSmith docs) is vendor documentation only. No peer-reviewed paper on online evaluation of production agent traces turned up. **This looks like a real gap**, but run a targeted arXiv search before claiming novelty.

## 4. Agent benchmarks and agent-evaluation infrastructure

- Survey: Yehudai et al., [arXiv 2503.16416](https://arxiv.org/abs/2503.16416). It names gaps in cost-efficiency, safety, robustness and scalable evaluation. Use it to frame the problem.
- SWE-bench, Jimenez et al., [arXiv 2310.06770](https://arxiv.org/abs/2310.06770) (ICLR 2024). One secondary source says SWE-bench Verified was deprecated in 2025 in favour of SWE-bench Pro (?). Verify, because the repo's docs refer to SWE-bench Verified.
- τ-bench, Yao et al., [arXiv 2406.12045](https://arxiv.org/abs/2406.12045): database-state scoring and the pass^k reliability metric. Related follow-up τ²-bench, arXiv 2506.07982 (?).
- Terminal-Bench, [arXiv 2601.11868](https://arxiv.org/abs/2601.11868): 89 command-line tasks. TUA-Bench, arXiv 2606.28480 (?).
- BFCL, Patil et al., [PMLR 267](https://proceedings.mlr.press/v267/patil25a.html) (ICML 2025): AST-based function-call scoring.
- HAL, Kapoor et al., [arXiv 2510.11977](https://arxiv.org/abs/2510.11977) (ICLR 2026): a standardised parallel harness over many VMs, with 21,730 rollouts costing about $40k. **Closest infrastructure paper for agents.** It stresses harness errors and cost. Compare directly.

## 5. Sandboxing and isolation

- Firecracker, Agache et al., [NSDI 2020](https://www.usenix.org/conference/nsdi20/presentation/agache): the microVM rung.
- Isolation comparison: [arXiv 2110.11462](https://arxiv.org/abs/2110.11462), "A Fresh Look at the Architecture and Performance of Contemporary Isolation Platforms" (gVisor, Kata, Firecracker). Also a MASCOTS 2020 study of runc, Kata and gVisor on Kubernetes, which found runc up to 5x faster (title and authors not confirmed). Use these to ground the isolation ladder and its overhead numbers.
- Landlock: only 2016 kernel RFCs and landlock.io found. Cite landlock.io and the kernel docs. **No paper found** on bubblewrap or on sandboxing LLM-generated code, so run a targeted search.

## 6. Sandboxes for agentic RL (the Reward mode)

- GRPO, DeepSeekMath, [arXiv 2402.03300](https://arxiv.org/abs/2402.03300): the training method used in the TRL demo.
- ProRL Agent, [arXiv 2603.18815](https://arxiv.org/abs/2603.18815): rollout-as-a-service, rootless HPC sandboxes.
- DSec (DeepSeek), [arXiv 2609.22978](https://arxiv.org/abs/2609.22978): about 160 nodes, about 3M sandboxes a day, reward-hacking mitigation. A strong comparison for the sandbox claims.
- ROCK, [arXiv 2512.24873](https://arxiv.org/abs/2512.24873) (ROME paper) and SWE-MiniSandbox, [arXiv 2602.11210](https://arxiv.org/abs/2602.11210) (?).
- Not searched: RLVR and Tulu 3, the TRL, verl and OpenRLHF reward interfaces, SkyRL, rLLM, and the reward-hacking literature.

## 7. Trace and observability standards

OpenTelemetry GenAI semantic conventions, OpenInference (Phoenix), Langfuse's OTel basis and MLflow tracing are all documented as specs and docs, with no papers. Cite the specs by version. Phoenix added OTel-GenAI to OpenInference conversion in May 2026 (release note, 15.10.0+), which matters for the "push" mode.

## Positioning

| Capability | HELM | lm-eval | Inspect | HAL | Langfuse / Phoenix | Evals.si |
|---|---|---|---|---|---|---|
| Offline benchmarks | yes | yes | yes | yes | partial | yes |
| Agent tasks | no | no | yes | yes | no | yes |
| Online scoring of live traces | no | no | no | no | yes | yes |
| RL reward from the same evaluators | no | no | no | no | no | yes |
| Tiered fail-closed sandbox | no | no | docker/k8s | VMs | no | yes |
| CIs, skip and error semantics | partial | partial | partial | ? | ? | yes |

Fill every "?" and every "partial" by reading each project's code or docs. The table is a hypothesis, not a result.

## Open searches still to run

1. Evaluation frameworks and platforms in the systems literature (MLSys, OSDI, SoCC), and the MLOps model-monitoring literature.
2. Online and continuous evaluation of LLM or agent systems, drift, sampling and cost control.
3. Sandbox escape and security analyses for agent code execution. Prompt injection into evaluators and judges.
4. Reward hacking and evaluator robustness when evaluators double as rewards.
5. Test contamination and benchmark validity (SWE-bench Verified status), plus the ABC-style benchmark checklists.
6. Inspect AI, Langfuse, Phoenix and MLflow: check each project's GitHub for an accompanying paper or tech report.
7. Kubernetes operators for ML evaluation (Kubeflow, Argo) as related systems work.

## Reading list, in priority order

HAL, Inspect docs, DSec, ProRL Agent, Miller, Bowyer et al., Zheng et al., the isolation-platform comparison, then τ-bench and Terminal-Bench.
