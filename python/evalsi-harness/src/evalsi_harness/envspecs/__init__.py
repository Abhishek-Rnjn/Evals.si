"""Task environments in other environments' specs.

:mod:`evalsi_harness.gym` turns the tasks agent runs evaluate into
``reset``/``step`` environments. These adapters present the same environments
as an `OpenEnv <https://github.com/meta-pytorch/OpenEnv>`_ ``Environment``
(:mod:`evalsi_harness.envspecs.openenv`) and as a `verifiers
<https://github.com/PrimeIntellect-ai/verifiers>`_ ``StatefulToolEnv``
(:mod:`evalsi_harness.envspecs.verifiers`), as a `Gymnasium
<https://gymnasium.farama.org>`_ ``Env`` (:mod:`evalsi_harness.envspecs.gymnasium`)
and as a `NeMo Gym <https://github.com/NVIDIA-NeMo/Gym>`_ resources server
(:mod:`evalsi_harness.envspecs.nemogym`), so one task definition serves
evaluation here and training there. Each needs its package
(``pip install openenv-core==0.3.0``, ``verifiers==0.3.1``, ``gymnasium==1.4.0``
or ``nemo-gym==0.7.0``, which needs Python 3.13.14 or newer; the versions they
are tested against) and is imported only when asked for.
"""
