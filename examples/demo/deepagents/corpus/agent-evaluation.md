# Evaluating agents

An agent is evaluated on its end state and on how it got there. A checker grades the final state of the environment (do the tests pass?). Trajectory evaluators then look at the steps: tool errors, loops, wasted steps and policy violations. Because agents are stochastic, each task is run several times; pass@k says the agent can solve it, pass^k says it solves it reliably.
