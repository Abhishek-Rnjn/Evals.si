"""Task environments in other environments' specs.

:mod:`evalsi_harness.gym` turns the tasks agent runs evaluate into
``reset``/``step`` environments. These adapters present the same environments
as an `OpenEnv <https://github.com/meta-pytorch/OpenEnv>`_ ``Environment``
(:mod:`evalsi_harness.envspecs.openenv`) and as a `verifiers
<https://github.com/PrimeIntellect-ai/verifiers>`_ ``StatefulToolEnv``
(:mod:`evalsi_harness.envspecs.verifiers`), so one task definition serves
evaluation here and training there. Each needs its package
(``pip install openenv-core==0.3.0`` or ``verifiers==0.3.1``, the versions
they are tested against) and is
imported only when asked for.
"""
