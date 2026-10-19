Synthetic Harness2 run fixture, written from the Harness2 source code
(core/schedule.py, core/judge.py, core/selection.py, cli/judge.py), not from a
recorded run. Task ids, scores and harness text are invented; there is no real
JobBench, LAB or WorkBuddy data here. The layout is unconfirmed against a real run.

ns/jb-oc-agg2: tasks alpha/one and beta (current keys: modes, mode) and gamma
(legacy keys: arms, arm). Reference executions base1, base2. Modes agg and seq at k=2.
beta's seq choice has no verdict (judge returned nothing); beta base2 was not judged.
Trees for the seq mode (labels c1, c2) are in the sibling ns/jb-oc-seq2, as Harness2 writes
each mode's harnesses under that mode's own namespace (summary.json stays in the agg one).
