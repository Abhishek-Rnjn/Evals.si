from __future__ import annotations

import importlib.util
from pathlib import Path


def load_preflight():
    spec = importlib.util.spec_from_file_location(
        "preflight", Path(__file__).with_name("preflight.py")
    )
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def test_regular_file_cannot_stand_in_for_kvm(tmp_path):
    device = tmp_path / "kvm"
    device.write_bytes(b"not a device")
    assert load_preflight().kvm_probe(str(device))["ready"] is False


def test_missing_device_is_blocked(tmp_path):
    assert load_preflight().kvm_probe(str(tmp_path / "absent"))["ready"] is False
