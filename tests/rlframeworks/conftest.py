"""Shared pieces for the verl and OpenRLHF tests: a byte-level tokenizer
built here (nothing downloaded, so decode gives back the text exactly) and
the reward spec."""

from __future__ import annotations

from pathlib import Path

import pytest

HERE = Path(__file__).parent


@pytest.fixture(autouse=True)
def reward_spec(monkeypatch: pytest.MonkeyPatch) -> str:
    spec = str(HERE / "reward.yaml")
    monkeypatch.setenv("EVALSI_REWARD_SPEC", spec)
    return spec


@pytest.fixture(scope="session")
def tokenizer():  # type: ignore[no-untyped-def]
    from tokenizers import Tokenizer, decoders, models, pre_tokenizers, trainers
    from transformers import PreTrainedTokenizerFast

    tok = Tokenizer(models.BPE(unk_token="<unk>"))
    tok.pre_tokenizer = pre_tokenizers.ByteLevel(add_prefix_space=False)
    tok.decoder = decoders.ByteLevel()
    tok.train_from_iterator(
        ["What is 2 + 2? 4 5 Paris"] * 10,
        trainers.BpeTrainer(
            vocab_size=280,
            special_tokens=["<unk>", "<pad>"],
            initial_alphabet=pre_tokenizers.ByteLevel.alphabet(),
        ),
    )
    return PreTrainedTokenizerFast(tokenizer_object=tok, unk_token="<unk>", pad_token="<pad>")
